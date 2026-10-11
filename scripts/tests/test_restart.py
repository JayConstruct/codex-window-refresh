import argparse
import contextlib
import importlib.util
import io
import pathlib
import unittest
from unittest import mock


ROOT = pathlib.Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("restart_installer", ROOT / "scripts/install.py")
installer = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(installer)


class RestartTests(unittest.TestCase):
    def test_system_service_detection_requires_unique_system_cgroup_and_main_pid(self):
        binary = pathlib.Path('/fixture/cli-proxy-api')
        process = {'pid': 123}
        valid = {'MainPID': '123', 'ExecStart': str(binary), 'name': 'fixture-cpa.service'}
        with mock.patch.object(pathlib.Path, 'read_text', return_value='0::/system.slice/fixture-cpa.service\n'), \
                mock.patch.object(installer, 'service_info', return_value=valid) as read:
            self.assertEqual(installer.detect_service(process, binary), valid)
        read.assert_called_once_with('fixture-cpa.service')
        for group in ['0::/user.slice/user-1000.slice/app.slice/fixture.service',
                      '0::/system.slice/docker-test.scope',
                      '0::/system.slice/one.service\n1:cpu:/system.slice/two.service',
                      '0::/system.slice/invalid\\x20name.service']:
            with mock.patch.object(pathlib.Path, 'read_text', return_value=group), \
                    mock.patch.object(installer, 'service_info') as read:
                self.assertIsNone(installer.detect_service(process, binary))
            read.assert_not_called()
        for info in [dict(valid, MainPID='456'), dict(valid, ExecStart='/fixture/supervisor')]:
            with mock.patch.object(pathlib.Path, 'read_text', return_value='0::/system.slice/fixture-cpa.service'), \
                    mock.patch.object(installer, 'service_info', return_value=info):
                self.assertIsNone(installer.detect_service(process, binary))
        with mock.patch.object(pathlib.Path, 'read_text', side_effect=OSError('private')):
            self.assertIsNone(installer.detect_service(process, binary))
        with mock.patch.object(pathlib.Path, 'read_text', return_value='0::/system.slice/fixture-cpa.service'), \
                mock.patch.object(installer, 'service_info', side_effect=installer.InstallError('no systemd')):
            self.assertIsNone(installer.detect_service(process, binary))

    def test_system_service_restart_is_explicit_and_defaults_to_no(self):
        plan = {'service': {'name': 'fixture-cpa.service'}}
        for answer in ['', 'no', 'yes']:
            args = argparse.Namespace(yes=False, restart=False, uninstall=False)
            with mock.patch.object(installer.sys.stdin, 'isatty', return_value=True), \
                    mock.patch('builtins.input', return_value=answer) as ask, \
                    mock.patch.object(installer, 'run', return_value='active') as run, \
                    contextlib.redirect_stdout(io.StringIO()) as output:
                installer.finish(args, plan)
            ask.assert_called_once()
            self.assertIn('现在重启 CPA 服务', ask.call_args.args[0])
            if answer == 'yes':
                self.assertEqual(run.call_args_list, [mock.call('systemctl', 'restart', 'fixture-cpa.service'),
                                                     mock.call('systemctl', 'is-active', 'fixture-cpa.service')])
            else:
                run.assert_not_called()
                self.assertIn('尚未重启', output.getvalue())

    def test_noninteractive_install_only_restarts_with_restart_flag(self):
        for restart in [False, True]:
            args = argparse.Namespace(yes=True, restart=restart, uninstall=False)
            with mock.patch('builtins.input', side_effect=AssertionError('no prompt')), \
                    mock.patch.object(installer, 'run', return_value='active') as run, \
                    contextlib.redirect_stdout(io.StringIO()):
                installer.finish(args, {'service': {'name': 'fixture-cpa.service'}})
            self.assertEqual(run.call_count, 2 if restart else 0)

    def test_unknown_startup_mode_prompts_manual_restart_without_claiming_success(self):
        args = argparse.Namespace(yes=False, restart=False, uninstall=False)
        for answer in ['yes', 'no']:
            with mock.patch.object(installer.sys.stdin, 'isatty', return_value=True), \
                    mock.patch('builtins.input', return_value=answer) as ask, \
                    mock.patch.object(installer, 'run', side_effect=AssertionError('no process mutation')), \
                    contextlib.redirect_stdout(io.StringIO()) as output:
                installer.finish(args, {'service': None, 'env': pathlib.Path('/fixture/window-refresh.env')})
            self.assertIn('手动重启 CPA', ask.call_args.args[0])
            self.assertNotIn('CPA 服务已重启', output.getvalue())
            self.assertIn('另一终端' if answer == 'yes' else '稍后手动重启', output.getvalue())

    def test_inactive_service_is_reported_as_restart_failure(self):
        args = argparse.Namespace(yes=True, restart=True, uninstall=False)
        with mock.patch.object(installer, 'run', return_value='inactive'), \
                contextlib.redirect_stdout(io.StringIO()) as output:
            with self.assertRaises(installer.InstallError):
                installer.finish(args, {'service': {'name': 'fixture-cpa.service'}})
        self.assertNotIn('CPA 服务已重启', output.getvalue())


if __name__ == '__main__':
    unittest.main()
