import os
import pathlib
import subprocess
import tempfile
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[2]


class BootstrapTests(unittest.TestCase):
    def run_script(self, directory, arguments=(), script=None, **settings):
        record = pathlib.Path(directory) / "arguments"
        operations = pathlib.Path(directory) / "operations"
        environment = dict(os.environ, TEST_UID="0", TEST_OS="debian", TEST_VERSION="12",
                           TEST_ARCH="x86_64", TEST_DEPENDENCIES="1", TEST_CURL_STATUS="0",
                           TEST_INSTALL_STATUS="0", TEST_APT_STATUS="0",
                           TEST_ARGUMENTS=str(record), TEST_OPERATIONS=str(operations))
        environment.update(settings)
        shell = '''
id() { printf '%s\\n' "$TEST_UID"; }
cat() {
  if [[ $1 == /etc/os-release ]]; then
    printf 'ID=%s\\nVERSION_ID="%s"\\n' "$TEST_OS" "$TEST_VERSION"
  else
    command cat "$@"
  fi
}
uname() { printf '%s\\n' "$TEST_ARCH"; }
dpkg-query() {
  if [[ $TEST_DEPENDENCIES == 1 ]]; then printf 'install ok installed'; else return 1; fi
}
apt-get() {
  printf 'apt %s\\n' "$*" >> "$TEST_OPERATIONS"
  return "$TEST_APT_STATUS"
}
curl() {
  printf 'curl %s\\n' "$*" >> "$TEST_OPERATIONS"
  if [[ $TEST_CURL_STATUS != 0 ]]; then return "$TEST_CURL_STATUS"; fi
  local destination=${@: -1}
  if [[ $* == *main/scripts/install.sh* ]]; then
    command cp "$TEST_BOOTSTRAP" "$destination"
  fi
}
python3() {
  if [[ $1 == -c ]]; then return 0; fi
  printf '%s\\n' "$@" > "$TEST_ARGUMENTS"
  return "$TEST_INSTALL_STATUS"
}
export -f id cat uname dpkg-query apt-get curl python3
bash "$1" "${@:2}"
'''
        environment["TEST_BOOTSTRAP"] = str(ROOT / "scripts/install.sh")
        if script is None:
            script = pathlib.Path(directory) / "install.sh"
            script.write_bytes((ROOT / "scripts/install.sh").read_bytes())
        result = subprocess.run(["bash", "-c", shell, "bootstrap-test",
                                 str(script), *arguments],
                                env=environment, capture_output=True, text=True)
        return result, record, operations

    def test_install_and_legacy_upgrade_share_latest_noninteractive_defaults(self):
        for script in [ROOT / "scripts/install.sh", ROOT / "cpa-quota-upgrade.sh"]:
            with self.subTest(script=script), tempfile.TemporaryDirectory() as directory:
                result, record, operations = self.run_script(directory, script=script)
                self.assertEqual(result.returncode, 0, result.stderr)
                values = record.read_text().splitlines()
                self.assertEqual(values[1:], ["--version", "latest", "--quota-transport", "host",
                                              "--defer-model", "--yes", "--restart"])
                self.assertEqual(pathlib.Path(values[0]), ROOT / "scripts/install.py")
                self.assertFalse(operations.exists())

    def test_missing_dependencies_are_installed_automatically(self):
        with tempfile.TemporaryDirectory() as directory:
            result, record, operations = self.run_script(directory, TEST_DEPENDENCIES="0")
            self.assertEqual(result.returncode, 0, result.stderr)
            actions = operations.read_text().splitlines()
            self.assertEqual(actions[0], "apt update")
            self.assertEqual(actions[1], "apt install -y --no-install-recommends ca-certificates curl python3 python3-yaml")
            self.assertTrue(record.exists())

    def test_options_with_spaces_are_forwarded_and_defaults_can_be_overridden(self):
        options = ["--cpa-dir", "/fixture/cpa with spaces", "--version", "v1.0.0",
                   "--model", "fixture-model", "--user-service", "--service", "cpa.service", "--user", "root"]
        with tempfile.TemporaryDirectory() as directory:
            result, record, _ = self.run_script(directory, options)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(record.read_text().splitlines()[-len(options):], options)

    def test_read_only_modes_and_no_restart_do_not_request_restart(self):
        for option in ["--dry-run", "--check-model", "--no-restart", "--uninstall"]:
            with self.subTest(option=option), tempfile.TemporaryDirectory() as directory:
                result, record, _ = self.run_script(directory, [option])
                self.assertEqual(result.returncode, 0, result.stderr)
                values = record.read_text().splitlines()
                self.assertNotIn("--restart", values)
                self.assertEqual(option in values, option != "--no-restart")

    def test_read_only_modes_do_not_install_missing_dependencies(self):
        for option in ["--dry-run", "--check-model"]:
            with self.subTest(option=option), tempfile.TemporaryDirectory() as directory:
                result, record, operations = self.run_script(directory, [option], TEST_DEPENDENCIES="0")
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(record.exists() or operations.exists())

    def test_preconditions_fail_before_download_or_dependency_install(self):
        for settings in [dict(TEST_UID="1000"), dict(TEST_OS="ubuntu"),
                         dict(TEST_VERSION="11"), dict(TEST_ARCH="armv7l")]:
            with self.subTest(settings=settings), tempfile.TemporaryDirectory() as directory:
                result, record, operations = self.run_script(directory, **settings)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(record.exists() or operations.exists())

    def test_failed_download_does_not_execute_installer(self):
        with tempfile.TemporaryDirectory() as directory:
            result, record, _ = self.run_script(directory, TEST_CURL_STATUS="22")
            self.assertEqual(result.returncode, 22)
            self.assertFalse(record.exists())

    def test_failed_dependency_install_does_not_download_or_install(self):
        with tempfile.TemporaryDirectory() as directory:
            result, record, operations = self.run_script(directory, TEST_DEPENDENCIES="0", TEST_APT_STATUS="1")
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(operations.read_text(), "apt update\n")
            self.assertFalse(record.exists())

    def test_failed_installer_propagates_status_and_cleans_temporary_directory(self):
        with tempfile.TemporaryDirectory() as directory:
            result, record, _ = self.run_script(directory, TEST_INSTALL_STATUS="1")
            self.assertEqual(result.returncode, 1)
            self.assertFalse(pathlib.Path(record.read_text().splitlines()[0]).parent.exists())

    def test_downloaded_legacy_script_delegates_to_same_bootstrap(self):
        with tempfile.TemporaryDirectory() as directory:
            legacy = pathlib.Path(directory) / "upgrade.sh"
            legacy.write_bytes((ROOT / "cpa-quota-upgrade.sh").read_bytes())
            result, record, operations = self.run_script(directory, ["--model", "fixture-model"], script=legacy)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(record.read_text().splitlines()[-2:], ["--model", "fixture-model"])
            self.assertEqual(operations.read_text().count("curl "), 2)
            bootstrap_path = operations.read_text().splitlines()[0].split()[-1]
            self.assertFalse(pathlib.Path(bootstrap_path).parent.exists())

    def test_help_needs_no_root_download_or_dependencies(self):
        with tempfile.TemporaryDirectory() as directory:
            result, record, operations = self.run_script(directory, ["--help"], TEST_UID="1000")
            self.assertEqual(result.returncode, 0)
            self.assertIn("--no-restart", result.stdout)
            self.assertFalse(record.exists() or operations.exists())


if __name__ == "__main__":
    unittest.main()
