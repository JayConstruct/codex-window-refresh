import argparse
import contextlib
import copy
import hashlib
import importlib.util
import io
import json
import os
import pathlib
import tempfile
import unittest
from unittest import mock

import yaml

ROOT = pathlib.Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("hot_installer", ROOT / "scripts/install.py")
installer = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(installer)


def runtime_report(plan, version, library):
    raw = pathlib.Path("/proc/self/stat").read_text()
    return {"hot_reload_protocol":2, "version":version, "phase":"running", "instance_id":"a" * 32,
            "pid":os.getpid(), "process_start":raw[raw.rindex(")")+1:].split()[19],
            "boot_id":pathlib.Path("/proc/sys/kernel/random/boot_id").read_text().strip(),
            "library_path":str(library), "config_file":str(plan["policy"]), "state_dir":str(plan["state"]),
            "reload_token":"", "updated_at":"2026-10-15T00:00:00Z"}


class HotInstallTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = pathlib.Path(self.temp.name)
        self.plugin_dir = self.root / "plugins"
        self.plugin_dir.mkdir()
        self.policy = self.root / "policy.yaml"
        self.policy.write_text("enabled: false\n")
        self.wrapper = {"enabled": True, "priority": 0, "quota_transport": "host",
                        "config_file": str(self.policy), "state_dir": str(self.root / "state")}
        self.config = self.root / "config.yaml"
        self.config.write_text(yaml.safe_dump({"port": 8317, "api-keys": ["fixture-private"],
            "plugins": {"enabled": True, "dir": str(self.plugin_dir),
                        "configs": {installer.PLUGIN: self.wrapper, "another": {"enabled": False}}}}))
        self.original = self.config.read_bytes()
        self.record_dir = self.root / "record"
        self.record_dir.mkdir(mode=0o700)
        self.lib = self.plugin_dir / (installer.PLUGIN + "-v0.1.0-rc.21.so")
        self.lib.write_bytes(b"original-library")
        self.checker = self.root / "window-refresh"
        self.checker.write_bytes(b"original-checker")
        self.record_file = self.record_dir / "installed.json"
        self.record = {"config": str(self.config), "version": "v0.1.0-rc.21", "library": str(self.lib),
                       "checker": str(self.checker), "dropin": None, "installed_hashes": {
            str(path): hashlib.sha256(path.read_bytes()).hexdigest() for path in [self.lib, self.checker]}}
        self.record_file.write_text(json.dumps(self.record))
        self.record_file.chmod(0o600)
        self.plan = {"directory": self.root, "plugin_dir": self.plugin_dir, "config": self.config,
                     "text": self.original.decode(), "policy": self.policy, "wrapper": self.wrapper,
                     "record": self.record_dir, "quota_transport": "host", "url": "http://127.0.0.1:8317/v0/management"}
        self.args = argparse.Namespace(version="v0.1.0-rc.22", restart=False, uninstall=False,
                                       check_model=False, yes=True)
        self.unpacked = self.root / "package"
        self.unpacked.mkdir()
        (self.unpacked / "BUILD-INFO.json").write_text('{"hot_reload_protocol":2}')
        (self.unpacked / "codex-window-refresh.so").write_bytes(b"new-library")
        (self.unpacked / "window-refresh").write_bytes(b"new-checker")
        self.target = self.plugin_dir / (installer.PLUGIN + "-v0.1.0-rc.22.so")
        self.calls = []
        self.mode = "success"
        self.active = "0.1.0-rc.21"
        self.instance = "a" * 32
        self.new_instance = True
        self.first_target_token = None
        mock.patch.object(installer, "management_credentials", side_effect=AssertionError("hot reload must not read a key")).start()
        mock.patch.object(installer.urllib.request, "build_opener", side_effect=AssertionError("hot reload must not use management HTTP")).start()
        mock.patch.object(installer, "private_file", side_effect=pathlib.Path).start()
        mock.patch.object(installer.os, "fchown", return_value=None).start()
        directory = installer.directory
        mock.patch.object(installer, "directory", side_effect=lambda p, m, u, g, strict=False:
                          directory(p, m, os.geteuid(), os.getegid(), strict)).start()
        mock.patch.object(installer, "run", return_value="valid").start()
        mock.patch.object(installer, "service_run", side_effect=AssertionError("must never touch systemd")).start()
        self.state = self.root / "state"
        self.state.mkdir(mode=0o700)
        self.version_file = self.state / "runtime-version.json"
        self.plan.update(state=self.state, uid=os.geteuid(), process_pid=os.getpid(),
                         binary=pathlib.Path("/proc/self/exe").resolve(), plugins={"enabled":True})
        self.report = runtime_report(self.plan, "0.1.0-rc.21", self.lib)
        self.publish()
        self.actual_read = installer.local_runtime
        mock.patch.object(installer, "local_runtime", side_effect=self.get).start()
        wait = installer.hot_wait
        mock.patch.object(installer, "hot_wait", side_effect=lambda p, d: wait(p, d, timeout=0)).start()
        self.addCleanup(mock.patch.stopall)

    def publish(self):
        self.version_file.write_text(json.dumps(self.report))
        self.version_file.chmod(0o600)

    def get(self, plan):
        self.calls.append("local-read")
        wrapper = yaml.safe_load(self.config.read_text())["plugins"]["configs"][installer.PLUGIN]
        store = wrapper.get("store", {})
        selected = store.get("version", "0.1.0-rc.21")
        token = store.get("local-reload-token", "")
        if self.target.exists() and self.mode == "unreachable-rollback":
            raise installer.LocalVersionError("unreachable")
        first_ack_only = self.mode == "first-ack-only" and self.target.exists() and selected == "0.1.0-rc.22"
        if first_ack_only and self.first_target_token is not None and self.first_target_token != token:
            return self.actual_read(plan)
        if self.target.exists() and selected == "0.1.0-rc.22" and self.mode in {"success", "first-ack-only"}:
            self.active = selected
            self.instance = "b" * 32 if self.new_instance else "a" * 32
            self.first_target_token = token
        elif selected == "0.1.0-rc.21":
            self.active = selected
        if self.mode != "ignore-config":
            self.report.update(version=self.active, instance_id=self.instance,
                               library_path=str(self.target if self.active == "0.1.0-rc.22" else self.lib),
                               reload_token=token)
            self.publish()
        return self.actual_read(plan)

    def install(self):
        with contextlib.redirect_stdout(io.StringIO()) as output:
            installer.hot_install(self.args, self.plan, self.unpacked)
        self.assertNotIn("fixture-key", output.getvalue())
        self.assertNotIn("fixture-private", output.getvalue())
        return output.getvalue()

    def test_versioned_handoff_confirms_new_instance_and_preserves_policy(self):
        self.assertIn("热重载成功", self.install())
        self.assertEqual(self.lib.read_bytes(), b"original-library")
        self.assertEqual(self.target.read_bytes(), b"new-library")
        self.assertEqual(self.policy.read_text(), "enabled: false\n")
        self.assertEqual(self.checker.read_bytes(), b"new-checker")
        config = yaml.safe_load(self.config.read_text())
        self.assertEqual(config["api-keys"], ["fixture-private"])
        self.assertEqual(config["plugins"]["configs"]["another"], {"enabled": False})
        self.assertEqual(config["plugins"]["configs"][installer.PLUGIN]["store"]["version"], "0.1.0-rc.22")
        record = json.loads(self.record_file.read_text())
        self.assertEqual(record["library"], str(self.target))
        self.assertEqual(set(installer.managed_libraries(record, self.plan)), {self.lib, self.target})
        self.assertFalse((self.root / "window-refresh.env").exists())

    def test_failed_activation_restores_old_version_and_reserves_attempted_path(self):
        self.mode = "failed-load"
        with self.assertRaisesRegex(installer.InstallError, "旧版已恢复"):
            self.install()
        self.assertEqual(self.checker.read_bytes(), b"original-checker")
        self.assertEqual(self.target.read_bytes(), b"new-library")
        wrapper = yaml.safe_load(self.config.read_text())["plugins"]["configs"][installer.PLUGIN]
        self.assertEqual(wrapper["store"]["version"], "0.1.0-rc.21")
        self.assertEqual({k: v for k, v in wrapper.items() if k != "store"}, self.wrapper)
        record = json.loads(self.record_file.read_text())
        self.assertEqual(record["version"], "v0.1.0-rc.21")
        self.assertEqual(set(installer.managed_libraries(record, self.plan)), {self.lib, self.target})

    def test_unknown_rollback_preserves_files_and_backup_without_false_success(self):
        self.mode = "unreachable-rollback"
        with self.assertRaisesRegex(installer.InstallError, "无法确认旧版恢复"):
            self.install()
        self.assertTrue(self.target.is_file())
        self.assertEqual(self.checker.read_bytes(), b"new-checker")
        self.assertTrue(list(self.record_dir.glob("*/index.json")))

    def test_same_instance_cannot_count_as_successful_activation(self):
        self.new_instance = False
        with self.assertRaisesRegex(installer.InstallError, "无法确认旧版恢复"):
            self.install()

    def test_legacy_plugin_fails_before_mutation_with_bootstrap_instruction(self):
        with mock.patch.object(installer, "local_runtime", side_effect=installer.LocalVersionError("旧版本须普通升级并重启 CPA 一次")):
            with self.assertRaisesRegex(installer.InstallError, "重启 CPA 一次"):
                self.install()
        self.assertEqual(self.config.read_bytes(), self.original)
        self.assertFalse(self.target.exists())

    def test_rejects_existing_library_and_old_package_without_overwrite(self):
        self.target.write_bytes(b"must-not-overwrite")
        with self.assertRaisesRegex(installer.InstallError, "目标版本文件已存在"):
            self.install()
        self.assertEqual(self.target.read_bytes(), b"must-not-overwrite")
        (self.unpacked / "BUILD-INFO.json").write_text('{}')
        with self.assertRaisesRegex(installer.InstallError, "目标安装包不支持"):
            self.install()
        self.assertEqual(self.config.read_bytes(), self.original)

    def test_does_not_overwrite_concurrent_config_change(self):
        self.config.write_bytes(self.original + b"# external change\n")
        with self.assertRaisesRegex(installer.InstallError, "无法确认旧版恢复"):
            self.install()
        self.assertTrue(self.config.read_bytes().endswith(b"# external change\n"))
        self.assertFalse(self.target.exists())

    def test_hot_reload_cannot_change_transport_or_restart(self):
        self.args.restart = True
        with self.assertRaisesRegex(installer.InstallError, "不能与"):
            self.install()
        self.args.restart = False
        self.plan["quota_transport"] = "management"
        with self.assertRaisesRegex(installer.InstallError, "不能修改额度查询"):
            self.install()
        self.assertEqual(self.config.read_bytes(), self.original)

    def test_existing_running_file_cannot_replace_live_configuration_acknowledgement(self):
        self.mode = "ignore-config"
        with self.assertRaisesRegex(installer.InstallError, "无法确认旧版恢复"):
            self.install()
        self.assertFalse(self.target.exists())
        self.assertEqual(self.checker.read_bytes(), b"original-checker")

    def test_new_instance_must_acknowledge_a_second_configuration_round_trip(self):
        self.mode = "first-ack-only"
        with self.assertRaisesRegex(installer.InstallError, "无法确认旧版恢复"):
            self.install()
        self.assertTrue(self.target.is_file())
        self.assertEqual(self.report["version"], "0.1.0-rc.22")

    def test_missing_or_unverified_process_is_rejected_without_changes(self):
        self.plan["process_pid"] = None
        with self.assertRaisesRegex(installer.InstallError, "核实正在运行"):
            self.install()
        self.assertEqual(self.config.read_bytes(), self.original)

    def test_local_receipt_rejects_stale_or_mismatched_process_and_installation(self):
        original = copy.deepcopy(self.report)
        for key, value in [("pid", os.getpid() + 100000), ("process_start", "0"), ("boot_id", "old-boot"),
                           ("config_file", "/other-policy"), ("state_dir", "/other-state"),
                           ("library_path", "/outside/plugins/plugin.so"), ("hot_reload_protocol", 1),
                           ("instance_id", "bad"), ("reload_token", "bad"), ("version", "invalid")]:
            with self.subTest(key=key):
                self.report = dict(original, **{key:value})
                self.publish()
                with self.assertRaises(installer.LocalVersionError):
                    self.actual_read(self.plan)
        self.report = original
        self.publish()
        plan = dict(self.plan, binary=self.root / "other-cpa")
        with self.assertRaises(installer.LocalVersionError):
            self.actual_read(plan)

    def test_local_receipt_rejects_symlink_fifo_bad_permissions_oversize_and_bad_json(self):
        self.version_file.chmod(0o644)
        with self.assertRaises(installer.LocalVersionError): self.actual_read(self.plan)
        self.version_file.unlink()
        self.version_file.symlink_to(self.policy)
        with self.assertRaises(installer.LocalVersionError): self.actual_read(self.plan)
        self.version_file.unlink()
        os.mkfifo(self.version_file, 0o600)
        with self.assertRaises(installer.LocalVersionError): self.actual_read(self.plan)
        self.version_file.unlink()
        for data in ['x' * 16385, '{"fixture-private-secret"', '[]']:
            self.version_file.write_text(data)
            self.version_file.chmod(0o600)
            with self.assertRaises(installer.LocalVersionError) as error: self.actual_read(self.plan)
            self.assertNotIn("fixture-private-secret", str(error.exception))
