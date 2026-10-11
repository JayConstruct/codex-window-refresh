import argparse
import importlib.util
import json
import pathlib
import tempfile
import unittest
import urllib.parse
from unittest import mock


ROOT = pathlib.Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("model_installer", ROOT / "scripts/install.py")
installer = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(installer)


def account(name, **extra):
    return dict(id=name, provider="codex", disabled=False, **extra)


def models(*names):
    return {"models": [{"id": name} for name in names]}


class ModelDiscoveryTests(unittest.TestCase):
    def test_reads_common_codex_registry_models_and_prefers_mini_without_execution(self):
        entries = [account("first ?/账号"), account("second", unavailable=True),
                   dict(id="disabled", provider="codex", disabled=True),
                   dict(id="other", provider="claude", disabled=False)]
        calls = []

        def read(base, path, key, timeout):
            calls.append(path)
            self.assertEqual(base, "http://127.0.0.1:8317/v0/management")
            self.assertEqual(key, "fixture-secret")
            self.assertGreater(timeout, 0)
            if path == "/auth-files": return {"files": entries}
            name = urllib.parse.parse_qs(urllib.parse.urlsplit(path).query)["name"][0]
            return models("codex-large", "codex-mini", "first-only") if name == "first ?/账号" else models("codex-large", "codex-mini")

        with mock.patch.object(installer, "management_json", side_effect=read):
            self.assertEqual(installer.discover_codex_model("http://127.0.0.1:8317/v0/management/api-call", "fixture-secret"), ("codex-mini", 2))
        self.assertEqual(len(calls), 3)
        self.assertNotIn("账号", calls[1])
        self.assertTrue(all(p == "/auth-files" or p.startswith("/auth-files/models?") for p in calls))

    def test_common_alias_is_supported_and_never_guesses_unknown_model(self):
        with mock.patch.object(installer, "management_json", side_effect=[{"files": [account("a")]}, models("custom-alias")]):
            self.assertEqual(installer.discover_codex_model("https://cpa.example.com", "secret"), ("custom-alias", 1))

    def test_no_accounts_empty_models_no_common_models_and_limits_fail_closed(self):
        fixtures = [
            [{"files": []}],
            [{"files": [account("a")]}, models()],
            [{"files": [account("a"), account("b")]}, models("a-only"), models("b-only")],
            [{"files": [account(str(i)) for i in range(65)]}],
            [{"files": [account("same"), account("same")]}],
            [{"files": [dict(id="a", provider="codex")]}],
            [{"files": "not-a-list"}],
            [{"files": [account("a")]}, {"models": "not-a-list"}],
            [{"files": [account("a")]}, models("model\nterminal-command", "REPLACE_WITH_MODEL")],
        ]
        for fixture in fixtures:
            with self.subTest(fixture=fixture), mock.patch.object(installer, "management_json", side_effect=fixture):
                with self.assertRaises(installer.InstallError):
                    installer.discover_codex_model("http://localhost:8317", "secret")

    def test_discovery_deadline_prevents_additional_reads(self):
        with mock.patch.object(installer.time, "monotonic", side_effect=[0, 0, 31]), \
                mock.patch.object(installer, "management_json", return_value={"files": [account("a")]}) as read:
            with self.assertRaises(installer.InstallError): installer.discover_codex_model("http://localhost:8317", "secret")
        self.assertEqual(read.call_count, 1)

    def test_management_paths_and_redirects_rejected_before_sending_key(self):
        for url in ["https://user:secret@cpa.example.com", "https://cpa.example.com/?secret=x", "https://cpa.example.com/v8/management/requests/api-call"]:
            with mock.patch.object(installer, "management_json") as read:
                with self.assertRaises(installer.InstallError): installer.discover_codex_model(url, "secret")
            read.assert_not_called()
        with self.assertRaises(installer.InstallError):
            installer.NoManagementRedirect().redirect_request(None, None, 302, "", {}, "https://other.example.com")

    def test_management_transport_get_only_bounded_response_and_no_error_body_exposure(self):
        reply = mock.MagicMock()
        reply.__enter__.return_value = reply
        reply.url = "http://localhost:8317/v0/management/auth-files"
        reply.read.return_value = b'{"files": []}'
        opener = mock.Mock(); opener.open.return_value = reply
        with mock.patch.object(installer.urllib.request, "build_opener", return_value=opener) as build:
            self.assertEqual(installer.management_json("http://localhost:8317/v0/management", "/auth-files", "fixture-secret", 5), {"files": []})
        self.assertIsInstance(build.call_args.args[0], installer.NoManagementRedirect)
        request = opener.open.call_args.args[0]
        self.assertEqual(request.get_method(), "GET")
        self.assertEqual(request.get_header("Authorization"), "Bearer fixture-secret")
        self.assertIsNone(request.data)
        reply.read.assert_called_once_with((4 << 20) + 1)
        for url, raw in [(reply.url, b"x" * ((4 << 20) + 1)), ("https://other.example.com", b"{}"), (reply.url, b"[]")]:
            reply.url, reply.read.return_value = url, raw
            with mock.patch.object(installer.urllib.request, "build_opener", return_value=opener):
                with self.assertRaises(installer.InstallError): installer.management_json("http://localhost:8317/v0/management", "/auth-files", "fixture-secret", 5)
        opener.open.side_effect = OSError("fixture-private-context")
        with mock.patch.object(installer.urllib.request, "build_opener", return_value=opener):
            with self.assertRaises(installer.InstallError) as error: installer.management_json("http://localhost:8317/v0/management", "/auth-files", "fixture-secret", 5)
        self.assertNotIn("fixture-private-context", str(error.exception))

    def test_saved_key_decodes_literal_quotes_backslashes_and_shell_characters(self):
        with tempfile.TemporaryDirectory() as directory:
            env = pathlib.Path(directory) / "window-refresh.env"
            key = 'fixture"\\$(no-shell)`not-a-command`$HOME'
            env.write_bytes(installer.environment_content(key, "CPA_MANAGEMENT_KEY"))
            plan = {"env": env, "env_name": "CPA_MANAGEMENT_KEY"}
            with mock.patch.object(installer, "private_file", side_effect=lambda p: pathlib.Path(p)):
                self.assertEqual(installer.management_credentials(argparse.Namespace(key_file=None), plan), (key, None))
                env.write_text('CPA_MANAGEMENT_KEY="bad\\nkey"\n')
                with self.assertRaises(installer.InstallError): installer.management_credentials(argparse.Namespace(key_file=None), plan)


if __name__ == "__main__":
    unittest.main()
