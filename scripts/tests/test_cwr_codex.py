import argparse
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("cwr_codex", ROOT / "scripts/cwr-codex.py")
launcher = importlib.util.module_from_spec(spec)
spec.loader.exec_module(launcher)


class LauncherFixture(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)

    def args(self, **overrides):
        values = dict(directory=str(self.root), project="work", provider="cpa",
                      codex="codex", arguments=[], map=str(self.root / "missing-map.json"))
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
        for arguments in [["--", "--cd=/another/project"], ["-C", "/another/project"]]:
            with self.subTest(arguments=arguments), self.assertRaises(ValueError):
                launcher.command(self.args(arguments=arguments))

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
    def test_header_is_added_without_opening_any_codex_configuration(self):
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
                self.assertEqual(launcher.command(self.args()), ["codex", "-C", str(self.root), "-c",
                                 'model_providers.cpa.http_headers.X-CWR-Project="work"'])
            if content is not None:
                self.assertEqual(config.read_bytes(), content)

    def test_custom_provider_selects_header_namespace_without_switching_provider(self):
        command = launcher.command(self.args(provider="my_proxy"))
        self.assertIn('model_providers.my_proxy.http_headers.X-CWR-Project="work"', command)
        self.assertFalse(any(value.startswith("model_provider=") for value in command))

    def test_codex_options_are_forwarded_without_provider_or_profile_detection(self):
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
                                 ["codex", "-C", str(self.root), "-c",
                                  'model_providers.cpa.http_headers.X-CWR-Project="work"', *arguments])

    @unittest.skipIf(os.name == "nt", "Executable fixture uses a POSIX shebang")
    def test_script_launch_adds_header_and_direct_launch_stays_unchanged(self):
        fake = self.root / "fake-codex"
        fake.write_text("#!" + sys.executable + "\nimport json,os,sys\nprint(json.dumps({'args':sys.argv[1:],'env':os.environ['CWR_TEST_LITERAL']}))\n", encoding="utf-8")
        fake.chmod(0o755)
        codex_home = self.root / "codex-home"
        codex_home.mkdir()
        config = codex_home / "config.toml"
        profile = codex_home / "other.config.toml"
        profile.write_bytes(b"not TOML")
        argv = [sys.executable, str(ROOT / "scripts/cwr-codex.py"), "--directory", str(self.root),
                "--codex", str(fake), "--provider", "cpa", "--project", "work", "--", "exec", "literal $(no shell)"]
        env = dict(os.environ, CODEX_HOME=str(codex_home), CWR_TEST_LITERAL="preserved")
        expected = {"args": ["-C", str(self.root), "-c", 'model_providers.cpa.http_headers.X-CWR-Project="work"',
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


class RememberedProvider(LauncherFixture):
    def setUp(self):
        super().setUp()
        self.fake = self.root / "fake-codex"
        self.attempts = self.root / "attempts.jsonl"
        self.settings = self.root / "settings" / "cwr" / "client.json"
        self.fake.write_text("#!" + sys.executable + '''
import json,os,sys
key=next(item for item in sys.argv[1:] if '.http_headers.X-CWR-Project=' in item)
provider=key.split('.')[1]
with open(os.environ['CWR_FIXTURE_ATTEMPTS'],'a') as stream:
    stream.write(json.dumps({'provider':provider,'argv':sys.argv[1:]})+'\\n')
if os.environ.get('CWR_FIXTURE_ERROR'):
    print(os.environ['CWR_FIXTURE_ERROR'],file=sys.stderr)
    sys.exit(1)
if provider!=os.environ['CWR_FIXTURE_PROVIDER']:
    print('Error: model_providers.'+provider+': provider name must not be empty\\nin `model_providers`',file=sys.stderr)
    sys.exit(1)
print('Codex fixture started')
''', encoding="utf-8")
        self.fake.chmod(0o755)
        self.environment = dict(os.environ, XDG_CONFIG_HOME=str(self.root / "settings"),
                                CWR_FIXTURE_ATTEMPTS=str(self.attempts), CWR_FIXTURE_PROVIDER="my_proxy")
        self.argv = [sys.executable, str(ROOT / "scripts/cwr-codex.py"), "--directory", str(self.root),
                     "--project", "work", "--codex", str(self.fake)]

    def launch(self, answers=None, extra=(), environment=None):
        environment = {**self.environment, **(environment or {})}
        if answers is None:
            return subprocess.run([*self.argv, *extra], env=environment, stdin=subprocess.DEVNULL,
                                  capture_output=True, text=True, timeout=10)
        master, slave = os.openpty()
        try:
            with subprocess.Popen([*self.argv, *extra], env=environment, stdin=slave,
                                  stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True) as process:
                os.write(master, answers.encode())
                stdout, stderr = process.communicate(timeout=10)
                return subprocess.CompletedProcess(process.args, process.returncode, stdout, stderr)
        finally:
            os.close(master)
            os.close(slave)

    def test_no_arguments_use_cliproxyapi_without_creating_client_settings(self):
        result = self.launch(environment={"CWR_FIXTURE_PROVIDER": "cliproxyapi"})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(self.attempts.read_text())["provider"], "cliproxyapi")
        self.assertFalse(self.settings.exists())

    @unittest.skipIf(os.name == "nt", "PTY input fixture requires POSIX")
    def test_provider_startup_error_prompts_saves_and_next_launch_needs_no_arguments(self):
        codex_config = self.root / "codex-config.toml"
        codex_config.write_text("preserve original config", encoding="utf-8")
        result = self.launch("bad id\nmy_proxy\n")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("请重新输入", result.stderr)
        self.assertEqual(json.loads(self.settings.read_text()), {"provider": "my_proxy"})
        self.assertEqual(self.settings.stat().st_mode & 0o777, 0o600)
        self.assertEqual(codex_config.read_text(), "preserve original config")
        attempts = [json.loads(line)["provider"] for line in self.attempts.read_text().splitlines()]
        self.assertEqual(attempts, ["cliproxyapi", "my_proxy"])
        result = self.launch()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("提供商 ID（", result.stderr)
        self.assertEqual(json.loads(self.attempts.read_text().splitlines()[-1])["provider"], "my_proxy")
        self.assertFalse(list(self.settings.parent.glob(".client-*")))

    @unittest.skipIf(os.name == "nt", "PTY input fixture requires POSIX")
    def test_stale_remembered_provider_can_be_corrected_and_blank_cancels_without_changing_it(self):
        self.settings.parent.mkdir(parents=True)
        self.settings.write_text('{"provider":"old_proxy"}', encoding="utf-8")
        original = self.settings.read_bytes()
        result = self.launch("\n")
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertEqual(self.settings.read_bytes(), original)
        result = self.launch("my_proxy\n")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(self.settings.read_text())["provider"], "my_proxy")

    def test_noninteractive_failure_and_unrelated_codex_errors_never_prompt_or_save(self):
        result = self.launch()
        self.assertEqual(result.returncode, 1)
        self.assertIn("--provider", result.stderr)
        self.assertFalse(self.settings.exists())
        for error in ["HTTP 401 authentication failed", "model_providers.other: provider name must not be empty", "model_providers.cliproxyapi_extra: provider name must not be empty"]:
            result = self.launch(environment={"CWR_FIXTURE_ERROR": error})
            self.assertEqual(result.returncode, 1)
            self.assertNotIn("提供商 ID（", result.stderr)
            self.assertFalse(self.settings.exists())

    def test_explicit_provider_overrides_saved_value_only_for_this_invocation(self):
        self.settings.parent.mkdir(parents=True)
        self.settings.write_text('{"provider":"remembered"}', encoding="utf-8")
        result = self.launch(extra=("--provider", "my_proxy"))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(self.settings.read_text())["provider"], "remembered")

    def test_invalid_client_settings_fall_back_without_exposing_contents(self):
        self.settings.parent.mkdir(parents=True)
        self.settings.write_text('private-do-not-print-invalid-json', encoding="utf-8")
        result = self.launch(environment={"CWR_FIXTURE_PROVIDER": "cliproxyapi"})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("private-do-not-print", result.stderr)

    @unittest.skipUnless(shutil.which("codex"), "Real Codex CLI is optional")
    def test_real_codex_accepts_existing_cliproxyapi_definition_without_creating_cpa(self):
        home = self.root / "codex-home"
        home.mkdir()
        config = home / "config.toml"
        original = 'model_provider="cliproxyapi"\n[model_providers.cliproxyapi]\nname="Fixture proxy"\nbase_url="http://127.0.0.1:9/v1"\nwire_api="responses"\n'
        config.write_text(original, encoding="utf-8")
        result = subprocess.run([sys.executable, str(ROOT / "scripts/cwr-codex.py"), "--directory", str(self.root),
                                 "--codex", shutil.which("codex"), "--project", "work", "--", "features", "list"],
                                env={**self.environment, "CODEX_HOME": str(home)}, cwd=self.root,
                                capture_output=True, text=True, timeout=15)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(config.read_text(), original)

    @unittest.skipIf(os.name == "nt", "PTY input fixture requires POSIX")
    @unittest.skipUnless(shutil.which("codex"), "Real Codex CLI is optional")
    def test_real_codex_provider_error_can_be_corrected_and_remembered(self):
        home = self.root / "codex-home"
        home.mkdir()
        config = home / "config.toml"
        original = 'model_provider="my_proxy"\n[model_providers.my_proxy]\nname="Fixture proxy"\nbase_url="http://127.0.0.1:9/v1"\nwire_api="responses"\n'
        config.write_text(original, encoding="utf-8")
        self.argv = [sys.executable, str(ROOT / "scripts/cwr-codex.py"), "--directory", str(self.root),
                     "--codex", shutil.which("codex"), "--project", "work", "--", "features", "list"]
        result = self.launch("my_proxy\n", environment={"CODEX_HOME": str(home)})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("provider name must not be empty", result.stderr)
        self.assertEqual(json.loads(self.settings.read_text()), {"provider": "my_proxy"})
        result = self.launch(environment={"CODEX_HOME": str(home)})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("provider name must not be empty", result.stderr)
        self.assertEqual(config.read_text(), original)
