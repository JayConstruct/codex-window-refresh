import hashlib
import importlib.util
import io
import json
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


if __name__ == "__main__":
    unittest.main()
