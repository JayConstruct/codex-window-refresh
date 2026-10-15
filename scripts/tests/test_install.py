import argparse
import contextlib
import hashlib
import importlib.util
import io
import json
import os
import pathlib
import platform
import pwd
import struct
import sys
import tarfile
import tempfile
import unittest
from unittest import mock

import yaml


ROOT = pathlib.Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("installer", ROOT / "scripts/install.py")
installer = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(installer)


def elf(machine=62):
    return b"\x7fELF\x02\x01" + b"\0" * 12 + struct.pack("<H", machine)


class ConfigurationTests(unittest.TestCase):
    def test_only_plugins_change_and_other_plugins_remain(self):
        text = ('# Main config\nport: 8317\napi-keys: ["fixture-secret"]\n'
                'plugins:\n  enabled: false\n  dir: /existing\n'
                '  configs:\n    another: {enabled: true, custom: [a, b]}\n'
                'routing: {strategy: round-robin}\n')
        data, _ = installer.load_yaml(text)
        plugins = data["plugins"]
        plugins["configs"][installer.PLUGIN] = {"enabled": True, "config_file": "/policy.yaml"}
        changed = installer.replace_plugins(text, plugins)
        self.assertTrue(changed.startswith('# Main config\nport: 8317\napi-keys: ["fixture-secret"]\n'))
        self.assertTrue(changed.endswith('routing: {strategy: round-robin}\n'))
        self.assertEqual(yaml.safe_load(changed)["plugins"]["configs"]["another"],
                         {"enabled": True, "custom": ["a", "b"]})

    def test_missing_plugins_before_document_end_and_inline_mapping(self):
        for text in ['---\nport: 8317\n...\n', 'port: 8317\n',
                     'plugins: {enabled: false}\nport: 8317\n']:
            changed = installer.replace_plugins(text, {"enabled": True, "configs": {}})
            self.assertEqual(yaml.safe_load(changed), {"port": 8317, "plugins": {"enabled": True, "configs": {}}})

    def test_unsafe_yaml_rejected_without_secret_context(self):
        for text in ['port: 1\nport: 2\n', 'plugins: {configs: {x: 1, x: 2}}',
                     'x: &x [a]\ny: *x\n', 'port: 1\n---\nport: 2',
                     'key: "fixture-private-value\n', '{port: 8317}']:
            with self.assertRaises(installer.InstallError) as result:
                installer.load_yaml(text)
            self.assertNotIn("fixture-private-value", str(result.exception))

    def test_key_is_only_environment_file_data(self):
        value = 'fixture$`value"\\suffix'
        result = installer.environment_content(value, "CPA_MANAGEMENT_KEY").decode()
        self.assertEqual(result, 'CPA_MANAGEMENT_KEY="fixture$`value\\"\\\\suffix"\n')
        for key in ['', 'a\nb', 'a\rb', '\x00key']:
            with self.assertRaises(installer.InstallError):
                installer.environment_content(key, "CPA_MANAGEMENT_KEY")

    def test_symlink_and_systemd_expansion_paths_are_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            (root / "alias").symlink_to(root, target_is_directory=True)
            for path in [root / 'alias/file', root / 'file%name', root / 'line\nbreak', root / 'file"name']:
                with self.assertRaises(installer.InstallError):
                    installer.safe_path(path)

    def test_config_arguments_with_spaces(self):
        self.assertEqual(installer.cli_config(['cpa', '-config', 'my config.yaml'], pathlib.Path('/srv')),
                         pathlib.Path('/srv/my config.yaml'))
        with self.assertRaises(installer.InstallError):
            installer.cli_config(['cpa', '-config=a', '--config=b'], pathlib.Path('/srv'))


class PackageTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = pathlib.Path(self.temp.name)
        self.output = self.root / "unpacked"
        self.output.mkdir()
        self.name = 'codex-window-refresh_v0.1.0-rc.1_linux_amd64.tar.gz'
        self.archive = self.root / self.name
        self.checksums = self.root / 'SHA256SUMS'

    def package(self, extras=None, architecture="amd64"):
        files = {'codex-window-refresh.so': elf(), 'window-refresh': elf(),
                 'config.example.yaml': b'enabled: false\n',
                 'BUILD-INFO.json': json.dumps({'release': 'v0.1.0-rc.1', 'architecture': architecture,
                                              'platform': 'linux', 'host_abi': 1, 'rpc_schema': 6,
                                              'minimum_glibc_symbols': '2.0'}).encode()}
        with tarfile.open(self.archive, 'w:gz') as tar:
            for name, data in files.items():
                item = tarfile.TarInfo(self.name[:-7] + '/' + name)
                item.size = len(data)
                tar.addfile(item, io.BytesIO(data))
            for item, content in extras or []:
                tar.addfile(item, io.BytesIO(content) if content is not None else None)
        self.checksums.write_text(hashlib.sha256(self.archive.read_bytes()).hexdigest() + '  ' + self.name + '\n')

    def verify(self):
        installer.verify_archive(self.archive, self.checksums, self.name, self.output, 'v0.1.0-rc.1', 'amd64')

    def test_checksum_and_version_architecture_proof(self):
        self.package()
        self.verify()
        self.assertTrue((self.output / 'window-refresh').stat().st_mode & 0o111)
        self.archive.write_bytes(self.archive.read_bytes() + b'tampered')
        with self.assertRaises(installer.InstallError):
            self.verify()
        self.package(architecture='arm64')
        with self.assertRaises(installer.InstallError):
            self.verify()

    def test_traversal_symlink_duplicate_and_unknown_file_rejected(self):
        candidates = []
        for name in ['../escaped', self.name[:-7] + '/unknown.sh',
                     self.name[:-7] + '/codex-window-refresh.so']:
            item = tarfile.TarInfo(name)
            item.size = 1
            candidates.append((item, b'x'))
        symlink = tarfile.TarInfo(self.name[:-7] + '/install.py')
        symlink.type = tarfile.SYMTYPE
        symlink.linkname = '/etc/passwd'
        candidates.append((symlink, None))
        for candidate in candidates:
            self.package([candidate])
            with self.assertRaises(installer.InstallError):
                self.verify()
            self.assertFalse((self.root / 'escaped').exists())


@unittest.skipUnless((ROOT / 'bin/window-refresh').is_file(), 'run make build before installer integration tests')
class InstallIntegrationTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='window-refresh-installer-test-')
        self.addCleanup(self.temp.cleanup)
        self.root = pathlib.Path(self.temp.name)
        self.cpa = self.root / 'cpa'
        self.cpa.mkdir()
        (self.cpa / 'cli-proxy-api').write_bytes(elf(62 if platform.machine() == 'x86_64' else 183))
        self.config = self.cpa / 'config.yaml'
        self.original = ('# deployment configuration\nport: 8317\n'
                         'api-keys: ["fixture-private-value"]\n'
                         'plugins:\n  enabled: false\n  configs:\n'
                         '    another: {enabled: false, custom: untouched}\n')
        self.config.write_text(self.original)
        self.config.chmod(0o640)
        self.key = self.root / 'key'
        self.key.write_text('fixture-management-secret\n')
        self.key.chmod(0o600)
        arch = {'x86_64': 'amd64', 'aarch64': 'arm64'}[platform.machine()]
        self.arch = arch
        name = f'codex-window-refresh_v0.1.0-rc.9_linux_{arch}'
        self.archive = self.root / (name + '.tar.gz')
        metadata = {'release': 'v0.1.0-rc.9', 'architecture': arch, 'platform': 'linux',
                    'host_abi': 1, 'rpc_schema': 6, 'minimum_glibc_symbols': '2.0'}
        machine = 62 if arch == 'amd64' else 183
        files = {'codex-window-refresh.so': elf(machine),
                 'window-refresh': (ROOT / 'bin/window-refresh').read_bytes(),
                 'config.example.yaml': (ROOT / 'config.example.yaml').read_bytes(),
                 'BUILD-INFO.json': json.dumps(metadata).encode()}
        with tarfile.open(self.archive, 'w:gz') as tar:
            for path, data in files.items():
                item = tarfile.TarInfo(name + '/' + path)
                item.size = len(data)
                tar.addfile(item, io.BytesIO(data))
        self.checksums = self.root / 'SHA256SUMS'
        self.checksums.write_text(hashlib.sha256(self.archive.read_bytes()).hexdigest() + '  ' + self.archive.name + '\n')
        self.args = argparse.Namespace(cpa_dir=str(self.cpa), config=None, user=pwd.getpwuid(os.geteuid()).pw_name, working_dir=str(self.cpa),
                                       service=None, plugin_dir=None, management_url=None, quota_transport='management', model='test-model',
                                       yes=True, key_file=str(self.key), version='v0.1.0-rc.9',
                                       package=str(self.archive), checksums=str(self.checksums), restart=False, uninstall=False)
        self.plan = installer.infer(self.args)
        # Simulate root ownership operations inside a disposable directory without
        # needing root or changing any real service. Actual policy checks use Go CLI.
        uid, gid = os.geteuid(), os.getegid()
        old_fchown, old_chown = os.fchown, os.chown
        self.addCleanup(mock.patch.stopall)
        mock.patch.object(installer.os, 'fchown', side_effect=lambda fd, u, g: old_fchown(fd, uid if u == 0 else u, gid if g == 0 else g)).start()
        mock.patch.object(installer.os, 'chown', side_effect=lambda p, u, g: old_chown(p, uid if u == 0 else u, gid if g == 0 else g)).start()
        old_directory = installer.directory
        mock.patch.object(installer, 'directory', side_effect=lambda p, m, u, g, strict=False: old_directory(p, m, uid if u == 0 else u, gid if g == 0 else g, strict)).start()

        def private(path):
            path = installer.safe_path(path)
            self.assertEqual(path.stat().st_uid, uid)
            installer.require(path.stat().st_mode & 0o077 == 0, 'private file permissions invalid')
            return path

        mock.patch.object(installer, 'private_file', side_effect=private).start()

    def install(self):
        with contextlib.redirect_stdout(io.StringIO()) as output:
            installer.install(self.args, self.plan, self.arch)
        self.assertNotIn('fixture-management-secret', output.getvalue())
        self.assertNotIn('fixture-private-value', output.getvalue())

    def package_version(self, version):
        with tarfile.open(self.archive, 'r:gz') as tar:
            files = {pathlib.PurePosixPath(item.name).name: tar.extractfile(item).read() for item in tar}
        metadata = json.loads(files['BUILD-INFO.json'])
        metadata.update(release=version, hot_reload_protocol=1)
        files['BUILD-INFO.json'] = json.dumps(metadata).encode()
        files['codex-window-refresh.so'] += version.encode()
        name = f'codex-window-refresh_{version}_linux_{self.arch}'
        self.archive = self.root / (name + '.tar.gz')
        with tarfile.open(self.archive, 'w:gz') as tar:
            for path, data in files.items():
                item = tarfile.TarInfo(name + '/' + path)
                item.size = len(data)
                tar.addfile(item, io.BytesIO(data))
        self.checksums.write_text(hashlib.sha256(self.archive.read_bytes()).hexdigest() + '  ' + self.archive.name + '\n')
        self.args.version = version
        self.args.package = str(self.archive)
        self.plan = installer.infer(self.args)

    def test_bootstrap_installs_versioned_file_and_restart_upgrade_uninstalls_all_owned_versions(self):
        self.install() # Existing pre-hot-reload stable installation.
        self.package_version('v0.1.0-rc.20')
        self.install()
        wrapper = yaml.safe_load(self.config.read_text())['plugins']['configs'][installer.PLUGIN]
        self.assertEqual(wrapper['store']['version'], '0.1.0-rc.20')
        current = self.plan['plugin_dir'] / 'codex-window-refresh-v0.1.0-rc.20.so'
        self.assertTrue(current.is_file())
        record = json.loads((self.plan['record'] / 'installed.json').read_text())
        self.assertEqual(record['library'], str(current))
        # CPA may clean the unselected stable file on its next process startup.
        (self.plan['plugin_dir'] / 'codex-window-refresh.so').unlink()
        self.package_version('v0.1.0-rc.21')
        self.install()
        newest = self.plan['plugin_dir'] / 'codex-window-refresh-v0.1.0-rc.21.so'
        self.assertTrue(newest.is_file() and current.is_file())
        self.plan = installer.infer(self.args)
        with contextlib.redirect_stdout(io.StringIO()):
            installer.uninstall(self.args, self.plan)
        self.assertFalse(newest.exists() or current.exists())
        self.assertTrue(self.plan['policy'].is_file() and self.plan['state'].is_dir())

    def test_first_hot_capable_install_uses_versioned_file_and_pin(self):
        self.package_version('v0.1.0-rc.20')
        self.install()
        self.assertFalse((self.plan['plugin_dir'] / 'codex-window-refresh.so').exists())
        self.assertTrue((self.plan['plugin_dir'] / 'codex-window-refresh-v0.1.0-rc.20.so').is_file())
        wrapper = yaml.safe_load(self.config.read_text())['plugins']['configs'][installer.PLUGIN]
        self.assertEqual(wrapper['store']['release-tag'], 'v0.1.0-rc.20')

    def test_local_hot_reload_needs_no_management_tls_address_even_with_bridge_transport(self):
        self.install()
        data = yaml.safe_load(self.config.read_text())
        data['tls'] = {'enable': True}
        data['plugins']['configs'][installer.PLUGIN].pop('management_url', None)
        data['plugins']['configs'][installer.PLUGIN].pop('management_key_env', None)
        self.config.write_text(yaml.safe_dump(data))
        self.args.hot_reload = True
        self.args.key_file = None
        self.args.management_url = None
        self.plan = installer.infer(self.args)
        self.assertEqual(self.plan['quota_transport'], 'management')

    def test_install_upgrade_uninstall_preserve_policy_and_state(self):
        self.install()
        data = yaml.safe_load(self.config.read_text())
        self.assertTrue(data['plugins']['enabled'])
        self.assertEqual(data['plugins']['configs']['another'], {'enabled': False, 'custom': 'untouched'})
        self.assertEqual(self.config.stat().st_mode & 0o777, 0o640)
        policy = self.plan['policy']
        self.assertFalse(yaml.safe_load(policy.read_text())['enabled'])
        state = self.plan['state'] / 'window.json'
        state.write_text('durable-window-fixture')
        custom = policy.read_text().replace('enabled: false', 'enabled: true', 1)
        policy.write_text(custom)
        self.plan = installer.infer(self.args)
        self.install()
        self.assertEqual(policy.read_text(), custom)
        self.assertEqual(state.read_text(), 'durable-window-fixture')
        self.plan = installer.infer(self.args)
        self.args.uninstall = True
        with contextlib.redirect_stdout(io.StringIO()):
            installer.uninstall(self.args, self.plan)
        self.assertNotIn(installer.PLUGIN, yaml.safe_load(self.config.read_text())['plugins']['configs'])
        self.assertIn('another', yaml.safe_load(self.config.read_text())['plugins']['configs'])
        self.assertFalse((self.plan['plugin_dir'] / 'codex-window-refresh.so').exists())
        self.assertTrue(policy.exists() and state.exists() and self.plan['env'].exists())
        backups = list(self.plan['record'].glob('*/index.json'))
        self.assertEqual(len(backups), 3)
        backup_files = list(self.plan['record'].glob('*/*config.yaml'))
        self.assertTrue(any(p.read_text() == self.original for p in backup_files))

    def test_default_host_install_does_not_read_or_persist_management_credentials(self):
        self.args.quota_transport = None
        self.args.key_file = None
        self.plan = installer.infer(self.args)
        self.assertEqual(self.plan['quota_transport'], 'host')
        with mock.patch.object(installer, 'management_credentials', side_effect=AssertionError('host install must not ask for a key')):
            self.install()
        wrapper = yaml.safe_load(self.config.read_text())['plugins']['configs'][installer.PLUGIN]
        self.assertEqual(wrapper['quota_transport'], 'host')
        self.assertNotIn('management_url', wrapper)
        self.assertNotIn('management_key_env', wrapper)
        self.assertFalse(self.plan['env'].exists())

    def test_deferred_model_first_install_needs_no_key_and_upgrades_preserve_policy(self):
        self.args.quota_transport = 'host'
        self.args.model = None
        self.args.key_file = None
        self.args.defer_model = True
        self.plan = installer.infer(self.args)
        with mock.patch.object(installer, 'management_credentials', side_effect=AssertionError('must not ask for a key')), \
                mock.patch.object(installer, 'discover_codex_model', side_effect=AssertionError('must not query models')):
            self.install()
        policy = yaml.safe_load(self.plan['policy'].read_text())
        self.assertFalse(policy['enabled'])
        self.assertEqual(policy['execution']['model'], '')
        self.assertFalse(policy['quota_query']['automatic'])
        self.assertTrue(policy['quota_query']['manual'])
        self.assertFalse(self.plan['env'].exists())
        policy['enabled'] = True
        policy['execution']['model'] = 'operator-selected-model'
        self.plan['policy'].write_text(yaml.safe_dump(policy))
        original = self.plan['policy'].read_bytes()
        self.args.model = 'must-not-overwrite'
        self.plan = installer.infer(self.args)
        self.install()
        self.assertEqual(self.plan['policy'].read_bytes(), original)

    def test_deferred_model_respects_explicit_first_install_model(self):
        self.args.quota_transport = 'host'
        self.args.key_file = None
        self.args.defer_model = True
        self.plan = installer.infer(self.args)
        with mock.patch.object(installer, 'management_credentials', side_effect=AssertionError('must not ask for a key')):
            self.install()
        self.assertEqual(yaml.safe_load(self.plan['policy'].read_text())['execution']['model'], 'test-model')

    def test_deferred_model_host_install_does_not_require_management_tls_override(self):
        self.config.write_text(self.original + 'tls: {enable: true}\n')
        self.args.quota_transport = 'host'
        self.args.model = None
        self.args.key_file = None
        self.args.defer_model = True
        self.plan = installer.infer(self.args)
        self.install()
        self.assertEqual(yaml.safe_load(self.plan['policy'].read_text())['execution']['model'], '')

    def test_older_release_keeps_management_mode_and_rejects_explicit_host(self):
        self.args.version = 'v0.1.0-rc.8'
        self.args.quota_transport = None
        self.plan = installer.infer(self.args)
        self.assertEqual(self.plan['quota_transport'], 'management')
        self.args.quota_transport = 'host'
        with mock.patch.object(installer, 'download') as download, self.assertRaises(installer.InstallError):
            self.install()
        download.assert_not_called()
        self.assertEqual(self.config.read_text(), self.original)

    def test_host_upgrade_preserves_old_key_file_without_reading_it(self):
        self.install()
        policy = self.plan['policy'].read_bytes()
        self.plan['env'].write_text('private legacy content outside installer format\n')
        self.args.quota_transport = 'host'
        self.args.key_file = None
        self.plan = installer.infer(self.args)
        with mock.patch.object(installer, 'management_credentials', side_effect=AssertionError('host upgrade must not read old key')):
            self.install()
        self.assertEqual(self.plan['policy'].read_bytes(), policy)
        self.assertEqual(self.plan['env'].read_text(), 'private legacy content outside installer format\n')
        self.assertEqual(yaml.safe_load(self.config.read_text())['plugins']['configs'][installer.PLUGIN]['quota_transport'], 'host')

    def test_host_user_service_override_does_not_load_management_environment(self):
        self.args.quota_transport = 'host'
        self.args.key_file = None
        self.plan = installer.infer(self.args)
        service = {'name': 'fixture-cpa.service', 'scope': 'user'}
        self.plan['service'] = service
        account = pwd.getpwuid(os.geteuid())
        home = self.root / 'user-home'
        home.mkdir()
        replacement = pwd.struct_passwd((account.pw_name, account.pw_passwd, account.pw_uid,
                                        account.pw_gid, account.pw_gecos, str(home), account.pw_shell))
        self.plan['dropin'] = home / '.config/systemd/user/fixture-cpa.service.d/60-window-refresh.conf'
        # A prior managed override may refer to a file that never reached CPA.
        self.plan['dropin'].parent.mkdir(parents=True)
        self.plan['dropin'].write_text('[Service]\nEnvironmentFile=/missing/old-quota.env\n')
        self.args.restart = True
        with mock.patch.object(installer.pwd, 'getpwuid', return_value=replacement), mock.patch.object(installer, 'service_run', return_value='active'), mock.patch.object(installer, 'management_credentials', side_effect=AssertionError('host user service must not ask for a key')):
            self.install()
        self.assertNotIn('EnvironmentFile=', self.plan['dropin'].read_text())
        self.assertFalse(self.plan['env'].exists())
        self.assertEqual(json.loads((self.plan['record'] / 'installed.json').read_text())['service_scope'], 'user')

    def test_failed_config_write_restores_old_binary_and_all_files(self):
        plugin_dir = self.plan['plugin_dir']
        plugin_dir.mkdir()
        library = plugin_dir / 'codex-window-refresh.so'
        library.write_bytes(b'previous-library')
        original = installer.atomic_write
        failed = []

        def injected(path, *args):
            if path == self.config and not failed:
                failed.append(True)
                raise OSError('injected disk failure')
            return original(path, *args)

        with mock.patch.object(installer, 'atomic_write', side_effect=injected):
            with self.assertRaises(OSError):
                self.install()
        self.assertEqual(library.read_bytes(), b'previous-library')
        self.assertEqual(self.config.read_text(), self.original)
        self.assertFalse(self.plan['policy'].exists())
        self.assertFalse(self.plan['env'].exists())
        self.assertFalse((self.cpa / 'window-refresh').exists())

    def test_bad_download_does_not_touch_deployment(self):
        self.archive.write_bytes(self.archive.read_bytes() + b'corruption')
        before = self.config.read_bytes()
        with self.assertRaises(installer.InstallError):
            self.install()
        self.assertEqual(self.config.read_bytes(), before)
        self.assertFalse(self.plan['plugin_dir'].exists())

    def test_invalid_model_is_rejected_before_config_and_key_writes(self):
        self.args.model = 'REPLACE_WITH_SUPPORTED_MODEL'
        with self.assertRaises(installer.InstallError):
            self.install()
        self.assertEqual(self.config.read_text(), self.original)
        self.assertFalse(self.plan['plugin_dir'].exists())
        self.assertFalse(self.plan['env'].exists())

    def test_first_install_detects_common_model_and_upgrade_preserves_policy(self):
        self.args.model = None
        with mock.patch.object(installer, 'discover_codex_model', return_value=('fixture-codex-mini', 2)) as detect:
            self.install()
        detect.assert_called_once_with(self.plan['url'], 'fixture-management-secret')
        policy = self.plan['policy'].read_bytes()
        self.assertEqual(yaml.safe_load(policy)['execution']['model'], 'fixture-codex-mini')
        self.assertFalse(yaml.safe_load(policy)['enabled'])
        self.plan = installer.infer(self.args)
        with mock.patch.object(installer, 'discover_codex_model') as detect:
            self.install()
        detect.assert_not_called()
        self.assertEqual(self.plan['policy'].read_bytes(), policy)

    def test_explicit_model_skips_discovery(self):
        with mock.patch.object(installer, 'discover_codex_model') as detect:
            self.install()
        detect.assert_not_called()

    def test_detection_failure_in_noninteractive_install_writes_nothing(self):
        self.args.model = None
        with mock.patch.object(installer, 'discover_codex_model', side_effect=installer.InstallError('fixture-discovery-unavailable')):
            with self.assertRaises(installer.InstallError), contextlib.redirect_stdout(io.StringIO()):
                installer.install(self.args, self.plan, self.arch)
        self.assertEqual(self.config.read_text(), self.original)
        self.assertFalse(self.plan['env'].exists())
        self.assertFalse(self.plan['policy'].exists())
        self.assertFalse(self.plan['record'].exists())

    def test_detection_failure_falls_back_to_interactive_model_entry(self):
        self.args.model = None
        self.args.yes = False
        with mock.patch.object(installer, 'discover_codex_model', side_effect=installer.InstallError('fixture-discovery-unavailable')), \
                mock.patch.object(installer.sys.stdin, 'isatty', return_value=True), \
                mock.patch('builtins.input', side_effect=['fixture-manual-model', 'yes', 'no']) as ask:
            self.install()
        self.assertEqual(yaml.safe_load(self.plan['policy'].read_text())['execution']['model'], 'fixture-manual-model')
        self.assertEqual(ask.call_count, 3)

    def test_detected_system_service_uses_restart_prompt_and_attaches_existing_direct_install(self):
        self.install()
        self.assertIsNone(json.loads((self.plan['record'] / 'installed.json').read_text())['dropin'])
        process = {'pid': 123, 'uid': os.geteuid(), 'cwd': self.cpa, 'args': [str(self.cpa / 'cli-proxy-api')]}
        service = {'name': 'fixture-cpa.service', 'User': self.args.user, 'MainPID': '123',
                   'WorkingDirectory': str(self.cpa), 'ExecStart': str(self.cpa / 'cli-proxy-api')}
        with mock.patch.object(installer, 'detect_processes', return_value=[process]), \
                mock.patch.object(installer, 'detect_service', return_value=service):
            self.plan = installer.infer(self.args)
        self.assertTrue(self.plan['service_auto'])
        # Keep the complete install transaction inside the disposable fixture.
        self.plan['dropin'] = self.root / 'systemd/fixture-cpa.service.d/60-window-refresh.conf'
        actual = installer.run
        calls = []

        def execute(*args):
            if args[0] == 'systemctl':
                calls.append(args)
                return 'active'
            return actual(*args)

        policy = self.plan['policy'].read_bytes()
        self.args.yes = False
        with mock.patch.object(installer, 'run', side_effect=execute), \
                mock.patch.object(installer.sys.stdin, 'isatty', return_value=True), \
                mock.patch('builtins.input', side_effect=['yes', 'yes']):
            self.install()
        self.assertIn(('systemctl', 'restart', 'fixture-cpa.service'), calls)
        self.assertEqual(self.plan['policy'].read_bytes(), policy)
        self.assertEqual(json.loads((self.plan['record'] / 'installed.json').read_text())['dropin'], str(self.plan['dropin']))

    def test_uninstall_old_direct_install_after_service_is_detected_leaves_unowned_override(self):
        self.install()
        self.plan = installer.infer(self.args)
        self.plan['service'] = {'name': 'fixture-cpa.service'}
        self.plan['service_auto'] = True
        self.plan['dropin'] = self.root / 'unowned.conf'
        self.plan['dropin'].write_text('unowned')
        self.args.uninstall = True
        with mock.patch.object(installer, 'run', return_value='active'), contextlib.redirect_stdout(io.StringIO()):
            installer.uninstall(self.args, self.plan)
        self.assertEqual(self.plan['dropin'].read_text(), 'unowned')

    def test_user_service_install_upgrade_and_uninstall_preserve_private_environment(self):
        account = pwd.getpwuid(os.geteuid())
        home = self.root / 'user-home'
        home.mkdir()
        replacement = pwd.struct_passwd((account.pw_name, account.pw_passwd, account.pw_uid,
                                        account.pw_gid, account.pw_gecos, str(home), account.pw_shell))
        service = {'name': 'fixture-cpa.service', 'scope': 'user', 'manager_user': account.pw_name,
                   'User': account.pw_name, 'unit_dir': str(home / '.config/systemd/user'),
                   'WorkingDirectory': str(self.cpa), 'ExecStart': str(self.cpa / 'cli-proxy-api')}
        self.plan['service'] = service
        self.plan['dropin'] = home / '.config/systemd/user/fixture-cpa.service.d/60-window-refresh.conf'
        # Force a separate user copy even when the test runner itself is root.
        self.plan['service_env'] = self.plan['dropin'].parent / '60-window-refresh.env'
        self.args.restart = True
        with mock.patch.object(installer.pwd, 'getpwuid', return_value=replacement), \
                mock.patch.object(installer, 'service_run', return_value='active') as calls:
            self.install()
        calls.assert_has_calls([mock.call(service, 'daemon-reload'), mock.call(service, 'restart', service['name']),
                                mock.call(service, 'is-active', service['name'])])
        env = self.plan['service_env']
        self.assertEqual(env.read_bytes(), self.plan['env'].read_bytes())
        self.assertEqual(env.stat().st_mode & 0o777, 0o600)
        self.assertEqual(env.stat().st_uid, os.geteuid())
        self.assertIn(str(env), self.plan['dropin'].read_text())
        self.assertEqual(json.loads((self.plan['record'] / 'installed.json').read_text())['service_scope'], 'user')
        self.key.write_text('fixture-updated-management-secret\n')
        self.plan['text'] = self.config.read_text()
        self.plan['plugins'] = yaml.safe_load(self.plan['text'])['plugins']
        with mock.patch.object(installer.pwd, 'getpwuid', return_value=replacement), \
                mock.patch.object(installer, 'service_run', return_value='active'):
            self.install()
        self.assertEqual(env.read_bytes(), self.plan['env'].read_bytes())
        self.plan['text'] = self.config.read_text()
        self.plan['plugins'] = yaml.safe_load(self.plan['text'])['plugins']
        self.args.uninstall = True
        with mock.patch.object(installer, 'service_run', return_value='active'), contextlib.redirect_stdout(io.StringIO()):
            installer.uninstall(self.args, self.plan)
        self.assertFalse(self.plan['dropin'].exists())
        self.assertTrue(env.exists())
        self.assertTrue(self.plan['policy'].exists())

    def test_user_service_reload_failure_rolls_back_private_copy_and_override(self):
        account = pwd.getpwuid(os.geteuid())
        home = self.root / 'user-home'
        home.mkdir()
        replacement = pwd.struct_passwd((account.pw_name, account.pw_passwd, account.pw_uid,
                                        account.pw_gid, account.pw_gecos, str(home), account.pw_shell))
        self.plan['service'] = {'name': 'fixture-cpa.service', 'scope': 'user', 'manager_user': account.pw_name}
        self.plan['dropin'] = home / '.config/systemd/user/fixture-cpa.service.d/60-window-refresh.conf'
        self.plan['service_env'] = self.plan['dropin'].parent / '60-window-refresh.env'
        with mock.patch.object(installer.pwd, 'getpwuid', return_value=replacement), \
                mock.patch.object(installer, 'service_run', side_effect=installer.InstallError('fixture-reload-failure')):
            with self.assertRaises(installer.InstallError): self.install()
        self.assertEqual(self.config.read_text(), self.original)
        self.assertFalse(self.plan['dropin'].exists())
        self.assertFalse(self.plan['service_env'].exists())
        self.assertFalse(self.plan['env'].exists())

    def test_explicit_user_service_infers_user_unit_directory_and_private_environment(self):
        home = self.root / 'user-home'
        service = {'name': 'fixture-cpa.service', 'scope': 'user', 'manager_user': self.args.user,
                   'User': self.args.user, 'unit_dir': str(home / '.config/systemd/user'),
                   'WorkingDirectory': str(self.cpa), 'ExecStart': str(self.cpa / 'cli-proxy-api')}
        self.args.user_service = True
        self.args.service = 'fixture-cpa.service'
        with mock.patch.object(installer, 'service_info', return_value=service) as read:
            plan = installer.infer(self.args)
        read.assert_called_once_with(self.args.service, self.args.user)
        self.assertEqual(plan['dropin'], home / '.config/systemd/user/fixture-cpa.service.d/60-window-refresh.conf')
        self.assertEqual(plan['service_env'], plan['env'] if os.geteuid() == 0 else plan['dropin'].parent / '60-window-refresh.env')
        self.assertFalse(plan['service_auto'])

    def test_live_config_change_is_not_overwritten(self):
        self.config.write_text(self.original + 'routing: {strategy: fill-first}\n')
        with self.assertRaises(installer.InstallError):
            self.install()
        self.assertIn('routing: {strategy: fill-first}', self.config.read_text())
        self.assertFalse(self.plan['policy'].exists())

    def test_systemd_dropin_and_explicit_restart(self):
        self.plan['service'] = {'name': 'fixture-cpa.service'}
        self.plan['dropin'] = self.root / 'systemd/fixture-cpa.service.d/60-window-refresh.conf'
        actual = installer.run
        calls = []

        def service(*args):
            if args[0] == 'systemctl':
                calls.append(args)
                return 'active'
            return actual(*args)

        self.args.restart = True
        with mock.patch.object(installer, 'run', side_effect=service):
            self.install()
        self.assertIn(('systemctl', 'daemon-reload'), calls)
        self.assertIn(('systemctl', 'restart', 'fixture-cpa.service'), calls)
        self.assertIn('EnvironmentFile="' + str(self.plan['env']) + '"', self.plan['dropin'].read_text())

    def test_systemd_daemon_reload_failure_restores_config_and_dropin(self):
        self.plan['service'] = {'name': 'fixture-cpa.service'}
        self.plan['dropin'] = self.root / 'systemd/fixture-cpa.service.d/60-window-refresh.conf'
        actual = installer.run

        def service(*args):
            if args[0] == 'systemctl':
                raise installer.InstallError('injected daemon-reload failure')
            return actual(*args)

        with mock.patch.object(installer, 'run', side_effect=service):
            with self.assertRaises(installer.InstallError):
                self.install()
        self.assertEqual(self.config.read_text(), self.original)
        self.assertFalse(self.plan['dropin'].exists())
        self.assertFalse(self.plan['env'].exists())
        self.assertFalse((self.plan['record'] / 'installed.json').exists())

    def test_uninstall_does_not_remove_changed_binary(self):
        self.install()
        library = self.plan['plugin_dir'] / 'codex-window-refresh.so'
        library.write_bytes(b'locally-updated-plugin')
        self.plan = installer.infer(self.args)
        with self.assertRaises(installer.InstallError):
            installer.uninstall(self.args, self.plan)
        self.assertTrue(library.exists())
        self.assertIn(installer.PLUGIN, yaml.safe_load(self.config.read_text())['plugins']['configs'])

    def test_preview_performs_no_writes_or_downloads(self):
        read_text = pathlib.Path.read_text

        def read(path, *args, **kwargs):
            if str(path) == '/etc/os-release':
                return 'ID=debian\nVERSION_ID="12"\n'
            return read_text(path, *args, **kwargs)

        before = sorted(str(p.relative_to(self.root)) for p in self.root.rglob('*'))
        with mock.patch.object(installer.os, 'geteuid', return_value=0), \
                mock.patch.object(pathlib.Path, 'read_text', new=read), \
                mock.patch.object(installer, 'download') as download, \
                mock.patch.object(sys, 'argv', ['install.py', '--cpa-dir', str(self.cpa), '--user', self.args.user,
                                               '--working-dir', str(self.cpa), '--dry-run']), \
                contextlib.redirect_stdout(io.StringIO()):
            installer.main()
        download.assert_not_called()
        self.assertEqual(before, sorted(str(p.relative_to(self.root)) for p in self.root.rglob('*')))

    def test_running_default_config_uses_process_working_directory(self):
        working = self.root / 'actual-working-directory'
        working.mkdir()
        (working / 'config.yaml').write_text('port: 9317\nplugins: {dir: plugins}\n')
        self.args.working_dir = None
        process = {'uid': os.geteuid(), 'cwd': working, 'args': [str(self.cpa / 'cli-proxy-api')]}
        with mock.patch.object(installer, 'detect_processes', return_value=[process]):
            plan = installer.infer(self.args)
            self.assertEqual(plan['config'], working / 'config.yaml')
            self.assertEqual(plan['plugin_dir'], working / 'plugins')
            self.assertEqual(plan['url'], 'http://127.0.0.1:9317/v0/management')
            self.args.config = str(self.config)
            with self.assertRaises(installer.InstallError):
                installer.infer(self.args)

    def test_home_plugin_dir_uses_cpa_user_home(self):
        self.config.write_text('port: 8317\nplugins: {dir: "~/cpa-plugins"}\n')
        current = pwd.getpwuid(os.geteuid())
        replacement = pwd.struct_passwd((current.pw_name, current.pw_passwd, current.pw_uid,
                                          current.pw_gid, current.pw_gecos, str(self.root / 'user-home'), current.pw_shell))
        with mock.patch.object(installer.pwd, 'getpwnam', return_value=replacement):
            plan = installer.infer(self.args)
        self.assertEqual(plan['plugin_dir'], self.root / 'user-home/cpa-plugins')


if __name__ == '__main__':
    unittest.main()
