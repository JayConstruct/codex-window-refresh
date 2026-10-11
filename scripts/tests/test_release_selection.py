import contextlib
import importlib.util
import io
import json
import pathlib
import sys
import tempfile
import types
import unittest
from unittest import mock


ROOT = pathlib.Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("release_installer", ROOT / "scripts/install.py")
installer = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(installer)


def release(tag, **extra):
    return dict(tag_name=tag, draft=False, published_at="2026-10-11T12:00:00Z",
                assets=[dict(name=f"codex-window-refresh_{tag}_linux_amd64.tar.gz", state="uploaded", size=100),
                        dict(name="SHA256SUMS", state="uploaded", size=100)], **extra)


class ReleaseSelectionTests(unittest.TestCase):
    def test_includes_prereleases_and_orders_versions_numerically(self):
        records = [release("v0.1.0-rc.2", prerelease=True), release("v0.1.0-rc.10", prerelease=True),
                   release("v0.0.9", prerelease=False)]
        with mock.patch.object(installer, "release_page", return_value=records) as pages:
            self.assertEqual(installer.latest_version("amd64"), "v0.1.0-rc.10")
        pages.assert_called_once_with(1)

    def test_stable_wins_same_version_and_newer_candidate_wins_older_stable(self):
        for records, expected in [([release("v1.0.0-rc.99"), release("v1.0.0")], "v1.0.0"),
                                  ([release("v1.0.0"), release("v1.1.0-rc.1")], "v1.1.0-rc.1")]:
            with mock.patch.object(installer, "release_page", return_value=records):
                self.assertEqual(installer.latest_version("amd64"), expected)

    def test_skips_drafts_unpublished_and_unsupported_tags(self):
        draft = release("v99.0.0"); draft["draft"] = True
        unpublished = release("v98.0.0"); unpublished["published_at"] = None
        with mock.patch.object(installer, "release_page", return_value=[draft, unpublished, release("nightly"), release("v1.0.0")]):
            self.assertEqual(installer.latest_version("amd64"), "v1.0.0")

    def test_reads_additional_pages_and_fails_on_query_limit(self):
        with mock.patch.object(installer, "release_page", side_effect=[[release("nightly")] * 100, [release("v1.0.0-rc.1")]]) as pages:
            self.assertEqual(installer.latest_version("amd64"), "v1.0.0-rc.1")
            self.assertEqual(pages.call_count, 2)
        with mock.patch.object(installer, "release_page", return_value=[release("nightly")] * 100):
            with self.assertRaises(installer.InstallError):
                installer.latest_version("amd64")

    def test_latest_missing_arch_or_checksum_fails_without_silent_downgrade(self):
        for assets in [[], [release("v2.0.0")["assets"][0]], [release("v2.0.0")["assets"][1]]]:
            newest = release("v2.0.0"); newest["assets"] = assets
            with mock.patch.object(installer, "release_page", return_value=[release("v1.0.0"), newest]):
                with self.assertRaises(installer.InstallError):
                    installer.latest_version("amd64")
        with mock.patch.object(installer, "release_page", return_value=[release("v1.0.0")]):
            with self.assertRaises(installer.InstallError):
                installer.latest_version("arm64")

    def test_duplicate_or_unready_assets_are_rejected(self):
        for change in ["duplicate", "uploading", "oversize", "invalid_size"]:
            item = release("v1.0.0")
            if change == "duplicate": item["assets"].append(item["assets"][0])
            if change == "uploading": item["assets"][0]["state"] = "new"
            if change == "oversize": item["assets"][0]["size"] = (64 << 20) + 1
            if change == "invalid_size": item["assets"][0]["size"] = True
            with mock.patch.object(installer, "release_page", return_value=[item]):
                with self.assertRaises(installer.InstallError):
                    installer.latest_version("amd64")

    def test_api_response_is_bounded_and_fixed_to_github(self):
        result = mock.MagicMock()
        result.__enter__.return_value = result
        result.url = installer.RELEASE_API
        result.read.return_value = json.dumps([release("v1.0.0")]).encode()
        with mock.patch.object(installer.urllib.request, "urlopen", return_value=result) as fetch:
            self.assertEqual(installer.release_page(1)[0]["tag_name"], "v1.0.0")
        request = fetch.call_args.args[0]
        self.assertEqual(request.full_url, installer.RELEASE_API + "?per_page=100&page=1")
        self.assertNotIn("Authorization", request.headers)
        result.read.assert_called_once_with((2 << 20) + 1)
        for data, url in [(b"{}", installer.RELEASE_API), (b"[]", "https://example.com"),
                          (b"x" * ((2 << 20) + 1), installer.RELEASE_API)]:
            result.url, result.read.return_value = url, data
            with mock.patch.object(installer.urllib.request, "urlopen", return_value=result):
                with self.assertRaises(installer.InstallError): installer.release_page(1)

    def test_api_failure_does_not_expose_response_context(self):
        with mock.patch.object(installer.urllib.request, "urlopen", side_effect=OSError("fixture-private-context")):
            with self.assertRaises(installer.InstallError) as error: installer.release_page(1)
        self.assertNotIn("fixture-private-context", str(error.exception))
        self.assertIn("--version", str(error.exception))

    def test_main_resolves_once_but_preview_uninstall_and_pins_need_no_api(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            plan = {name: root / name for name in ["binary", "config", "plugin_dir", "policy", "state", "env"]}
            plan.update(directory=root, user="root", service=None, url="http://127.0.0.1:8317/v0/management")
            original = pathlib.Path.read_text

            def read(path, *args, **kwargs):
                return 'ID=debian\nVERSION_ID="12"\n' if str(path) == "/etc/os-release" else original(path, *args, **kwargs)

            cases = [([], True), (["--dry-run"], False), (["--uninstall"], False),
                     (["--version", "v1.0.0"], False), (["--version", "v1.0.0-rc.2", "--package", "offline.tar.gz", "--checksums", "SHA256SUMS"], False)]
            for flags, online in cases:
                with self.subTest(flags=flags), contextlib.ExitStack() as stack:
                    stack.enter_context(mock.patch.object(sys, "argv", ["install.py", *flags]))
                    stack.enter_context(mock.patch.object(installer.os, "geteuid", return_value=0))
                    stack.enter_context(mock.patch.object(installer.platform, "system", return_value="Linux"))
                    stack.enter_context(mock.patch.object(installer.platform, "machine", return_value="x86_64"))
                    stack.enter_context(mock.patch.object(pathlib.Path, "read_text", new=read))
                    stack.enter_context(mock.patch.object(installer, "infer", return_value=plan))
                    stack.enter_context(mock.patch.object(installer, "verify_elf"))
                    stack.enter_context(mock.patch.object(installer.os, "fstat", return_value=types.SimpleNamespace(st_uid=0)))
                    install = stack.enter_context(mock.patch.object(installer, "install"))
                    stack.enter_context(mock.patch.object(installer, "uninstall"))
                    latest = stack.enter_context(mock.patch.object(installer, "latest_version", return_value="v1.0.0-rc.10"))
                    stack.enter_context(contextlib.redirect_stdout(io.StringIO()))
                    installer.main()
                    if online:
                        latest.assert_called_once_with("amd64")
                        self.assertEqual(install.call_args.args[0].version, "v1.0.0-rc.10")
                    else: latest.assert_not_called()

    def test_unresolved_offline_install_is_rejected_before_query_or_writes(self):
        with mock.patch.object(sys, "argv", ["install.py", "--package", "offline.tar.gz"]), \
                mock.patch.object(installer.os, "geteuid", return_value=0), \
                mock.patch.object(installer.platform, "system", return_value="Linux"), \
                mock.patch.object(installer.platform, "machine", return_value="x86_64"), \
                mock.patch.object(pathlib.Path, "read_text", return_value='ID=debian\nVERSION_ID="12"\n'), \
                mock.patch.object(installer, "latest_version") as query, \
                mock.patch.object(installer, "infer") as infer:
            with self.assertRaises(installer.InstallError): installer.main()
        query.assert_not_called(); infer.assert_not_called()

    def test_model_diagnostic_skips_release_package_installer_lock_and_file_writes(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            plan = {name: root / name for name in ["binary", "config", "plugin_dir", "policy", "state", "env"]}
            plan.update(directory=root, user="root", service=None, url="http://127.0.0.1:8317/v0/management")
            with contextlib.ExitStack() as stack:
                stack.enter_context(mock.patch.object(sys, "argv", ["install.py", "--check-model"]))
                stack.enter_context(mock.patch.object(installer.os, "geteuid", return_value=0))
                stack.enter_context(mock.patch.object(installer.platform, "system", return_value="Linux"))
                stack.enter_context(mock.patch.object(installer.platform, "machine", return_value="x86_64"))
                stack.enter_context(mock.patch.object(pathlib.Path, "read_text", return_value='ID=debian\nVERSION_ID="12"\n'))
                stack.enter_context(mock.patch.object(installer, "infer", return_value=plan))
                stack.enter_context(mock.patch.object(installer, "verify_elf"))
                stack.enter_context(mock.patch.object(installer, "management_credentials", return_value=("fixture-secret", None)))
                detect = stack.enter_context(mock.patch.object(installer, "discover_codex_model", return_value=("fixture-mini", 2)))
                install = stack.enter_context(mock.patch.object(installer, "install"))
                query = stack.enter_context(mock.patch.object(installer, "latest_version"))
                write = stack.enter_context(mock.patch.object(installer.os, "open", side_effect=AssertionError("no installer lock or file writes")))
                output = stack.enter_context(contextlib.redirect_stdout(io.StringIO()))
                installer.main()
                detect.assert_called_once_with(plan["url"], "fixture-secret")
                install.assert_not_called(); query.assert_not_called(); write.assert_not_called()
                self.assertIn("模型诊断", output.getvalue())
                self.assertNotIn("fixture-secret", output.getvalue())
            self.assertEqual(list(root.iterdir()), [])


if __name__ == "__main__":
    unittest.main()
