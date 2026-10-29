import argparse
import importlib.util
from pathlib import Path
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("cwr_codex", ROOT / "scripts/cwr-codex.py")
launcher = importlib.util.module_from_spec(spec)
spec.loader.exec_module(launcher)


class DirectoryRouting(unittest.TestCase):
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
