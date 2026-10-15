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
import urllib.error
from unittest import mock

import yaml

ROOT = pathlib.Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("hot_installer", ROOT / "scripts/install.py")
installer = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(installer)


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
        self.lib = self.plugin_dir / (installer.PLUGIN + "-v0.1.0-rc.20.so")
        self.lib.write_bytes(b"original-library")
        self.checker = self.root / "window-refresh"
        self.checker.write_bytes(b"original-checker")
        self.record_file = self.record_dir / "installed.json"
        self.record = {"config": str(self.config), "version": "v0.1.0-rc.20", "library": str(self.lib),
                       "checker": str(self.checker), "dropin": None, "installed_hashes": {
            str(path): hashlib.sha256(path.read_bytes()).hexdigest() for path in [self.lib, self.checker]}}
        self.record_file.write_text(json.dumps(self.record))
        self.record_file.chmod(0o600)
        self.plan = {"directory": self.root, "plugin_dir": self.plugin_dir, "config": self.config,
                     "text": self.original.decode(), "policy": self.policy, "wrapper": self.wrapper,
                     "record": self.record_dir, "quota_transport": "host", "url": "http://127.0.0.1:8317/v0/management"}
        self.args = argparse.Namespace(version="v0.1.0-rc.21", restart=False, uninstall=False,
                                       check_model=False, yes=True)
        self.unpacked = self.root / "package"
        self.unpacked.mkdir()
        (self.unpacked / "BUILD-INFO.json").write_text('{"hot_reload_protocol":1}')
        (self.unpacked / "codex-window-refresh.so").write_bytes(b"new-library")
        (self.unpacked / "window-refresh").write_bytes(b"new-checker")
        self.target = self.plugin_dir / (installer.PLUGIN + "-v0.1.0-rc.21.so")
        self.calls = []
        self.mode = "success"
        self.active = "0.1.0-rc.20"
        self.instance = "original"
        self.new_instance = True
        self.credentials = mock.patch.object(installer, "management_credentials", return_value=("fixture-key", b"must-not-persist"))
        self.credentials.start()
        mock.patch.object(installer, "private_file", side_effect=pathlib.Path).start()
        mock.patch.object(installer.os, "fchown", return_value=None).start()
        directory = installer.directory
        mock.patch.object(installer, "directory", side_effect=lambda p, m, u, g, strict=False:
                          directory(p, m, os.geteuid(), os.getegid(), strict)).start()
        mock.patch.object(installer, "run", return_value="valid").start()
        mock.patch.object(installer, "service_run", side_effect=AssertionError("must never touch systemd")).start()
        mock.patch.object(installer, "hot_get", side_effect=self.get).start()
        wait = installer.hot_wait
        mock.patch.object(installer, "hot_wait", side_effect=lambda b, k, p, d: wait(b, k, p, d, timeout=0)).start()
        self.addCleanup(mock.patch.stopall)

    def get(self, base, route, key):
        self.assertEqual(key, "fixture-key")
        self.calls.append(route)
        data = yaml.safe_load(self.config.read_text())
        wrapper = data["plugins"]["configs"][installer.PLUGIN]
        if route == "/plugins":
            return {"plugins_enabled": True, "plugins_dir": str(self.plugin_dir), "plugins": [
                {"id": installer.PLUGIN, "effective_enabled": True, "path": str(self.lib)}]}
        if route.endswith("/config"):
            return wrapper
        self.assertEqual(route, "/codex-window-refresh/lifecycle")
        selected = wrapper.get("store", {}).get("version", "0.1.0-rc.20")
        if self.target.exists() and selected == "0.1.0-rc.21" and self.mode == "success":
            self.active = selected
            self.instance = "replacement" if self.new_instance else "original"
        elif selected == "0.1.0-rc.20" and self.mode != "unreachable-rollback":
            self.active = selected
        elif self.target.exists() and self.mode == "unreachable-rollback":
            raise installer.ManagementQueryError("unreachable")
        return {"version": self.active, "hot_reload_protocol": 1, "phase": "running", "instance_id": self.instance}

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
        self.assertEqual(config["plugins"]["configs"][installer.PLUGIN]["store"]["version"], "0.1.0-rc.21")
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
        self.assertEqual(wrapper["store"]["version"], "0.1.0-rc.20")
        self.assertEqual({k: v for k, v in wrapper.items() if k != "store"}, self.wrapper)
        record = json.loads(self.record_file.read_text())
        self.assertEqual(record["version"], "v0.1.0-rc.20")
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
        with mock.patch.object(installer, "hot_get", side_effect=installer.ManagementQueryError("404", 404)):
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


class HotTransportTests(unittest.TestCase):
    def test_get_only_allowlist_no_redirect_or_secret_error_body(self):
        base = "http://127.0.0.1:8317/v0/management"
        with mock.patch.object(installer.urllib.request, "build_opener") as build:
            with self.assertRaises(installer.InstallError):
                installer.hot_get(base, "/api-call", "fixture-key")
            build.assert_not_called()
            build.return_value.open.side_effect = urllib.error.HTTPError(base, 401, "fixture-secret", {}, io.BytesIO(b"fixture-private-body"))
            with self.assertRaises(installer.ManagementQueryError) as error:
                installer.hot_get(base, "/codex-window-refresh/lifecycle", "fixture-key")
            self.assertNotIn("fixture", str(error.exception))
            request = build.return_value.open.call_args.args[0]
            self.assertEqual(request.get_method(), "GET")
            self.assertEqual(request.get_header("Authorization"), "Bearer fixture-key")
            self.assertIsNone(request.data)
        with self.assertRaises(installer.InstallError):
            installer.NoManagementRedirect().redirect_request(None, None, 302, "", {}, "https://elsewhere.invalid")


if __name__ == "__main__":
    unittest.main()
