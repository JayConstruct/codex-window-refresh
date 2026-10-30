import importlib.util
import pathlib
import subprocess
import sys
import tempfile
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("package_docs", ROOT / "scripts/package_docs.py")
package_docs = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(package_docs)


class DocumentationTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = pathlib.Path(temporary.name)

    def put(self, name, content="fixture\n"):
        path = self.root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content)
        return path

    def test_flat_package_links_use_member_names_and_pinned_source(self):
        source = "docs/user/install.md"
        original = ('[Architecture](../development/architecture.md#构建)\n'
                    '[Client](../../scripts/client.py)\n'
                    '[Analysis](../research/analysis.md#results)\n'
                    '[Root](/README.md)\n'
                    '[Local](#install) [Web](https://example.com/docs)\n')
        self.put(source, original)
        self.put("docs/development/architecture.md")
        self.put("docs/research/analysis.md")
        self.put("scripts/client.py")
        self.put("README.md")
        members = {"INSTALL.md": source, "PROTOTYPE.md": "docs/development/architecture.md",
                   "client.py": "scripts/client.py"}
        commit = "a" * 40
        rendered = package_docs.render_document(self.root, source, members, commit).decode()
        self.assertIn("[Architecture](PROTOTYPE.md#构建)", rendered)
        self.assertIn("[Client](client.py)", rendered)
        self.assertIn(f"/blob/{commit}/docs/research/analysis.md#results", rendered)
        self.assertIn(f"/blob/{commit}/README.md", rendered)
        self.assertIn("[Local](#install) [Web](https://example.com/docs)", rendered)
        self.assertEqual((self.root / source).read_text(), original)

    def test_markdown_titles_encoded_paths_and_fenced_examples_are_preserved(self):
        self.put("docs/with space.md")
        source = "docs/install.md"
        self.put(source, '[Guide](<with%20space.md#section> "Read more")\n'
                 '```md\n[Example](missing-example.md)\n```\n'
                 '~~~markdown\n[Example](another-missing.md)\n~~~\n')
        rendered = package_docs.render_document(self.root, source,
                    {"GUIDE.md": "docs/with space.md"}, "b" * 40).decode()
        self.assertIn('[Guide](<GUIDE.md#section> "Read more")', rendered)
        self.assertIn("[Example](missing-example.md)", rendered)
        self.assertIn("[Example](another-missing.md)", rendered)

    def test_packaging_rejects_missing_targets_and_unpinned_source(self):
        self.put("docs/install.md", "[Missing](missing.md)\n")
        with self.assertRaisesRegex(ValueError, "full source commit"):
            package_docs.render_document(self.root, "docs/install.md", {}, "main")
        with self.assertRaisesRegex(ValueError, "docs/install.md:1: missing"):
            package_docs.render_document(self.root, "docs/install.md", {}, "a" * 40)

    def check(self):
        return subprocess.run([sys.executable, str(ROOT / "scripts/docs_check.py"),
                               "--root", str(self.root)], capture_output=True, text=True)

    def test_repository_check_reports_moved_and_escaping_links(self):
        readme = self.put("README.md", "[Install](docs/user/install.md)\n")
        self.put("docs/user/install.md", "[Back](../../README.md)\n")
        self.assertEqual(self.check().returncode, 0)
        readme.write_text("[Stale](docs/install.md)\n[Outside](../outside.md)\n")
        result = self.check()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("README.md:1: missing target: docs/install.md", result.stderr)
        self.assertIn("README.md:2: link escapes repository", result.stderr)


if __name__ == "__main__":
    unittest.main()
