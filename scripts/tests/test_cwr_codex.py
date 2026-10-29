import argparse
import importlib.util
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
