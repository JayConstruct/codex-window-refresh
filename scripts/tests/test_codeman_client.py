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
spec = importlib.util.spec_from_file_location("codeman_client", ROOT / "scripts/cwr-codex.py")
launcher = importlib.util.module_from_spec(spec)
spec.loader.exec_module(launcher)


def codeman_fixture(root):
    """A small stand-in for Codeman's public stock/resolveRegistry module contract."""
    modules = root / "app/dist/config/cli-registry"
    modules.mkdir(parents=True)
    (root / "app/package.json").write_text('{"type":"module"}')
    (modules / "stock.js").write_text('''export const STOCK_CLIS = [{id:'codex', launch:{
      params:{model:{type:'token',pattern:'model'}},
      variants:[{id:'default',args:[{lit:'codex'},{flag:'--model',valueFrom:'model'},
        {lit:'resume',when:{param:'resumeId',state:'set'}}]}]}}];''')
    (modules / "registry.js").write_text('''export function resolveRegistry(stock, file, warnings) {
      const override=file.clis.codex || {};
      return {entries:[{...stock[0],...override,launch:{...stock[0].launch,...override.launch,
        params:{...stock[0].launch.params,...override.launch?.params}}}]};
    }''')
    return root


@unittest.skipUnless(shutil.which("node"), "Codeman setup requires Node.js")
class CodemanSetupTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = codeman_fixture(Path(self.temporary.name))
        self.path = self.root / "clis.json"

    def test_preserves_codeman_options_other_clis_and_private_backup(self):
        original = {"schemaVersion": 1, "clis": {"claude": {"enabled": False}, "codex": {
            "enabled": True, "launch": {"params": {"custom": {"type": "bool"}}}}}}
        self.path.write_text(json.dumps(original))
        before = self.path.read_bytes()
        launcher.configure_codeman(self.root)
        saved = json.loads(self.path.read_text())
        self.assertEqual(saved["clis"]["claude"], original["clis"]["claude"])
        self.assertEqual(saved["clis"]["codex"]["enabled"], True)
        launch = saved["clis"]["codex"]["launch"]
        self.assertEqual(launch["params"]["custom"], {"type": "bool"})
        self.assertEqual(launch["variants"][0]["args"][1], {"lit": "--"})
        self.assertEqual(launch["variants"][0]["args"][2], {"flag": "--model", "valueFrom": "model"})
        self.assertEqual(launch["variants"][0]["args"][3], {"lit": "resume", "when": {"param": "resumeId", "state": "set"}})
        self.assertEqual(self.path.stat().st_mode & 0o777, 0o600)
        backup = self.root / "clis.json.cwr-backup"
        self.assertEqual(backup.read_bytes(), before)
        self.assertEqual(backup.stat().st_mode & 0o777, 0o600)

    def test_repeat_setup_updates_path_without_duplicate_arguments_or_overwriting_backup(self):
        self.path.write_text('{"clis":{}}')
        launcher.configure_codeman(self.root)
        backup = (self.root / "clis.json.cwr-backup").read_bytes()
        moved = self.root / "bin with 'quotes'" / "cwr-codex.py"
        with mock.patch.object(launcher, "__file__", str(moved)):
            launcher.configure_codeman(self.root)
            before = self.path.read_bytes()
            launcher.configure_codeman(self.root)
        saved = json.loads(self.path.read_text())["clis"]["codex"]["launch"]
        self.assertEqual(saved["params"]["cwrExecutable"]["default"], str(moved))
        self.assertEqual(sum(arg.get("lit") == "--" for arg in saved["variants"][0]["args"]), 1)
        self.assertEqual(self.path.read_bytes(), before)
        self.assertEqual((self.root / "clis.json.cwr-backup").read_bytes(), backup)
        self.assertFalse(list(self.root.glob(".cwr-codeman-*")))

    def test_invalid_registry_or_custom_launch_command_is_never_overwritten(self):
        for content in [b"private invalid json", b'{"clis":[]}',
                        b'{"clis":{"codex":{"launch":{"variants":[{"id":"custom","args":[{"lit":"custom-codex"}]}]}}}}']:
            with self.subTest(content=content):
                self.path.write_bytes(content)
                with self.assertRaises(ValueError):
                    launcher.configure_codeman(self.root)
                self.assertEqual(self.path.read_bytes(), content)
                self.assertFalse((self.root / "clis.json.cwr-backup").exists())

    def test_missing_dependencies_do_not_create_registry(self):
        with mock.patch.object(launcher.shutil, "which", return_value=None), self.assertRaisesRegex(ValueError, "Node.js"):
            launcher.configure_codeman(self.root)
        (self.root / "app/dist/config/cli-registry/stock.js").unlink()
        with self.assertRaisesRegex(ValueError, "Codeman CLI"):
            launcher.configure_codeman(self.root)
        self.assertFalse(self.path.exists())


REAL_CODEMAN = Path(os.environ.get("CWR_TEST_CODEMAN_APP", str(Path.home() / ".codeman/app")))


@unittest.skipUnless(shutil.which("node") and (REAL_CODEMAN / "dist/config/cli-registry/stock.js").is_file(),
                     "Installed Codeman is optional")
class RealCodemanLaunchTests(unittest.TestCase):
    def test_actual_registry_rendering_preserves_codex_parameters_and_routes_the_project(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "app").symlink_to(REAL_CODEMAN, target_is_directory=True)
            script = root / "client bin 'quoted'" / "cwr-codex.py"
            script.parent.mkdir()
            shutil.copyfile(ROOT / "scripts/cwr-codex.py", script)
            script.chmod(0o755)
            subprocess.run([sys.executable, str(script), "--setup-codeman", str(root)], check=True, capture_output=True)
            render = '''import {pathToFileURL} from 'node:url';
import {readFileSync} from 'node:fs';
const modules=process.argv[1], config=JSON.parse(readFileSync(process.argv[2],'utf8'));
const {STOCK_CLIS}=await import(pathToFileURL(modules+'/stock.js'));
const {resolveRegistry}=await import(pathToFileURL(modules+'/registry.js'));
const {renderLaunch}=await import(pathToFileURL(modules+'/argv.js'));
const warnings=[];
const entry=resolveRegistry(STOCK_CLIS,config,warnings).entries.find(entry=>entry.id==='codex');
if(warnings.length)throw new Error('Invalid registry');
process.stdout.write(renderLaunch(entry.launch,{model:'test-model',reasoningEffort:'high',animations:false,resumeId:'previous-session'},{}));'''
            command = subprocess.run(["node", "--input-type=module", "-e", render,
                                      str(root / "app/dist/config/cli-registry"), str(root / "clis.json")],
                                     check=True, capture_output=True, text=True).stdout
            official = root / "official"
            official.mkdir()
            binary = official / "codex"
            binary.write_text("#!" + sys.executable + "\nimport json,sys\nprint(json.dumps(sys.argv[1:]))\nsys.exit(7)\n")
            binary.chmod(0o755)
            project = root / "project"
            project.mkdir()
            environment = {**os.environ, "HOME": str(root), "XDG_CONFIG_HOME": str(root / "settings"),
                           "PATH": str(official) + os.pathsep + os.environ["PATH"]}
            executed = subprocess.run(["bash", "--noprofile", "--norc", "-c", command], cwd=project,
                                      env=environment, capture_output=True, text=True, timeout=10)
            self.assertEqual(executed.returncode, 7, executed.stderr)
            argv = json.loads(executed.stdout)
            self.assertEqual(argv[:2], ["-C", str(project)])
            self.assertEqual(sum(".http_headers.X-CWR-Project=" in arg for arg in argv), 1)
            self.assertIn("tui.animations=false", argv)
            self.assertIn("model_reasoning_effort=high", argv)
            self.assertEqual(argv[-2:], ["resume", "previous-session"])
            self.assertEqual(argv[argv.index("--model") + 1], "test-model")
