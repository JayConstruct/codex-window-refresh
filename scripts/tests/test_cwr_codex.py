import argparse
import importlib.util
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import tempfile
import threading
import unittest
from unittest import mock
from client_protocol_fixture import CONFIG_READER, CPA_URL
from terminal_fixture import RECORD_LAUNCH, TerminalLaunch

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("cwr_codex", ROOT / "scripts/cwr-codex.py")
launcher = importlib.util.module_from_spec(spec)
spec.loader.exec_module(launcher)


class LauncherFixture(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.route_patch = mock.patch.object(launcher, "scheduling_route", side_effect=lambda args, *_:
                                             (args.provider, CPA_URL))
        self.route_patch.start()
        self.addCleanup(self.route_patch.stop)

    def prefix(self, provider="cpa"):
        return ["codex", "-C", str(self.root), "-c", 'model_provider="' + provider + '"',
                "-c", 'model_providers.' + provider + '.base_url="' + CPA_URL + '"']

    def args(self, **overrides):
        values = dict(directory=str(self.root), project="work", provider="cpa",
                      codex="codex", arguments=[], map=str(self.root / "missing-map.json"), mode="cpa")
        values.update(overrides)
        return argparse.Namespace(**values)


class DirectoryRouting(LauncherFixture):
    def test_automatic_identity_uses_nearest_git_root_and_canonical_paths(self):
        repo = self.root / "项目 商城"
        nested = repo / "src" / "api"
        nested.mkdir(parents=True)
        (repo / ".git").mkdir()
        (repo / ".git" / "HEAD").write_text("ref: refs/heads/main\n", encoding="utf-8")
        root, project = launcher.directory_project(nested)
        self.assertEqual(root, repo)
        self.assertRegex(project, r"^dir-[a-f0-9]{64}$")
        self.assertEqual(launcher.directory_project(repo)[1], project)
        alias = self.root / "alias"
        alias.symlink_to(nested, target_is_directory=True)
        self.assertEqual(launcher.directory_project(alias)[1], project)
        other = self.root / "other"
        other.mkdir()
        self.assertNotEqual(launcher.directory_project(other)[1], project)
        (nested / ".git").write_text("gitdir: /external/worktree", encoding="utf-8")
        self.assertEqual(launcher.directory_project(nested)[0], nested)

    def test_missing_default_map_uses_hash_and_encoded_name_without_full_path(self):
        with mock.patch.dict(os.environ, {"HOME": str(self.root)}):
            command = launcher.command(self.args(project=None, map=None))
        project = launcher.directory_project(self.root)[1]
        self.assertIn('model_providers.cpa.http_headers.X-CWR-Project="' + project + '"', command)
        name_header = next(item for item in command if ".X-CWR-Project-Name=" in item)
        self.assertNotIn(str(self.root.parent), name_header)
        repo = self.root / "中文 空格"
        repo.mkdir()
        with mock.patch.dict(os.environ, {"HOME": str(self.root)}):
            command = launcher.command(self.args(directory=str(repo), project=None, map=None))
        self.assertIn('model_providers.cpa.http_headers.X-CWR-Project-Name="%E4%B8%AD%E6%96%87%20%E7%A9%BA%E6%A0%BC"', command)

    def test_unmatched_legacy_map_falls_back_but_explicit_project_wins(self):
        mapping = self.root / "projects.json"
        mapping.write_text('{"projects":[]}', encoding="utf-8")
        command = launcher.command(self.args(project=None, map=str(mapping)))
        self.assertTrue(any('.X-CWR-Project="dir-' in item for item in command))
        command = launcher.command(self.args(project="explicit", map=str(mapping)))
        self.assertIn('model_providers.cpa.http_headers.X-CWR-Project="explicit"', command)
        self.assertFalse(any('.X-CWR-Project-Name=' in item for item in command))

    def test_longest_canonical_directory_match_and_safe_exec_arguments(self):
        nested = self.root / "nested"
        nested.mkdir()
        alias = self.root / "alias"
        alias.symlink_to(nested, target_is_directory=True)
        mappings = [{"directory": str(self.root), "project": "root"}, {"directory": str(nested), "project": "nested"}]
        self.assertEqual(launcher.project_for(alias, mappings), "nested")
        args = self.args(directory=str(alias), project="nested", arguments=["--", "exec", "a;$(echo literal)"])
        command = launcher.command(args)
        self.assertIn('model_providers.cpa.http_headers.X-CWR-Project="nested"', command)
        self.assertEqual(command[-1], "a;$(echo literal)")
        self.assertEqual(command[2], str(nested))

    def test_native_directory_options_use_the_requested_project_and_forward_remaining_arguments(self):
        nested = self.root / "requested project"
        nested.mkdir()
        for arguments in [["-C", str(nested)], ["--cd", str(nested)], ["--cd=" + str(nested)],
                          ["-C" + str(nested)], ["-C=" + str(nested)], ["exec", "-C", str(nested)]]:
            with self.subTest(arguments=arguments):
                command = launcher.command(self.args(project=None, map=None, arguments=["--", *arguments]))
                self.assertEqual(command[1:3], ["-C", str(nested)])
                project = launcher.directory_project(nested)[1]
                self.assertIn('model_providers.cpa.http_headers.X-CWR-Project="' + project + '"', command)
                self.assertEqual(command.count("-C"), 1)
                if arguments[0] == "exec":
                    self.assertEqual(command[-1], "exec")
        with mock.patch.dict(os.environ, {"HOME": str(self.root)}), mock.patch("os.getcwd", return_value=str(self.root)):
            directory, extra = launcher.launch_directory(["-C", "~/requested project", "resume", "--last"], "/unused")
        self.assertEqual(directory, nested)
        self.assertEqual(extra, ["resume", "--last"])

    def test_directory_flags_do_not_consume_option_values_or_literal_prompts(self):
        cases = [["-c", "-Config-literal", "exec", "--", "-C", "/prompt"],
                 ["--profile", "-Cprofile", "exec", "prompt"],
                 ["exec", "--", "--cd=/literal/prompt"]]
        for arguments in cases:
            with self.subTest(arguments=arguments):
                directory, extra = launcher.launch_directory(arguments, self.root)
                self.assertEqual(directory, self.root)
                self.assertEqual(extra, arguments)
        for arguments in [["-C"], ["--cd="], ["-C", str(self.root), "--cd", str(self.root)]]:
            with self.subTest(arguments=arguments), self.assertRaises(ValueError):
                launcher.launch_directory(arguments, self.root)

    def test_missing_and_duplicate_directory_mappings_are_rejected(self):
        with self.assertRaises(ValueError):
            launcher.project_for(self.root, [])
        with self.assertRaises(ValueError):
            launcher.project_for(self.root, [{"directory": str(self.root), "project": "a"}, {"directory": str(self.root), "project": "b"}])

    def test_exported_directory_map_launches_nested_client_project(self):
        nested = self.root / "repo" / "subdirectory"
        nested.mkdir(parents=True)
        mapping = self.root / "projects.json"
        mapping.write_text(json.dumps({"projects": [{"directory": str(self.root), "project": "root"}, {"directory": str(self.root / "repo"), "project": "repo"}]}))
        command = launcher.command(self.args(directory=str(nested), project=None, map=str(mapping)))
        self.assertIn('model_providers.cpa.http_headers.X-CWR-Project="repo"', command)
        self.assertEqual(command[2], str(nested))

    @unittest.skipIf(os.name == "nt", "POSIX client check")
    def test_foreign_windows_roots_do_not_break_a_shared_client_map(self):
        mappings = [{"directory": "C:\\Work\\Repo", "project": "windows"}, {"directory": str(self.root), "project": "local"}]
        self.assertEqual(launcher.project_for(self.root, mappings), "local")
        self.assertEqual(launcher.project_for(self.root, [{"directory": "//" + str(self.root).lstrip("/"), "project": "local"}]), "local")
        with self.assertRaises(ValueError):
            launcher.project_for(self.root, [{"directory": "C:relative", "project": "invalid"}])

    def test_launcher_requires_a_valid_project_instead_of_silently_skipping_header(self):
        with self.assertRaises(FileNotFoundError):
            launcher.command(self.args(project=None))
        with self.assertRaisesRegex(ValueError, "项目标识"):
            launcher.command(self.args(project="bad project"))
        with self.assertRaisesRegex(ValueError, "提供商"):
            launcher.command(self.args(provider="bad.provider"))
        mapping = self.root / "oversized.json"
        mapping.write_bytes(b" " * (256 * 1024 + 1))
        with self.assertRaisesRegex(ValueError, "过大"):
            launcher.command(self.args(project=None, map=str(mapping)))


class ExplicitLaunch(LauncherFixture):
    def test_header_building_after_route_validation_does_not_parse_toml_files(self):
        codex_home = self.root / "codex-home"
        codex_home.mkdir()
        config = codex_home / "config.toml"
        for content in [None, b'model_provider = "other"\n', b'\xffbroken TOML secret', b""]:
            if content is None:
                if config.exists():
                    config.unlink()
            else:
                config.write_bytes(content)
            with self.subTest(content=content), mock.patch.dict(os.environ, {"CODEX_HOME": str(codex_home)}), \
                    mock.patch.object(Path, "open", side_effect=AssertionError("launcher read configuration")):
                self.assertEqual(launcher.command(self.args()), [*self.prefix(), "-c",
                                 'model_providers.cpa.http_headers.X-CWR-Project="work"'])
            if content is not None:
                self.assertEqual(config.read_bytes(), content)

    def test_validated_provider_and_url_are_pinned_with_the_header_namespace(self):
        command = launcher.command(self.args(provider="my_proxy"))
        self.assertIn('model_providers.my_proxy.http_headers.X-CWR-Project="work"', command)
        self.assertIn('model_provider="my_proxy"', command)
        self.assertIn('model_providers.my_proxy.base_url="' + CPA_URL + '"', command)

    def test_validated_launch_preserves_native_arguments(self):
        cases = [
            ["--profile", "other", "exec", "prompt"],
            ["--profile=missing"],
            ["-pother"],
            ["-c", 'model_provider="other"', "exec", "prompt"],
            ["--config=model_provider=other"],
            ["-cmodel_provider='other'"],
            ["--oss", "--local-provider", "ollama"],
            ["--remote", "ws://localhost:1234"],
            ["exec", "--", "--oss", "-c", "literal prompt"],
        ]
        for arguments in cases:
            with self.subTest(arguments=arguments):
                self.assertEqual(launcher.command(self.args(arguments=["--", *arguments])),
                                 [*self.prefix(), "-c",
                                  'model_providers.cpa.http_headers.X-CWR-Project="work"', *arguments])

    @unittest.skipIf(os.name == "nt", "Executable fixture uses a POSIX shebang")
    def test_script_launch_adds_header_and_direct_launch_stays_unchanged(self):
        fake = self.root / "fake-codex"
        fake.write_text("#!" + sys.executable + "\n" + CONFIG_READER + "\nprint(json.dumps({'args':sys.argv[1:],'env':os.environ['CWR_TEST_LITERAL']}))\n", encoding="utf-8")
        fake.chmod(0o755)
        codex_home = self.root / "codex-home"
        codex_home.mkdir()
        config = codex_home / "config.toml"
        profile = codex_home / "other.config.toml"
        profile.write_bytes(b"not TOML")
        argv = [sys.executable, str(ROOT / "scripts/cwr-codex.py"), "--directory", str(self.root),
                "--codex", str(fake), "--provider", "cpa", "--project", "work", "--", "exec", "literal $(no shell)"]
        settings = self.root / "settings/cwr/client.json"
        settings.parent.mkdir(parents=True)
        settings.write_text(json.dumps({"cpa_urls": [CPA_URL]}))
        env = dict(os.environ, CODEX_HOME=str(codex_home), CWR_TEST_LITERAL="preserved", XDG_CONFIG_HOME=str(settings.parent.parent),
                   CWR_FIXTURE_CONFIG=json.dumps({"model_provider": "cpa", "model_providers": {"cpa": {"base_url": CPA_URL}}}))
        expected = {"args": [*self.prefix()[1:], "-c", 'model_providers.cpa.http_headers.X-CWR-Project="work"',
                             "exec", "literal $(no shell)"], "env": "preserved"}
        for raw in [None, b'model_provider = "cpa"\n', b'model_provider = "other"\n', b"invalid TOML"]:
            if raw is not None:
                config.write_bytes(raw)
            with self.subTest(config=raw):
                result = subprocess.run(argv, env=env, capture_output=True, text=True, check=True)
                self.assertEqual(json.loads(result.stdout), expected)
                self.assertEqual(result.stderr, "")
                if raw is not None:
                    self.assertEqual(config.read_bytes(), raw)
                else:
                    self.assertFalse(config.exists())
        direct = subprocess.run([str(fake), "exec", "literal $(no shell)"], env=env, capture_output=True, text=True, check=True)
        self.assertEqual(json.loads(direct.stdout), {"args": ["exec", "literal $(no shell)"], "env": "preserved"})
        self.assertEqual(profile.read_bytes(), b"not TOML")


class AutomaticRouting(LauncherFixture):
    def setUp(self):
        super().setUp()
        self.route_patch.stop()
        self.settings = self.root / "settings/cwr/client.json"
        self.fake = self.root / "fake-codex"
        self.fake.write_text("#!" + sys.executable + "\n" + CONFIG_READER + "\nprint(json.dumps(sys.argv[1:]))\n")
        self.fake.chmod(0o755)
        self.environment = {**os.environ, "XDG_CONFIG_HOME": str(self.settings.parent.parent)}

    def save(self, **settings):
        self.settings.parent.mkdir(parents=True, exist_ok=True)
        self.settings.write_text(json.dumps(settings))

    def launch(self, *arguments, config=None, requirements=None):
        env = dict(self.environment)
        if config is not None:
            env["CWR_FIXTURE_CONFIG"] = json.dumps(config)
        if requirements is not None:
            env["CWR_FIXTURE_REQUIREMENTS"] = json.dumps(requirements)
        return subprocess.run([sys.executable, str(ROOT / "scripts/cwr-codex.py"), "--directory", str(self.root),
                               "--codex", str(self.fake), *arguments], env=env, stdin=subprocess.DEVNULL,
                              capture_output=True, text=True, timeout=10)

    def test_default_without_registered_cpa_passes_through_without_probe_or_mapping(self):
        result = self.launch("--map", "/missing-map", "--", "resume", "previous-session")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout), ["-C", str(self.root), "resume", "previous-session"])
        self.assertFalse(self.settings.exists())
        with mock.patch.object(launcher, "read_route", side_effect=AssertionError("unnecessary probe")):
            self.assertIsNone(launcher.scheduling_route(self.args(mode="auto"), self.root, []))

    def test_trust_registration_normalizes_exact_url_and_preserves_mode_and_provider(self):
        self.save(mode="official", provider="old_proxy", future_setting=True)
        for _ in range(2):
            result = self.launch("--trust-cpa-url", "HTTP://LOCALHOST:80/v1/")
            self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(self.settings.read_text()), {"mode": "official", "provider": "old_proxy",
                         "future_setting": True, "cpa_urls": ["http://localhost/v1"]})
        self.assertEqual(self.settings.stat().st_mode & 0o777, 0o600)
        result = self.launch("--set-mode", "auto")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(self.settings.read_text())["mode"], "auto")
        result = self.launch("--status")
        self.assertIn("客户端模式：auto", result.stdout)
        self.assertIn("http://localhost/v1", result.stdout)

    def test_official_or_ambiguous_urls_cannot_be_registered_and_errors_do_not_echo_secrets(self):
        for url in ["https://api.openai.com/v1", "https://CHATGPT.com./backend-api/codex", "https://sub.chatgpt.com/v1",
                    "https://chat.com", "https://user:private-password@proxy.test/v1", "https://proxy.test/v1?key=private-secret",
                    "ftp://proxy.test/v1", "https://proxy.test/v1#secret", "https://proxy.test:invalid/v1",
                    "https://proxy.test\\@api.openai.com/v1", "https://%61pi.openai.com/v1", "https://proxy.test/v1\n"]:
            with self.subTest(url=url):
                result = self.launch("--trust-cpa-url", url)
                self.assertEqual(result.returncode, 2, result.stderr)
                self.assertNotIn("private-password", result.stderr)
                self.assertNotIn("private-secret", result.stderr)
                self.assertFalse(self.settings.exists())

    def test_only_detected_exact_approved_custom_provider_gets_headers_and_pinned_route(self):
        self.save(cpa_urls=[CPA_URL], provider="obsolete_name")
        result = self.launch("--", "--model", "chosen-model", "resume", "previous-session")
        self.assertEqual(result.returncode, 0, result.stderr)
        argv = json.loads(result.stdout)
        self.assertIn('model_provider="fixture_proxy"', argv)
        self.assertIn('model_providers.fixture_proxy.base_url="' + CPA_URL + '"', argv)
        self.assertEqual(sum("X-CWR-Project=" in value for value in argv), 1)
        self.assertEqual(argv[-4:], ["--model", "chosen-model", "resume", "previous-session"])
        self.assertNotIn("obsolete_name", result.stdout)

    def test_official_targets_unapproved_ports_and_paths_and_managed_routes_get_no_headers(self):
        self.save(cpa_urls=[CPA_URL])
        for provider, url, requirements in [
                ("openai", CPA_URL, None), ("fixture_proxy", "https://api.openai.com/v1", None),
                ("fixture_proxy", "https://chatgpt.com/backend-api/codex", None),
                ("fixture_proxy", "http://127.0.0.1:8318/v1", None), ("fixture_proxy", CPA_URL + "/other", None),
                ("fixture_proxy", CPA_URL, {"model_provider": "managed_proxy"}),
                ("fixture_proxy", CPA_URL, {"modelProviders": {"managed": {}}})]:
            with self.subTest(provider=provider, url=url, requirements=requirements):
                result = self.launch("--map", "/missing", config={"model_provider": provider,
                                     "model_providers": {provider: {"base_url": url}}}, requirements=requirements)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(json.loads(result.stdout), ["-C", str(self.root)])
                self.assertNotIn("X-CWR-", result.stdout)
        result = self.launch("--provider", "mismatch")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("X-CWR-", result.stdout)
        result = self.launch(config={"model_provider": "fixture_proxy", "model_providers": {"fixture_proxy": {
                             "base_url": CPA_URL, "model_catalog_url": "https://api.openai.com/v1/models"}}})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("X-CWR-", result.stdout)

    def test_profile_remote_and_config_probe_failure_pass_through_in_auto_and_stop_in_cpa(self):
        self.save(cpa_urls=[CPA_URL])
        for arguments in [["--profile", "proxy"], ["-pproxy"], ["--profile=proxy"],
                          ["--oss"], ["--remote", "ws://localhost:1234"]]:
            with self.subTest(arguments=arguments):
                result = self.launch("--", *arguments)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(json.loads(result.stdout), ["-C", str(self.root), *arguments])
                result = self.launch("--mode", "cpa", "--", *arguments)
                self.assertEqual(result.returncode, 2, result.stderr)
                self.assertEqual(result.stdout, "")
        for config in [{"model_provider": []}, {"model_provider": "missing"}, {"model_providers": []},
                       {"profile": "unsupported"}]:
            result = self.launch(config=config)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(json.loads(result.stdout), ["-C", str(self.root)])
        self.fake.write_text("#!" + sys.executable + "\nimport sys,json\nif 'app-server' in sys.argv:sys.exit(1)\nprint(json.dumps(sys.argv[1:]))\n")
        result = self.launch()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout), ["-C", str(self.root)])
        result = self.launch("--mode", "cpa")
        self.assertEqual(result.returncode, 2, result.stderr)

    def test_cpa_requires_registration_and_legacy_settings_do_not_bypass_it(self):
        self.save(mode="cpa", provider="cliproxyapi")
        result = self.launch()
        self.assertEqual(result.returncode, 2, result.stderr)
        self.assertIn("--trust-cpa-url", result.stderr)
        self.assertEqual(result.stdout, "")
        self.save(provider="cliproxyapi")
        result = self.launch()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout), ["-C", str(self.root)])

    def test_cpa_can_select_a_registered_definition_when_official_is_the_global_default(self):
        self.save(cpa_urls=[CPA_URL])
        config = {"model_provider": "openai", "model_providers": {"proxy": {"base_url": CPA_URL}}}
        result = self.launch("--mode", "cpa", config=config)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('model_provider="proxy"', json.loads(result.stdout))
        self.assertTrue(any("X-CWR-Project=" in value for value in json.loads(result.stdout)))
        result = self.launch(config=config)
        self.assertEqual(json.loads(result.stdout), ["-C", str(self.root)])
        result = self.launch("--mode", "cpa", "--", "-c", 'model_provider="openai"', config=config)
        self.assertEqual(result.returncode, 2, result.stderr)
        self.assertIn("冲突", result.stderr)

    def test_cpa_does_not_guess_between_multiple_inactive_providers(self):
        self.save(cpa_urls=[CPA_URL])
        config = {"model_provider": "openai", "model_providers": {
            "first": {"base_url": CPA_URL}, "second": {"base_url": CPA_URL}}}
        result = self.launch("--mode", "cpa", config=config)
        self.assertEqual(result.returncode, 2, result.stderr)
        self.assertIn("--provider", result.stderr)
        result = self.launch("--mode", "cpa", "--provider", "second", config=config)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('model_provider="second"', json.loads(result.stdout))
        result = self.launch("--mode", "cpa", "--provider", "second", config=config,
                             requirements={"model_provider": "managed"})
        self.assertEqual(result.returncode, 2, result.stderr)
        self.assertEqual(result.stdout, "")

    def test_probe_only_extracts_config_options_and_preserves_literal_prompts(self):
        arguments = ["--model", "-pmodel-value", "-c", 'model_provider="proxy"', "exec", "--",
                     "--profile", "literal-profile", "-c", "literal-prompt"]
        self.assertEqual(launcher.probe_options(arguments), ["-c", 'model_provider="proxy"'])
        self.assertEqual(launcher.probe_options(["--config=model_provider='proxy'", "--enable", "feature"]),
                         ["--config=model_provider='proxy'", "--enable", "feature"])

    def test_probe_timeout_and_invalid_protocol_responses_never_add_headers(self):
        self.save(cpa_urls=[CPA_URL])
        for response in ["[]", '{"id":1,"error":{"message":"private-error"}}']:
            self.fake.write_text("#!" + sys.executable + "\nimport json,sys\nif 'app-server' in sys.argv:\n sys.stdin.readline()\n print(" + repr(response) + ",flush=True)\n sys.exit(0)\nprint(json.dumps(sys.argv[1:]))\n")
            result = self.launch()
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(json.loads(result.stdout), ["-C", str(self.root)])
            self.assertNotIn("private-error", result.stderr)
        self.fake.write_text("#!" + sys.executable + "\nimport json,sys,time\nif 'app-server' in sys.argv:time.sleep(30)\nprint(json.dumps(sys.argv[1:]))\n")
        result = self.launch()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout), ["-C", str(self.root)])

    @unittest.skipUnless(shutil.which("codex"), "Real Codex CLI is optional")
    def test_real_codex_config_reader_resolves_cli_overrides_and_never_modifies_source_config(self):
        home = self.root / "codex-home"
        home.mkdir()
        config = home / "config.toml"
        original = 'model_provider="openai"\n[model_providers.proxy]\nname="Fixture"\nbase_url="' + CPA_URL + '"\nwire_api="responses"\n'
        config.write_text(original)
        env = {**os.environ, "CODEX_HOME": str(home)}
        with mock.patch.dict(os.environ, env):
            self.assertEqual(launcher.read_route(shutil.which("codex"), self.root, []), ("openai", None))
            self.assertEqual(launcher.read_route(shutil.which("codex"), self.root, [], cpa_urls=[CPA_URL]), ("proxy", CPA_URL))
            self.assertEqual(launcher.read_route(shutil.which("codex"), self.root, ["-c", 'model_provider="proxy"']), ("proxy", CPA_URL))
            with self.assertRaises(ValueError):
                launcher.read_route(shutil.which("codex"), self.root, ["-c", 'model_provider="proxy"', "-c", 'model_providers.proxy.base_url="https://api.openai.com/v1"'])
        self.assertEqual(config.read_text(), original)
        self.assertFalse((home / "auth.json").exists())

    @unittest.skipUnless(shutil.which("codex"), "Real Codex CLI is optional")
    def test_real_codex_http_requests_only_include_cwr_headers_for_registered_destination(self):
        captured = []

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):
                self.rfile.read(int(self.headers.get("Content-Length", "0")))
                captured.append({name.lower(): value for name, value in self.headers.items()})
                body = b'{"error":{"message":"offline header capture","type":"invalid_request_error"}}'
                self.send_response(400)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *_):
                pass

        try:
            server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        except PermissionError:
            self.skipTest("Loopback HTTP capture requires local socket access")
        worker = threading.Thread(target=server.serve_forever, daemon=True)
        worker.start()
        try:
            url = "http://127.0.0.1:" + str(server.server_port) + "/v1"
            home = self.root / "http-codex-home"
            home.mkdir()
            config = home / "config.toml"
            original = ('model_provider="proxy"\n[model_providers.proxy]\nname="Offline capture"\n'
                        'base_url="' + url + '"\nwire_api="responses"\nenv_key="CWR_FIXTURE_CPA_KEY"\n'
                        'requires_openai_auth=false\nrequest_max_retries=0\nstream_max_retries=0\n')
            config.write_text(original)
            auth = home / "auth.json"
            auth_original = '{"OPENAI_API_KEY":"unrelated-stored-fixture-key"}'
            auth.write_text(auth_original)
            environment = {**self.environment, "CODEX_HOME": str(home), "NO_PROXY": "127.0.0.1", "no_proxy": "127.0.0.1",
                           "CWR_FIXTURE_CPA_KEY": "independent-fixture-key"}
            for approved, override in ((False, False), (True, False), (True, True)):
                captured.clear()
                self.save(cpa_urls=[url] if approved else [])
                extra = ["-c", 'model_providers.proxy.base_url="' + url + '/other"'] if override else []
                result = subprocess.run([sys.executable, str(ROOT / "scripts/cwr-codex.py"), "--directory", str(self.root),
                                         "--codex", shutil.which("codex"), "--project", "offline-fixture", "--",
                                         "exec", *extra, "--skip-git-repo-check", "--model", "fixture-model", "offline fixture"],
                                        env=environment, cwd=self.root, stdin=subprocess.DEVNULL, capture_output=True,
                                        text=True, timeout=15)
                self.assertNotEqual(result.returncode, 0)
                self.assertTrue(captured, "No request reached local capture server: " + result.stderr[-1500:])
                self.assertEqual(any("x-cwr-project" in headers for headers in captured), approved and not override)
                if approved and not override:
                    self.assertTrue(all(headers.get("x-cwr-project") == "offline-fixture" for headers in captured))
                self.assertTrue(all("x-cwr-project-name" not in headers for headers in captured))
                self.assertTrue(all(headers.get("authorization") == "Bearer independent-fixture-key" for headers in captured))
            self.assertEqual(config.read_text(), original)
            self.assertEqual(auth.read_text(), auth_original)
        finally:
            server.shutdown()
            server.server_close()
            worker.join(timeout=2)


@unittest.skipIf(os.name == "nt", "POSIX terminal launches")
class WindowConnections(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.settings = self.root / "settings/cwr/client.json"
        self.settings.parent.mkdir(parents=True)
        self.settings.write_text(json.dumps({"mode": "auto", "cpa_urls": [CPA_URL], "future": True}))
        self.before = self.settings.read_bytes()
        self.fake = self.root / "codex"
        self.fake.write_text("#!" + sys.executable + "\n" + CONFIG_READER + RECORD_LAUNCH)
        self.fake.chmod(0o755)
        self.env = {**os.environ, "XDG_CONFIG_HOME": str(self.root / "settings"),
                    "OPENAI_API_KEY": "fixture-key", "CODEX_API_KEY": "fixture-key",
                    "CPA_API_KEY": "fixture-cpa-key",
                    "OPENAI_BASE_URL": "http://fixture.test/v1", "HTTPS_PROXY": "http://fixture-proxy:1234"}

    def terminal(self, name, *arguments, hold=False, extra_env=None):
        directory = self.root / name
        directory.mkdir(exist_ok=True)
        record = directory / "record.json"
        terminal = TerminalLaunch([sys.executable, str(ROOT / "scripts/cwr-codex.py"),
                                   "--codex", str(self.fake), *arguments], cwd=directory,
                                  env={**self.env, "CWR_FIXTURE_RECORD": str(record),
                                       "CWR_FIXTURE_HOLD": "1" if hold else "", **(extra_env or {})})
        self.addCleanup(terminal.close)
        return terminal, directory, record

    def test_simultaneous_windows_choose_independent_routes_and_keep_shared_settings(self):
        arguments = ["--", "--model", "chosen-model", "-c", "model_reasoning_effort=high",
                     "-c", "tui.animations=false", "resume", "previous-session"]
        official, first, first_record = self.terminal("official directory", *arguments, hold=True)
        cpa, second, second_record = self.terminal("CPA 目录", *arguments, hold=True)
        for window, directory in ((official, first), (cpa, second)):
            window.until("选择连接")
            self.assertIn(str(directory), window.transcript())
            self.assertIsNone(window.process.poll())
        official.send("1\n")
        official.until("FIXTURE_CODEX_STARTED")
        cpa.send("2\n")
        cpa.until("FIXTURE_CODEX_STARTED")
        self.assertIsNone(official.process.poll())
        self.assertIsNone(cpa.process.poll())
        for window, directory, record, mode in ((official, first, first_record, "official"),
                                                 (cpa, second, second_record, "cpa")):
            data = json.loads(record.read_text())
            argv = data["argv"]
            self.assertEqual(argv[:2], ["-C", str(directory)])
            self.assertEqual(argv[-len(arguments[1:]):], arguments[1:])
            self.assertEqual('model_provider="openai"' in argv, mode == "official")
            self.assertEqual(any("X-CWR-Project=" in value for value in argv), mode == "cpa")
            self.assertEqual("OPENAI_API_KEY" in data["environment"], mode == "cpa")
            self.assertEqual(data["environment"]["HTTPS_PROXY"], self.env["HTTPS_PROXY"])
            window.send("\n")
            self.assertEqual(window.finish(), 7, window.transcript())
        self.assertEqual(self.settings.read_bytes(), self.before)

    def test_invalid_selection_retries_and_native_directory_is_shown(self):
        requested = self.root / "requested"
        requested.mkdir()
        terminal, _, record = self.terminal("initial", "--", "-C", str(requested), "--model", "exec")
        terminal.until("选择连接")
        self.assertIn(str(requested), terminal.transcript())
        terminal.send("invalid\n\n")
        terminal.until("请输入 1")
        terminal.send("official\n")
        self.assertEqual(terminal.finish(), 7, terminal.transcript())
        self.assertEqual(json.loads(record.read_text())["argv"][-2:], ["--model", "exec"])

    def test_saved_mode_is_menu_default_but_selection_does_not_change_it(self):
        self.settings.write_text(json.dumps({"mode": "cpa", "cpa_urls": [CPA_URL]}))
        before = self.settings.read_bytes()
        for name, choice, expected in (("accept-default", "\n", "fixture_proxy"), ("override-default", "1\n", "openai")):
            terminal, _, record = self.terminal(name)
            terminal.until("回车选 2")
            terminal.send(choice)
            self.assertEqual(terminal.finish(), 7, terminal.transcript())
            self.assertIn('model_provider="' + expected + '"', json.loads(record.read_text())["argv"])
        self.assertEqual(self.settings.read_bytes(), before)

    def test_cancel_eof_and_interrupt_do_not_launch_or_probe_codex(self):
        for index, choice in enumerate(("q\n", "\x04", None)):
            terminal, _, record = self.terminal("cancel-" + str(index))
            terminal.until("选择连接")
            if choice is None:
                terminal.process.send_signal(signal.SIGINT)
            else:
                terminal.send(choice)
            self.assertEqual(terminal.finish(), 130, terminal.transcript())
            self.assertFalse(record.exists())
            self.assertNotIn("Traceback", terminal.transcript())
        self.assertEqual(self.settings.read_bytes(), self.before)

    def test_explicit_mode_and_noninteractive_commands_skip_menu_even_with_terminal(self):
        for index, arguments in enumerate((("--mode", "official"), ("--mode", "cpa"), ("--mode", "auto"),
                                            ("--", "exec", "prompt"), ("--", "features", "list"),
                                            ("--", "--help"), ("--", "login", "status"))):
            terminal, _, _ = self.terminal("direct-" + str(index), *arguments)
            self.assertEqual(terminal.finish(), 7, terminal.transcript())
            self.assertNotIn("选择连接", terminal.transcript())

    def test_nonterminal_stdin_is_passed_to_codex_without_being_consumed(self):
        self.fake.write_text("#!" + sys.executable + "\n" + CONFIG_READER + "\nprint(sys.stdin.read(),end='')\n")
        result = subprocess.run([sys.executable, str(ROOT / "scripts/cwr-codex.py"), "--codex", str(self.fake)],
                                cwd=self.root, env=self.env, input="pipeline input\n2\n", capture_output=True,
                                text=True, timeout=10)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, "pipeline input\n2\n")
        self.assertNotIn("选择连接", result.stderr)

    def test_pasted_second_line_is_left_for_the_child_terminal(self):
        self.fake.write_text("#!" + sys.executable + "\n" + CONFIG_READER +
                             "\nprint('CHILD_INPUT=' + repr(sys.stdin.readline()),flush=True)\n")
        terminal, _, _ = self.terminal("pasted-input")
        terminal.until("选择连接")
        terminal.send("1\nfor the child\n")
        self.assertEqual(terminal.finish(), 0, terminal.transcript())
        self.assertIn("CHILD_INPUT='for the child\\n'", terminal.transcript())

    def test_native_command_detection_skips_option_values_and_literal_prompt_flags(self):
        for arguments in ([], ["resume", "previous"], ["fork", "--last"], ["--model", "exec"],
                          ["-c", "--help", "resume", "previous"], ["--", "exec", "--help"]):
            self.assertTrue(launcher.interactive_session(arguments), arguments)
        for arguments in (["exec", "prompt"], ["-m", "chosen-model", "review"], ["--version"],
                          ["resume", "--help"], ["-c", "key=value", "app-server"]):
            self.assertFalse(launcher.interactive_session(arguments), arguments)


class OfficialMode(LauncherFixture):
    def setUp(self):
        super().setUp()
        self.settings = self.root / "settings/cwr/client.json"
        self.fake = self.root / "fake-codex"
        self.fake.write_text("#!" + sys.executable + "\n" + CONFIG_READER + '''
import json,os,sys
names=['OPENAI_BASE_URL','CHATGPT_BASE_URL','OPENAI_API_KEY','CODEX_API_KEY','HTTPS_PROXY','CODEX_CA_CERTIFICATE','CWR_FIXTURE_LITERAL']
print(json.dumps({'argv':sys.argv[1:],'environment':{name:os.environ[name] for name in names if name in os.environ}}))
sys.exit(int(os.environ.get('CWR_FIXTURE_EXIT','0')))
''', encoding="utf-8")
        self.fake.chmod(0o755)
        self.environment = {**os.environ, "XDG_CONFIG_HOME": str(self.root / "settings"),
                            "OPENAI_BASE_URL": "http://127.0.0.1:9/v1", "CHATGPT_BASE_URL": "http://127.0.0.1:9/",
                            "OPENAI_API_KEY": "fixture-key", "CODEX_API_KEY": "fixture-key",
                            "HTTPS_PROXY": "http://fixture-proxy:1234", "CODEX_CA_CERTIFICATE": "/fixture/ca.pem",
                            "CWR_FIXTURE_LITERAL": "preserve $(literal)"}

    def launch(self, *arguments, environment=None):
        return subprocess.run([sys.executable, str(ROOT / "scripts/cwr-codex.py"), "--directory", str(self.root),
                               "--codex", str(self.fake), *arguments],
                              env={**self.environment, **(environment or {})}, stdin=subprocess.DEVNULL,
                              capture_output=True, text=True, timeout=15)

    def test_official_skips_project_mapping_and_codex_config_preserves_native_arguments_and_exit_code(self):
        arguments = ["--profile", "proxy", "--model", "chosen-model", "-c", "model_reasoning_effort=high",
                     "-c", "tui.animations=false", "resume", "previous-session"]
        with mock.patch.object(Path, "open", side_effect=AssertionError("read configuration")), \
                mock.patch.object(launcher, "directory_project", side_effect=AssertionError("read project")):
            command = launcher.command(self.args(mode="official", provider=None, project="invalid project", arguments=["--", *arguments]))
        self.assertEqual(command[:3], ["codex", "-C", str(self.root)])
        self.assertEqual(command[-len(arguments):], arguments)
        self.assertIn('model_provider="openai"', command)
        self.assertIn('forced_login_method="chatgpt"', command)
        self.assertIn('openai_base_url="https://chatgpt.com/backend-api/codex"', command)
        self.assertIn('chatgpt_base_url="https://chatgpt.com/backend-api/"', command)
        self.assertFalse(any("model_providers." in item or "X-CWR-Project" in item for item in command))
        result = self.launch("--mode", "official", "--map", str(self.root / "missing.json"), "--", *arguments,
                             environment={"CWR_FIXTURE_EXIT": "7"})
        self.assertEqual(result.returncode, 7, result.stderr)
        data = json.loads(result.stdout)
        self.assertEqual(data["argv"], command[1:])
        self.assertEqual(data["environment"], {"HTTPS_PROXY": "http://fixture-proxy:1234",
                         "CODEX_CA_CERTIFICATE": "/fixture/ca.pem", "CWR_FIXTURE_LITERAL": "preserve $(literal)"})
        self.assertFalse(self.settings.exists())

    def test_mode_is_persistent_and_switching_back_preserves_provider_and_other_settings(self):
        self.settings.parent.mkdir(parents=True)
        self.settings.write_text(json.dumps({"provider": "fixture_proxy", "future_setting": True, "cpa_urls": [CPA_URL]}), encoding="utf-8")
        result = self.launch("--set-mode", "official")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn('"argv"', result.stdout)
        saved = self.settings.read_bytes()
        self.assertEqual(json.loads(saved), {"provider": "fixture_proxy", "future_setting": True, "mode": "official", "cpa_urls": [CPA_URL]})
        self.assertEqual(self.settings.stat().st_mode & 0o777, 0o600)
        result = self.launch("--status")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("客户端模式：official", result.stdout)
        self.assertIn("CPA 提供商：fixture_proxy", result.stdout)
        self.assertNotIn("future_setting", result.stdout)
        result = self.launch("--", "login", "status")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('model_provider="openai"', json.loads(result.stdout)["argv"])
        result = self.launch("--mode", "cpa", "--", "exec", "literal $(no shell)")
        self.assertEqual(result.returncode, 0, result.stderr)
        data = json.loads(result.stdout)
        self.assertTrue(any("model_providers.fixture_proxy.http_headers.X-CWR-Project=" in item for item in data["argv"]))
        self.assertIn("OPENAI_API_KEY", data["environment"])
        self.assertEqual(self.settings.read_bytes(), saved)
        result = self.launch("--set-mode", "cpa")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(self.settings.read_text()), {"provider": "fixture_proxy", "future_setting": True, "mode": "cpa", "cpa_urls": [CPA_URL]})
        result = self.launch()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(any("model_providers.fixture_proxy.http_headers.X-CWR-Project=" in item for item in json.loads(result.stdout)["argv"]))
        self.assertFalse(list(self.settings.parent.glob(".client-*")))

    def test_settings_updates_preserve_saved_mode_and_unknown_fields(self):
        self.settings.parent.mkdir(parents=True)
        self.settings.write_text('{"mode":"cpa","future_setting":true}', encoding="utf-8")
        with mock.patch.dict(os.environ, self.environment):
            launcher.save_settings(provider="repaired")
        self.assertEqual(json.loads(self.settings.read_text()), {"mode": "cpa", "future_setting": True, "provider": "repaired"})

    def test_invalid_settings_never_start_codex_or_overwrite_original(self):
        self.settings.parent.mkdir(parents=True)
        for content in [b'private invalid JSON', b'{"mode":"unknown"}', b'{"mode":null}',
                        b'{"mode":"official","provider":"bad.provider"}', b'[]', b' ' * 4097]:
            self.settings.write_bytes(content)
            for arguments in [(), ("--set-mode", "official"), ("--status",)]:
                with self.subTest(content=content[:80], arguments=arguments):
                    result = self.launch(*arguments)
                    self.assertEqual(result.returncode, 2, result.stderr)
                    self.assertIn("无法读取客户端设置", result.stderr)
                    self.assertNotIn("private invalid JSON", result.stderr)
                    self.assertEqual(self.settings.read_bytes(), content)
                    self.assertEqual(result.stdout, "")

    def test_conflicting_route_flags_are_rejected_but_option_values_and_literal_prompts_are_preserved(self):
        cases = [["-c", 'model_provider="proxy"'], ["--config=openai_base_url='http://proxy'"],
                 ["-c=model_provider='proxy'"],
                 ["-cchatgpt_base_url='http://proxy'"], ["-c", "forced_login_method='api'"],
                 ["-c", '"model_provider"="proxy"'], ["-c", "model_providers.openai.base_url='http://proxy'"],
                 ["--oss"], ["--local-provider=ollama"], ["--remote", "ws://localhost:1234"]]
        for arguments in cases:
            with self.subTest(arguments=arguments):
                result = self.launch("--mode", "official", "--", *arguments)
                self.assertEqual(result.returncode, 2, result.stderr)
                self.assertEqual(result.stdout, "")
        arguments = ["--model", "--remote", "exec", "--", "-c", 'model_provider="prompt text"', "--oss"]
        command = launcher.command(self.args(mode="official", arguments=["--", *arguments]))
        self.assertEqual(command[-len(arguments):], arguments)
        for arguments in [("--mode", "official", "--provider", "proxy"),
                          ("--set-mode", "official", "--", "exec", "prompt"),
                          ("--set-mode", "official", "--mode", "cpa"),
                          ("--status", "--set-mode", "official")]:
            with self.subTest(arguments=arguments):
                result = self.launch(*arguments)
                self.assertEqual(result.returncode, 2, result.stderr)
                self.assertFalse(self.settings.exists())

    @unittest.skipUnless(shutil.which("codex"), "Real Codex CLI is optional")
    def test_real_codex_accepts_official_overrides_on_top_of_proxy_config_without_rewriting_it(self):
        home = self.root / "codex-home"
        home.mkdir()
        config = home / "config.toml"
        original = ('model_provider="cliproxyapi"\nforced_login_method="api"\n'
                    'openai_base_url="http://127.0.0.1:9/v1"\nchatgpt_base_url="http://127.0.0.1:9/"\n'
                    '[model_providers.cliproxyapi]\nname="Fixture proxy"\nbase_url="http://127.0.0.1:9/v1"\nwire_api="responses"\n')
        config.write_text(original, encoding="utf-8")
        result = subprocess.run([sys.executable, str(ROOT / "scripts/cwr-codex.py"), "--directory", str(self.root),
                                 "--codex", shutil.which("codex"), "--mode", "official", "--", "features", "list"],
                                env={**self.environment, "CODEX_HOME": str(home)}, cwd=self.root,
                                capture_output=True, text=True, timeout=15)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(config.read_text(), original)
        self.assertFalse((home / "auth.json").exists())
