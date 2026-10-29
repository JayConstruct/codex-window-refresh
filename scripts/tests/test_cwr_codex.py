import argparse
import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("cwr_codex", ROOT / "scripts/cwr-codex.py")
launcher = importlib.util.module_from_spec(spec)
spec.loader.exec_module(launcher)


class ConfigFixture(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.codex_home = self.root / "codex-home"
        self.codex_home.mkdir()
        self.config = self.codex_home / "config.toml"
        self.config.write_text('model_provider = "cpa"\n', encoding="utf-8")
        self.environment = mock.patch.dict(os.environ, {"CODEX_HOME": str(self.codex_home)})
        self.environment.start()
        self.addCleanup(self.environment.stop)

    def args(self, **overrides):
        values = dict(directory=str(self.root), project="work", provider="cpa",
                      codex="codex", arguments=[], map=str(self.root / "missing-map.json"))
        values.update(overrides)
        return argparse.Namespace(**values)


class DirectoryRouting(ConfigFixture):
    @unittest.skipIf(launcher.tomllib is None, "Provider tests need Python 3.11+ or tomli")
    def test_longest_canonical_directory_match_and_safe_exec_arguments(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            nested = root / "nested"
            nested.mkdir()
            alias = root / "alias"
            alias.symlink_to(nested, target_is_directory=True)
            mappings = [{"directory": str(root), "project": "root"}, {"directory": str(nested), "project": "nested"}]
            self.assertEqual(launcher.project_for(alias, mappings), "nested")
            args = argparse.Namespace(directory=str(alias), project="nested", provider="cpa", codex="codex", arguments=["--", "exec", "a;$(echo literal)"], map="")
            command = launcher.command(args)
            self.assertIn('model_providers.cpa.http_headers.X-CWR-Project="nested"', command)
            self.assertEqual(command[-1], "a;$(echo literal)")
            self.assertEqual(command[2], str(nested))
            args.arguments = ["--", "--cd=/another/project"]
            with self.assertRaises(ValueError):
                launcher.command(args)

    def test_missing_and_duplicate_directory_mappings_are_rejected(self):
        with self.assertRaises(ValueError):
            launcher.project_for("/tmp", [])
        with self.assertRaises(ValueError):
            launcher.project_for("/tmp", [{"directory": "/tmp", "project": "a"}, {"directory": "/tmp", "project": "b"}])

    @unittest.skipIf(launcher.tomllib is None, "Provider tests need Python 3.11+ or tomli")
    def test_exported_directory_map_launches_nested_client_project(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            nested = root / "repo" / "subdirectory"
            nested.mkdir(parents=True)
            mapping = root / "projects.json"
            mapping.write_text(json.dumps({"projects": [{"directory": str(root), "project": "root"}, {"directory": str(root / "repo"), "project": "repo"}]}))
            args = argparse.Namespace(directory=str(nested), project=None, provider="cpa", codex="codex", arguments=[], map=str(mapping))
            command = launcher.command(args)
            self.assertIn('model_providers.cpa.http_headers.X-CWR-Project="repo"', command)
            self.assertEqual(command[2], str(nested))

    @unittest.skipIf(os.name == 'nt', 'POSIX client check')
    def test_foreign_windows_roots_do_not_break_a_shared_client_map(self):
        with tempfile.TemporaryDirectory() as directory:
            mappings = [{"directory": "C:\\Work\\Repo", "project": "windows"}, {"directory": directory, "project": "local"}]
            self.assertEqual(launcher.project_for(directory, mappings), "local")
            self.assertEqual(launcher.project_for(directory, [{"directory": "//" + directory.lstrip("/"), "project": "local"}]), "local")
            with self.assertRaises(ValueError):
                launcher.project_for(directory, [{"directory": "C:relative", "project": "invalid"}])


@unittest.skipIf(launcher.tomllib is None, "Provider tests need Python 3.11+ or tomli")
class ProviderGate(ConfigFixture):
    def test_only_matching_file_provider_adds_header_without_selecting_provider(self):
        original = self.config.read_bytes()
        args = self.args(arguments=["--", "exec", "literal;$(echo stays-literal)"])
        command = launcher.command(args)
        self.assertEqual(command, ["codex", "-C", str(self.root), "-c",
                                  'model_providers.cpa.http_headers.X-CWR-Project="work"',
                                  "exec", "literal;$(echo stays-literal)"])
        self.assertEqual(self.config.read_bytes(), original)
        self.assertFalse(any(value.startswith("model_provider=") for value in command))
        self.config.write_text('model_provider = "other"\n', encoding="utf-8")
        self.assertEqual(launcher.command(args), ["codex", "-C", str(self.root), *args.arguments[1:]])

    def test_other_or_missing_provider_does_not_read_map_even_for_explicit_project(self):
        for config in ['model_provider = "other"\n', "", '[model_providers.cpa]\nname = "CPA"\n']:
            self.config.write_text(config, encoding="utf-8")
            for project in [None, "work"]:
                with self.subTest(config=config, project=project), mock.patch.object(launcher, "project_for", side_effect=AssertionError("read map")):
                    args = self.args(project=project, arguments=["--", "exec", "--", "-c", "model_provider=cpa"])
                    self.assertEqual(launcher.command(args), ["codex", "-C", str(self.root), *args.arguments[1:]])
        self.config.unlink()
        self.assertEqual(launcher.command(self.args(project=None)), ["codex", "-C", str(self.root)])
        self.assertEqual(launcher.command(self.args(provider="openai", project=None)), ["codex", "-C", str(self.root)])

    def test_user_filled_provider_id_is_an_exact_match_condition(self):
        self.config.write_text('model_provider = "my_proxy"\n', encoding="utf-8")
        self.assertEqual(launcher.command(self.args(project=None)), ["codex", "-C", str(self.root)])
        self.assertIn('model_providers.my_proxy.http_headers.X-CWR-Project="work"',
                      launcher.command(self.args(provider="my_proxy")))
        self.assertEqual(launcher.command(self.args(provider="MY_PROXY", project=None)), ["codex", "-C", str(self.root)])

    def test_same_file_provider_with_cli_switches_skips_routing_and_preserves_arguments(self):
        cases = [
            ["exec", "-c", 'model_provider="other"', "prompt"],
            ["--config=model_provider=other", "exec", "prompt"],
            ["-cmodel_provider='other'", "exec", "prompt"],
            ["--oss", "--local-provider", "ollama"],
            ["--remote", "ws://localhost:1234"],
            ["-c", "model_provider=42"],
            ["-c", "model_provider.nested=cpa"],
            ["-c"],
        ]
        for arguments in cases:
            with self.subTest(arguments=arguments):
                self.assertEqual(launcher.command(self.args(project=None, arguments=["--", *arguments])),
                                 ["codex", "-C", str(self.root), *arguments])

    def test_cli_override_cannot_opt_an_unrelated_file_provider_into_routing(self):
        self.config.write_text('model_provider = "other"\n', encoding="utf-8")
        arguments = ["--config", 'model_provider="cpa"', "exec", "prompt"]
        self.assertEqual(launcher.command(self.args(project=None, arguments=arguments)),
                         ["codex", "-C", str(self.root), *arguments])

    def test_override_order_unquoted_strings_and_prompt_delimiter(self):
        for arguments in [
            ["-c", 'model_provider="other"', "--config=model_provider=cpa"],
            ["-c", '"model_provider" = "cpa"'],
            ["--config=model_reasoning_effort=high", "exec", "--", "--oss"],
            ["-c", 'model="literal --profile=other"', "exec", "prompt"],
        ]:
            with self.subTest(arguments=arguments):
                command = launcher.command(self.args(arguments=arguments))
                self.assertIn('model_providers.cpa.http_headers.X-CWR-Project="work"', command)
                self.assertEqual(command[-len(arguments):], arguments)

    def test_selected_profile_can_enable_or_disable_routing_without_being_overridden(self):
        profile = self.codex_home / "other.config.toml"
        profile.write_text('model_provider = "other"\n', encoding="utf-8")
        for options in [["--profile", "other"], ["--profile=other"], ["-pother"]]:
            with self.subTest(options=options):
                self.assertEqual(launcher.command(self.args(project=None, arguments=options)),
                                 ["codex", "-C", str(self.root), *options])
        self.config.write_text('model_provider = "other"\n', encoding="utf-8")
        (self.codex_home / "routing.config.toml").write_text('model_provider = "cpa"\n', encoding="utf-8")
        command = launcher.command(self.args(arguments=["-p", "routing"]))
        self.assertIn('model_providers.cpa.http_headers.X-CWR-Project="work"', command)
        self.assertEqual(command[-2:], ["-p", "routing"])
        self.config.write_text('model_provider = "cpa"\n', encoding="utf-8")
        (self.codex_home / "style.config.toml").write_text('model_reasoning_effort = "high"\n', encoding="utf-8")
        self.assertIn('model_providers.cpa.http_headers.X-CWR-Project="work"',
                      launcher.command(self.args(arguments=["--profile", "style"])))

    def test_ambiguous_legacy_profiles_are_not_assumed_to_match(self):
        self.config.write_text('model_provider = "cpa"\nprofile = "other"\n[profiles.other]\nmodel_provider = "other"\n', encoding="utf-8")
        self.assertEqual(launcher.command(self.args(project=None)), ["codex", "-C", str(self.root)])
        with contextlib.redirect_stderr(io.StringIO()):
            command = launcher.command(self.args(project=None, arguments=["-p", "missing"]))
        self.assertEqual(command[-2:], ["-p", "missing"])
        self.assertNotIn("-c", command)

    def test_invalid_config_and_missing_parser_pass_through_without_secrets_in_diagnostics(self):
        for raw in [b'model_provider = "cpa"\nsecret = "do-not-print" broken', b'\xff']:
            self.config.write_bytes(raw)
            errors = io.StringIO()
            with contextlib.redirect_stderr(errors):
                command = launcher.command(self.args(project=None))
            self.assertEqual(command, ["codex", "-C", str(self.root)])
            self.assertIn("跳过项目调度", errors.getvalue())
            self.assertNotIn("do-not-print", errors.getvalue())
        self.config.write_text('model_provider = "cpa"\n', encoding="utf-8")
        errors = io.StringIO()
        with mock.patch.object(launcher, "tomllib", None), contextlib.redirect_stderr(errors):
            self.assertEqual(launcher.command(self.args(project=None)), ["codex", "-C", str(self.root)])
        self.assertIn("Python 3.11+", errors.getvalue())

    def test_default_config_home_and_utf8_toml_structure(self):
        self.config.write_text('# model_provider = "wrong"\n"model_provider" = \'cpa\' # comment\n[model_providers.other]\nmodel_provider = "other"\nname = "其他服务"\n', encoding="utf-8")
        with mock.patch.dict(os.environ, {"CODEX_HOME": ""}), mock.patch.object(launcher.Path, "expanduser", return_value=self.codex_home):
            self.assertTrue(launcher.routing_enabled("cpa", []))
        self.config.write_text('model_provider = "cpa"\n[model_providers.other]\nmodel_provider = "other"\n', encoding="utf-8")
        self.assertTrue(launcher.routing_enabled("cpa", []))

    def test_matching_provider_still_requires_valid_project_mapping(self):
        with self.assertRaises(FileNotFoundError):
            launcher.command(self.args(project=None))
        with self.assertRaisesRegex(ValueError, "项目标识"):
            launcher.command(self.args(project="bad project"))

    @unittest.skipIf(os.name == "nt", "Executable fixture uses a POSIX shebang")
    def test_real_launch_follows_config_edits_without_changing_files_or_environment(self):
        fake = self.root / "fake-codex"
        fake.write_text("#!" + sys.executable + "\nimport json,os,sys\nprint(json.dumps({'args':sys.argv[1:],'env':os.environ['CWR_TEST_LITERAL']}))\n", encoding="utf-8")
        fake.chmod(0o755)
        argv = [sys.executable, str(ROOT / "scripts/cwr-codex.py"), "--directory", str(self.root),
                "--codex", str(fake), "--provider", "cpa", "--project", "work", "--", "exec", "literal $(no shell)"]
        env = dict(os.environ, CWR_TEST_LITERAL="preserved")
        original = self.config.read_bytes()
        first = subprocess.run(argv, env=env, capture_output=True, text=True, check=True)
        self.assertIn('model_providers.cpa.http_headers.X-CWR-Project="work"', json.loads(first.stdout)["args"])
        self.assertEqual(self.config.read_bytes(), original)
        self.config.write_text('model_provider = "other"\n', encoding="utf-8")
        second = subprocess.run(argv, env=env, capture_output=True, text=True, check=True)
        self.assertEqual(json.loads(second.stdout), {"args": ["-C", str(self.root), "exec", "literal $(no shell)"], "env": "preserved"})
        self.assertEqual(self.config.read_text(encoding="utf-8"), 'model_provider = "other"\n')
