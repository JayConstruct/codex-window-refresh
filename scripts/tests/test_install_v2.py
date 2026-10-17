import hashlib
import importlib.util
import io
import json
import os
import pathlib
import platform
import struct
import tarfile
import tempfile
import unittest
from unittest import mock

ROOT = pathlib.Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("installer_v2", ROOT / "scripts/install-v2.py")
v2 = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(v2)


class V2PackageTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = pathlib.Path(self.temp.name)
        self.version = "v0.2.0-rc.1"
        self.filename = f"{v2.PREFIX}_{self.version}_linux_amd64.tar.gz"

    def package(self, extra=None, changes=None):
        elf = b"\x7fELF\x02\x01" + b"\0" * 12 + struct.pack("<H", 62)
        info = dict(backend="v2", release=self.version, bridge_version=self.version, architecture="amd64", platform="linux",
                    host_abi=1, rpc_schema=6, protocol_version=1, model_execution=False, minimum_glibc_symbols="2.0")
        info.update(changes or {})
        members = {"window-refreshd": elf, "codex-window-refresh-v2-bridge.so": elf,
                   "BUILD-INFO.json": json.dumps(info).encode()}
        archive = self.root / self.filename
        with tarfile.open(archive, "w:gz") as tar:
            for name, data in members.items():
                entry = tarfile.TarInfo(self.filename[:-7] + "/" + name)
                entry.size = len(data)
                tar.addfile(entry, io.BytesIO(data))
            if extra:
                tar.addfile(extra, io.BytesIO(b""))
        sums = self.root / "SHA256SUMS"
        sums.write_text(f"{hashlib.sha256(archive.read_bytes()).hexdigest()}  {self.filename}\n")
        output = self.root / "unpacked"
        output.mkdir(exist_ok=True)
        return archive, sums, output

    def unpack(self, files):
        with mock.patch.object(platform, "libc_ver", return_value=("glibc", "2.36")):
            return v2.unpack(files[0], files[1], self.filename, files[2], self.version, "amd64")

    def test_valid_v2_and_wrong_protocol_or_backend(self):
        self.assertEqual(self.unpack(self.package())["backend"], "v2")
        for changes in [dict(backend="v1"), dict(protocol_version=2), dict(architecture="arm64"),
                        dict(model_execution=True), dict(release="v0.2.0-rc.2")]:
            with self.subTest(changes=changes), self.assertRaises(v2.InstallError):
                self.unpack(self.package(changes=changes))

    def test_checksum_traversal_and_symlink_rejected(self):
        files = self.package()
        files[0].write_bytes(files[0].read_bytes() + b"changed")
        with self.assertRaises(v2.InstallError): self.unpack(files)
        for name, kind in [("../escape", tarfile.REGTYPE), ("window-refreshd", tarfile.SYMTYPE)]:
            entry = tarfile.TarInfo(self.filename[:-7] + "/" + name)
            entry.type = kind
            entry.linkname = "/etc/passwd"
            with self.assertRaises(v2.InstallError): self.unpack(self.package(extra=entry))

    def test_explicit_wake_capability_is_accepted_without_enabling_any_rules(self):
        info = self.unpack(self.package(changes=dict(model_execution=True, wake=True, wake_providers=["codex"])))
        self.assertIs(info["model_execution"], True)
        for changes in [dict(model_execution="true", wake=True, wake_providers=["codex"]),
                        dict(model_execution=1, wake=True, wake_providers=["codex"]),
                        dict(model_execution=True, wake=False, wake_providers=["codex"]),
                        dict(model_execution=True, wake=True, wake_providers=["antigravity"])]:
            with self.subTest(changes=changes), self.assertRaises(v2.InstallError):
                self.unpack(self.package(changes=changes))

    def test_v1_latest_skips_v2_but_does_not_silently_downgrade_v1(self):
        records = [dict(tag_name="v0.1.0-rc.28", draft=False, published_at="now", assets=[
            dict(name="codex-window-refresh_v0.1.0-rc.28_linux_amd64.tar.gz", state="uploaded", size=100),
            dict(name="SHA256SUMS", state="uploaded", size=100)]),
            dict(tag_name=self.version, draft=False, published_at="now", assets=[
                dict(name=self.filename, state="uploaded", size=100), dict(name="SHA256SUMS", state="uploaded", size=100)])]
        with mock.patch.object(v2.common, "release_page", return_value=records):
            self.assertEqual(v2.common.latest_version("amd64"), "v0.1.0-rc.28")
            self.assertEqual(v2.latest("amd64"), self.version)
            with self.assertRaises(v2.InstallError): v2.latest("arm64")


class V2CPAResourceTests(unittest.TestCase):
    def test_listener_address_and_tls_are_inferred(self):
        self.assertEqual(v2.cpa_resource_url({"data": {"host": "0.0.0.0", "port": 9999}}),
                         "http://127.0.0.1:9999/v0/resource/plugins/codex-window-refresh/healthz")
        self.assertTrue(v2.cpa_resource_url({"data": {"server": {"host": "::", "port": 9443,
                            "tls": {"enable": True}}}}).startswith("https://[::1]:9443/"))
        self.assertTrue(v2.cpa_resource_url({"cpa_url": "https://cpa.example.com:9443/"}).startswith(
                        "https://cpa.example.com:9443/"))

    def test_resource_probe_never_uses_a_key_or_redirect_address(self):
        for url in ("http://user:secret@127.0.0.1", "http://127.0.0.1?token=secret",
                    "http://127.0.0.1/admin", "file:///tmp/daemon.sock", "http://127.0.0.1:0"):
            with self.subTest(url=url), self.assertRaises(v2.InstallError):
                v2.cpa_resource_url({"cpa_url": url})


class V2CleanupTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = pathlib.Path(self.temp.name)
        self.cpa = self.root / "cpa"
        self.cpa.mkdir()
        self.plan = dict(directory=self.cpa, binary=self.cpa/"cli-proxy-api", config=self.cpa/"config.yaml",
                         uid=os.getuid(), plugin_dir=self.cpa/"plugins", plugins={"configs": {}},
                         data={"auth-dir": str(self.cpa/"auth")})
        backend = self.cpa / "window-refresh-v2"
        self.locations = dict(root=backend, data=backend/"data", runtime=backend/"run", record=backend/"installed.json",
                              unit=self.root/"units/backend.service", dropin=self.root/"units/cpa.d/75-window-refresh-v2.conf")
        self.current = backend / "releases/v0.2.0-rc.2"
        self.library = self.plan["plugin_dir"] / "codex-window-refresh-v0.2.0-rc.1.so"

    def put(self, path, text="fixture"):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text)
        return path

    def test_cleanup_removes_legacy_and_backups_but_keeps_current_data_and_peers(self):
        old = [self.cpa/"window-refresh.yaml", self.cpa/"window-refresh.env", self.cpa/"window-refresh-state/state.json",
               self.cpa/"window-refresh-install/backup/private.json", self.cpa/"window-refresh",
               self.plan["plugin_dir"]/"codex-window-refresh-v0.1.0-rc.28.so",
               self.plan["plugin_dir"]/"codex-window-refresh.so",
               self.locations["root"]/"releases/v0.2.0-rc.1/window-refreshd",
               self.locations["root"]/"backups/old/backend.sqlite"]
        keep = [self.plan["config"], self.plan["binary"], self.cpa/"auth/account.json", self.library,
                self.current/"window-refreshd", self.locations["data"]/"backend.sqlite",
                self.plan["plugin_dir"]/"another-plugin.so", self.cpa/"other-state/data.json"]
        for path in old + keep: self.put(path)
        targets = v2.cleanup_plan(self.plan, self.locations, None, self.library, self.current)
        v2.cleanup_deployment(targets, self.plan, self.locations)
        for path in old: self.assertFalse(path.exists(), str(path))
        for path in keep: self.assertEqual(path.read_text(), "fixture", str(path))

    def test_cleanup_refuses_host_auth_current_data_other_plugins_and_symlinks(self):
        peer = self.cpa / "peer-state"
        self.plan["plugins"]["configs"]["peer"] = {"state_dir": str(peer)}
        for path in [self.cpa, self.plan["config"], self.cpa/"auth", self.cpa/"auth/account.json",
                     self.locations["data"], self.locations["data"]/"backend.sqlite", peer, peer/"data.json"]:
            with self.subTest(path=path), self.assertRaises(v2.InstallError):
                v2.validate_cleanup_path(path, self.plan, self.locations)
        self.put(self.cpa/"auth/account.json")
        (self.cpa/"window-refresh-state").symlink_to(self.cpa/"auth", target_is_directory=True)
        with self.assertRaises(v2.InstallError):
            v2.cleanup_plan(self.plan, self.locations, None, self.library, self.current)
        self.assertEqual((self.cpa/"auth/account.json").read_text(), "fixture")


if __name__ == "__main__":
    unittest.main()
