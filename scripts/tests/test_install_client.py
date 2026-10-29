import hashlib
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]


class ClientInstallerTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.installer = self.root / "install-client.sh"
        shutil.copyfile(ROOT / "scripts/install-client.sh", self.installer)
        self.destination = self.root / "client bin"
        self.operations = self.root / "downloads"

    def install(self, *arguments, status="0", source=None):
        environment = dict(os.environ, HOME=str(self.root), TEST_DOWNLOAD_STATUS=status,
                           TEST_CLIENT_SOURCE=str(source or ROOT / "scripts/cwr-codex.py"),
                           TEST_OPERATIONS=str(self.operations))
        shell = '''
curl() {
  printf '%s\\n' "$*" >> "$TEST_OPERATIONS"
  local destination=${@: -1}
  if [[ $TEST_DOWNLOAD_STATUS != 0 ]]; then
    printf 'incomplete download' > "$destination"
    return "$TEST_DOWNLOAD_STATUS"
  fi
  command cp "$TEST_CLIENT_SOURCE" "$destination"
}
export -f curl
bash "$1" "${@:2}"
'''
        return subprocess.run(["bash", "-c", shell, "client-test", str(self.installer), *arguments],
                              env=environment, capture_output=True, text=True)

    def test_install_update_and_executable_command_preserve_configuration_and_maps(self):
        config = self.root / ".codex" / "config.toml"
        mapping = self.root / ".config" / "cwr" / "projects.json"
        for file in (config, mapping):
            file.parent.mkdir(parents=True, exist_ok=True)
            file.write_text("preserve me", encoding="utf-8")
        self.destination.mkdir()
        script = self.destination / "cwr-codex.py"
        script.write_text("old script", encoding="utf-8")
        for _ in range(2):
            result = self.install("--bin-dir", str(self.destination))
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(script.read_bytes(), (ROOT / "scripts/cwr-codex.py").read_bytes())
            command = self.destination / "cwr-codex"
            self.assertEqual(os.readlink(command), "cwr-codex.py")
            self.assertTrue(os.access(command, os.X_OK))
            executed = subprocess.run([str(command), "--help"], capture_output=True, text=True)
            self.assertEqual(executed.returncode, 0, executed.stderr)
            self.assertIn("--provider", executed.stdout)
            self.assertFalse(list(self.destination.glob(".cwr-*")))
            for file in (config, mapping):
                self.assertEqual(file.read_text(encoding="utf-8"), "preserve me")
        request = self.operations.read_text(encoding="utf-8")
        self.assertIn("/" + (ROOT / "release/v2-version.txt").read_text().strip() + "/scripts/cwr-codex.py", request)
        self.assertNotIn("/main/scripts/cwr-codex.py", request)

    def test_default_user_directory_and_offline_package_install(self):
        shutil.copyfile(ROOT / "scripts/cwr-codex.py", self.root / "cwr-codex.py")
        result = self.install(status="99")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue((self.root / ".local/bin/cwr-codex").is_file())
        self.assertFalse(self.operations.exists())

    def test_download_and_checksum_failures_leave_existing_install_untouched(self):
        self.destination.mkdir()
        installed = self.destination / "cwr-codex.py"
        installed.write_text("old", encoding="utf-8")
        command = self.destination / "cwr-codex"
        command.symlink_to(installed.name)
        bad = self.root / "bad.py"
        bad.write_text("raise SystemExit('never run download')", encoding="utf-8")
        for options in [dict(status="22"), dict(source=bad)]:
            result = self.install("--bin-dir", str(self.destination), **options)
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(installed.read_text(encoding="utf-8"), "old")
            self.assertEqual(os.readlink(command), installed.name)
            self.assertFalse(list(self.destination.glob(".cwr-*")))

    def test_invalid_options_and_directory_targets_fail_without_replacing_files(self):
        for arguments in [["--bin-dir"], ["--bin-dir", ""], ["--version", "main"]]:
            result = self.install(*arguments)
            self.assertNotEqual(result.returncode, 0)
            self.assertFalse(self.operations.exists())
        target = self.destination / "cwr-codex"
        target.mkdir(parents=True)
        result = self.install("--bin-dir", str(self.destination))
        self.assertNotEqual(result.returncode, 0)
        self.assertTrue(target.is_dir())
        self.assertFalse((self.destination / "cwr-codex.py").exists())

    def test_installer_checksum_matches_launcher(self):
        expected = hashlib.sha256((ROOT / "scripts/cwr-codex.py").read_bytes()).hexdigest()
        self.assertIn("CLIENT_SHA256=" + expected + "\n", self.installer.read_text())
