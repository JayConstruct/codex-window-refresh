import importlib.util
import pathlib
import tempfile
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("release_check", ROOT / "scripts/release_check.py")
release_check = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(release_check)


class ReleaseCheckTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = pathlib.Path(temporary.name)
        for directory in ["release", "internal/plugin", "docs/releases"]:
            (self.root / directory).mkdir(parents=True)
        (self.root / "release/version.txt").write_text("v1.2.3-rc.4\n")
        (self.root / "internal/plugin/service.go").write_text('const Version = "1.2.3-rc.4"\n')
        self.notes = self.root / "docs/releases/v1.2.3-rc.4.md"
        self.notes.write_text("# v1.2.3-rc.4 测试版\n\nFixed query synchronization.\n")

    def test_matching_release_and_explicit_selection(self):
        self.assertEqual(release_check.check_release(self.root, "v1.2.3-rc.4"), "v1.2.3-rc.4")

    def test_rejects_version_drift_before_packaging(self):
        with self.assertRaisesRegex(ValueError, "requested version"):
            release_check.check_release(self.root, "v1.2.3-rc.5")
        (self.root / "internal/plugin/service.go").write_text('const Version = "1.2.3-rc.3"\n')
        with self.assertRaisesRegex(ValueError, "plugin Version"):
            release_check.check_release(self.root)

    def test_rejects_invalid_tag_and_path_input(self):
        for invalid in ["../outside", "v1.2.3", "v1.2.3-rc.0", "v1.2.3-rc.01"]:
            with self.subTest(invalid=invalid):
                (self.root / "release/version.txt").write_text(invalid)
                with self.assertRaises(ValueError):
                    release_check.check_release(self.root)

    def test_requires_matching_nonempty_release_notes(self):
        for notes in ["", "# v1.2.3-rc.3\n\nOld notes.", "# v1.2.3-rc.4\n\n## Changes\n"]:
            with self.subTest(notes=notes):
                self.notes.write_text(notes)
                with self.assertRaises(ValueError):
                    release_check.check_release(self.root)
        self.notes.unlink()
        with self.assertRaises(FileNotFoundError):
            release_check.check_release(self.root)


if __name__ == "__main__":
    unittest.main()
