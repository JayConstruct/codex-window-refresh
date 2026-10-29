"""Actual dlopen -> private native RPC -> Go daemon -> SQLite smoke tests.

The fake host exposes local fixture accounts with missing credentials and opt-in
in-memory model streams, so this suite cannot issue upstream HTTP or model requests. Successful HTTP normalization
is separately covered by executor/source_test.go with an injected transport.
"""
import base64
import ctypes as C
import contextlib
import hashlib
import http.server
import http.client
import importlib.util
import json
import os
import pwd
from pathlib import Path
import socket
import socketserver
import shlex
import struct
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest import mock

ROOT = Path(__file__).resolve().parents[3]
LIBRARY = ROOT / 'bin/codex-window-refresh-v2-bridge.so'
DAEMON = ROOT / 'bin/window-refreshd'

class Buffer(C.Structure):
    _fields_ = [('ptr', C.c_void_p), ('len', C.c_size_t)]

HostCall = C.CFUNCTYPE(C.c_int, C.c_void_p, C.c_char_p, C.c_void_p, C.c_size_t, C.POINTER(Buffer))
Free = C.CFUNCTYPE(None, C.c_void_p, C.c_size_t)
PluginCall = C.CFUNCTYPE(C.c_int, C.c_char_p, C.c_void_p, C.c_size_t, C.POINTER(Buffer))
Shutdown = C.CFUNCTYPE(None)

class HostAPI(C.Structure):
    _fields_ = [('version', C.c_uint32), ('ctx', C.c_void_p), ('call', HostCall), ('free', Free)]

class PluginAPI(C.Structure):
    _fields_ = [('version', C.c_uint32), ('call', PluginCall), ('free', Free), ('shutdown', Shutdown)]

class NativeBridge(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='cwr-v2-')
        self.root = Path(self.temp.name)
        self.runtime = self.root / 'run'
        self.runtime.mkdir(mode=0o700)
        self.previous_runtime = os.environ.get('CWR_V2_RUNTIME_DIR')
        os.environ['CWR_V2_RUNTIME_DIR'] = str(self.runtime)
        self.libc = C.CDLL(None)
        self.libc.malloc.argtypes = [C.c_size_t]
        self.libc.malloc.restype = C.c_void_p
        self.libc.free.argtypes = [C.c_void_p]
        self.calls = 0
        self.frees = 0
        self.models = 0
        self.expected_models = 0
        self.allow_models = False
        self.model_reads = 0
        self.model_closes = 0
        self.model_streams = {}
        self.model_requests = []
        self.model_error = False
        self.block_model_read = False
        self.block_model_close = False
        self.model_close_error = False
        self.antigravity_accounts = False
        self.allow_quota_http = False
        self.quota_operations = {}
        self.quota_opened = 0
        self.quota_requests = []
        self.block_quota_http = False
        self.quota_lock = threading.Lock()
        self.quota_cancel_failures = 0
        self.quota_cancel_attempts = 0
        self.quota_cancel_entered = threading.Event()
        self.quota_cancel_release = threading.Event()
        self.quota_cancel_release.set()
        self.catalog_error = None
        self.entered = threading.Event()
        self.release = threading.Event()
        self.release.set()
        self.host_call = HostCall(self.callback)
        self.host_free = Free(self.free)
        self.context = C.c_int(73)
        self.lib = C.CDLL(str(LIBRARY))
        self.lib.cliproxy_plugin_init.argtypes = [C.POINTER(HostAPI), C.POINTER(PluginAPI)]
        self.lib.cliproxy_plugin_init.restype = C.c_int
        self.api = PluginAPI()
        host = HostAPI(1, C.cast(C.pointer(self.context), C.c_void_p), self.host_call, self.host_free)
        self.assertEqual(self.lib.cliproxy_plugin_init(C.byref(host), C.byref(self.api)), 0)
        C.memset(C.byref(host), 0, C.sizeof(host))  # ABI must be copied.
        self.process = None
        self.addCleanup(self.cleanup)
        registration = self.call('plugin.register', {'schema_version': 6})
        self.assertEqual(registration["result"]["schema_version"], 6)
        self.assertNotIn("request_interceptor_omit_body_before", registration["result"]["capabilities"])
        self.assertTrue(registration['ok'], registration)
        self.assert_host_registration(registration)

    @staticmethod
    def routing_snapshot(projects=None, cap=8):
        now = int(time.time())
        return {'config': {'enabled': True, 'prefer_reset': True, 'max_inflight': cap,
                           'session_ttl_seconds': 1800, 'projects': projects or []},
                'accounts': [{'id': 'a', 'valid_until': now + 300, 'reset_at': now + 60, 'card_expiry': 0},
                             {'id': 'b', 'valid_until': now + 300, 'reset_at': now + 3600, 'card_expiry': 0}]}

    @staticmethod
    def routing_request(project='', session='', scope='caller'):
        return {'Candidates': [{'ID': 'a', 'Provider': 'codex'}, {'ID': 'b', 'Provider': 'codex'}],
                'Options': {'Headers': {'X-Cwr-Project': [project]},
                            'Metadata': {'session_id': session, 'caller_scope': scope}}}

    def test_scheduler_project_preference_cap_and_completion(self):
        instance = self.rpc('bridge.hello')['result']['bridge_instance_id']
        self.assertEqual(self.call('scheduler.pick', self.routing_request())['result'], {})
        snapshot = self.routing_snapshot([{'project': 'work', 'accounts': ['b']}], cap=1)
        self.assertTrue(self.rpc('bridge.scheduler.replace', snapshot, instance)['ok'])
        self.assertEqual(self.call('scheduler.pick', self.routing_request('work'))['result']['AuthID'], 'b')
        self.call('request.intercept_after', {'RequestID': 'work-request', 'Metadata': {'selected_auth_id': 'b'}})
        self.assertEqual(self.call('scheduler.pick', self.routing_request('work'))['result']['AuthID'], 'a')
        self.assertTrue(self.call('scheduler.pick', self.routing_request('work'))['result']['Reject'])
        self.call('request.complete', {'RequestID': 'work-request', 'Metadata': {}})
        self.assertEqual(self.call('scheduler.pick', self.routing_request('work'))['result']['AuthID'], 'b')
        self.assertEqual(self.models, 0)

    def test_scheduler_affinity_is_caller_scoped_and_evidence_expires(self):
        instance = self.rpc('bridge.hello')['result']['bridge_instance_id']
        snapshot = self.routing_snapshot()
        self.assertTrue(self.rpc('bridge.scheduler.replace', snapshot, instance)['ok'])
        request = self.routing_request(session='shared-session')
        self.assertEqual(self.call('scheduler.pick', request)['result']['AuthID'], 'a')
        snapshot['accounts'][1]['reset_at'] = int(time.time()) + 5
        self.assertTrue(self.rpc('bridge.scheduler.replace', snapshot, instance)['ok'])
        self.assertEqual(self.call('scheduler.pick', request)['result']['AuthID'], 'a')
        self.assertEqual(self.call('scheduler.pick', self.routing_request(session='shared-session', scope='other'))['result']['AuthID'], 'b')
        for a in snapshot['accounts']: a['valid_until'] = 1
        self.assertTrue(self.rpc('bridge.scheduler.replace', snapshot, instance)['ok'])
        self.assertEqual(self.call('scheduler.pick', request)['result'], {})
        snapshot['accounts'].reverse()
        self.assertFalse(self.rpc('bridge.scheduler.replace', snapshot, instance)['ok'])

    def test_schema7_omits_unused_interceptor_request_bodies(self):
        registration = self.call('plugin.reconfigure', {'schema_version': 7})
        self.assertTrue(registration['ok'], registration)
        result = registration['result']
        self.assertEqual(result['schema_version'], 7)
        caps = result['capabilities']
        self.assertTrue(caps['request_interceptor'])
        self.assertTrue(caps['request_interceptor_omit_body_before'])
        self.assertTrue(caps['request_interceptor_omit_body_after'])
        self.assertTrue(caps['response_stream_interceptor'])
        self.assertTrue(caps['websocket_response_observer'])
        self.assertEqual(self.call('request.intercept_before',
                                   {'RequestID': 'schema7', 'Metadata': {}}),
                         {'ok': True, 'result': {}})
        self.assertEqual(self.call('request.intercept_after',
                                   {'RequestID': 'schema7', 'Metadata': {}}),
                         {'ok': True, 'result': {}})
        previous = self.call('plugin.reconfigure', {'schema_version': 6})
        self.assertEqual(previous['result']['schema_version'], 6)
        self.assertNotIn('request_interceptor_omit_body_before',
                         previous['result']['capabilities'])
        self.assertNotIn('request_interceptor_omit_body_after',
                         previous['result']['capabilities'])

    @staticmethod
    def host_accepts_registration(registration):
        # CPA host.go validPlugin() at pinned upstream 3de4e248f6bb12fc7dfafb183404f3b248578922
        # requires all four metadata strings before publishing capabilities/routes.
        result = registration.get('result', {})
        metadata = result.get('metadata', {})
        return (registration.get('ok') is True and
                all(isinstance(metadata.get(key), str) and metadata[key].strip()
                    for key in ('Name', 'Version', 'Author', 'GitHubRepository')) and
                result.get('capabilities', {}).get('management_api') is True)

    @staticmethod
    def quota_request(operation):
        return {'operation_id': operation, 'method': 'GET',
                'url': 'https://chatgpt.com/backend-api/wham/usage',
                'headers': {'Authorization': ['Bearer fixture-token'],
                            'Chatgpt-Account-Id': ['fixture-upstream'],
                            'Accept': ['application/json'], 'User-Agent': ['codex-window-refresh-v2']}}

    def test_quota_http_is_fixed_endpoint_owned_and_one_shot(self):
        self.allow_quota_http = True
        instance = self.rpc('bridge.hello')['result']['bridge_instance_id']
        operation = self.rpc('host.http.operation_open', {}, instance)['result']['operation_id']
        request = self.quota_request(operation)
        for change in ({'url': 'https://example.invalid/'}, {'method': 'POST'},
                       {'body': 'e30='}, {'operation_id': 'foreign-operation'},
                       {'headers': {**request['headers'], 'X-Management-Key': ['private']}}):
            self.assertFalse(self.rpc('host.http.do', {**request, **change}, instance)['ok'])
        self.assertEqual(self.quota_requests, [])
        result = self.rpc('host.http.do', request, instance)
        self.assertTrue(result['ok'], result)
        self.assertEqual(json.loads(base64.b64decode(result['result']['Body'])), {'rate_limit': {'allowed': True}})
        self.assertFalse(self.rpc('host.http.do', request, instance)['ok'])
        self.assertTrue(self.rpc('host.http.cancel', {'operation_id': operation}, instance)['ok'])
        self.assertEqual(len(self.quota_requests), 1)

    def test_quota_http_cancel_survives_all_eight_blocked_requests(self):
        self.allow_quota_http = self.block_quota_http = True
        instance = self.rpc('bridge.hello')['result']['bridge_instance_id']
        operations = [self.rpc('host.http.operation_open', {}, instance)['result']['operation_id'] for _ in range(8)]
        results = []
        threads = [threading.Thread(target=lambda operation=operation: results.append(
            self.rpc('host.http.do', self.quota_request(operation), instance))) for operation in operations]
        for thread in threads:
            thread.start()
        deadline = time.monotonic()+3
        while len(self.quota_requests) < 8 and time.monotonic() < deadline:
            time.sleep(.01)
        self.assertEqual(len(self.quota_requests), 8)
        self.assertEqual(self.rpc('host.auth.list', {}, instance)['error']['code'], 'bridge_busy')
        for operation in operations:
            self.assertTrue(self.rpc('host.http.cancel', {'operation_id': operation}, instance)['ok'])
        for thread in threads:
            thread.join(3)
            self.assertFalse(thread.is_alive())
        self.assertEqual(len(results), 8)
        self.assertTrue(all(result['ok'] for result in results), results)

    def test_quota_http_expiry_and_quiesce_release_abandoned_handles(self):
        self.allow_quota_http = True
        instance = self.rpc('bridge.hello')['result']['bridge_instance_id']
        self.rpc('host.http.operation_open', {}, instance, timeout_ms=1)
        deadline = time.monotonic()+3
        while self.quota_operations and time.monotonic() < deadline:
            time.sleep(.02)
        self.assertFalse(self.quota_operations)
        operation = self.rpc('host.http.operation_open', {}, instance)['result']['operation_id']
        self.block_quota_http = True
        result = []
        thread = threading.Thread(target=lambda: result.append(self.rpc('host.http.do', self.quota_request(operation), instance)))
        thread.start()
        self.assertTrue(self.entered.wait(3))
        self.assertTrue(self.call('plugin.quiesce', {})['ok'])
        thread.join(3)
        self.assertFalse(thread.is_alive())
        self.assertFalse(self.quota_operations)
        self.assertTrue(result[0]['ok'], result)

    def test_quota_http_failed_reaper_cancel_keeps_ownership_until_retry(self):
        self.allow_quota_http = True
        self.quota_cancel_failures = 1
        self.quota_cancel_release.clear()
        instance = self.rpc('bridge.hello')['result']['bridge_instance_id']
        operation = self.rpc('host.http.operation_open', {}, instance, timeout_ms=1)['result']['operation_id']
        self.assertTrue(self.quota_cancel_entered.wait(3), 'reaper did not attempt expired operation cleanup')
        try:
            for method, payload in (('host.http.cancel', {'operation_id': operation}),
                                    ('host.http.do', self.quota_request(operation))):
                reply = self.rpc(method, payload, instance)
                self.assertEqual(reply['error']['code'], 'bridge_busy', reply)
            with self.quota_lock:
                self.assertIn(operation, self.quota_operations)
            # In-progress cancellation still occupies its slot. A failed host
            # callback must not make that slot available to an unrelated open.
            others = [self.rpc('host.http.operation_open', {}, instance)['result']['operation_id'] for _ in range(7)]
            self.assertEqual(self.rpc('host.http.operation_open', {}, instance)['error']['code'], 'bridge_busy')
            self.assertEqual(self.quota_requests, [], 'cleanup race dispatched HTTP')
        finally:
            self.quota_cancel_release.set()
        deadline = time.monotonic()+4
        while time.monotonic() < deadline:
            with self.quota_lock:
                pending = operation in self.quota_operations
            if not pending:
                break
            time.sleep(.01)
        self.assertFalse(pending, 'failed cancellation was not retried')
        self.assertGreaterEqual(self.quota_cancel_attempts, 2)
        # The fixture removes the host handle before its native callback returns;
        # allow the native reaper to finish its matching ownership transition.
        replacement = None
        while time.monotonic() < deadline:
            reply = self.rpc('host.http.operation_open', {}, instance)
            if reply['ok']:
                replacement = reply['result']['operation_id']
                break
            self.assertEqual(reply['error']['code'], 'bridge_busy', reply)
            time.sleep(.01)
        self.assertIsNotNone(replacement, 'released slot remained occupied')
        self.assertEqual(self.rpc('host.http.cancel', {'operation_id': operation}, instance)['error']['code'], 'quota_operation_unavailable')
        for handle in others+[replacement]:
            self.assertTrue(self.rpc('host.http.cancel', {'operation_id': handle}, instance)['ok'])

    def test_quota_http_reaper_does_not_truncate_the_monotonic_deadline(self):
        self.allow_quota_http = True
        instance = self.rpc('bridge.hello')['result']['bridge_instance_id']
        # Open near a monotonic second boundary. Dropping the fractional part
        # when adding a 1000 ms budget would expire this handle in ~200 ms.
        time.sleep((.8-time.monotonic() % 1) % 1)
        started = time.monotonic()
        operation = self.rpc('host.http.operation_open', {}, instance, timeout_ms=1000)['result']['operation_id']
        time.sleep(max(0, started+.4-time.monotonic()))
        # Wake the reaper after the incorrectly truncated deadline, rather than
        # depending on the phase of its normal one-second timer.
        self.assertTrue(self.rpc('host.auth.list', {}, instance)['ok'])
        checked = time.monotonic()
        self.assertLess(checked-started, 1, 'fixture scheduling exceeded the operation budget')
        until = min(started+.65, checked+.15)
        while time.monotonic() < until:
            with self.quota_lock:
                self.assertIn(operation, self.quota_operations, 'reaper cancelled before the full monotonic deadline')
            time.sleep(.01)
        self.assertTrue(self.rpc('host.http.cancel', {'operation_id': operation}, instance)['ok'])

    def test_daemon_quota_query_uses_owned_host_http_without_model_calls(self):
        self.allow_quota_http = True
        self.start_daemon()
        status, accepted = self.management('jobs', 'POST', {'kind': 'quota_query', 'account_id': 'a'}, 'host-quota-query')
        self.assertEqual(status, 202)
        deadline = time.monotonic()+5
        job = None
        while time.monotonic() < deadline:
            status, job = self.management('jobs', query={'job_id': [accepted['job_id']]})
            self.assertEqual(status, 200)
            if job['phase'] in ('completed', 'failed', 'cancelled'):
                break
            time.sleep(.02)
        self.assertEqual(job['phase'], 'completed', job)
        self.assertEqual(len(job['items']), 1, job)
        self.assertEqual((job['items'][0]['account_id'], job['items'][0]['phase'], job['items'][0].get('error_code', '')),
                         ('a', 'completed', ''), job)
        self.assertEqual(len(self.quota_requests), 1)
        self.assertEqual(self.quota_requests[0]['headers']['Authorization'], ['Bearer fixture-quota-token'])
        self.assertEqual(self.quota_requests[0]['headers']['Chatgpt-Account-Id'], ['fixture-upstream'])
        with self.quota_lock:
            self.assertFalse(self.quota_operations, 'query completed before its HTTP handle was released')
        status, page = self.management('accounts')
        self.assertEqual(status, 200)
        account = next(account for account in page['accounts'] if account['id'] == 'a')
        self.assertEqual((account['quota']['permission'], account['quota']['source']), ('allowed', 'cpa_wham_usage'))
        status, replay = self.management('jobs', 'POST', {'kind': 'quota_query', 'account_id': 'a'}, 'host-quota-query')
        self.assertEqual((status, replay['job_id']), (202, accepted['job_id']))
        self.assertEqual(len(self.quota_requests), 1, 'read-only task replay queried upstream again')
        self.assertEqual(self.models, 0)

    def test_account_models_are_scoped_and_unsupported_is_precise(self):
        instance = self.rpc('bridge.hello')['result']['bridge_instance_id']
        self.assertFalse(self.rpc('host.auth.models', {}, instance)['ok'])
        self.assertFalse(self.rpc('host.auth.models', {'auth_index': 'a', 'extra': True}, instance)['ok'])
        result = self.rpc('host.auth.models', {'auth_index': 'a'}, instance)
        self.assertEqual(result['result']['credentials'][0]['models'], ['fixture-model'])
        self.catalog_error = 'unsupported host callback host.auth.models'
        self.assertEqual(self.rpc('host.auth.models', {'auth_index': 'a'}, instance)['error']['code'], 'model_catalog_unsupported')
        self.catalog_error = 'private token text'
        failure = self.rpc('host.auth.models', {'auth_index': 'a'}, instance)
        self.assertEqual(failure['error']['code'], 'host_callback_failed')
        self.assertNotIn('private token', json.dumps(failure))
        self.catalog_error = None
        self.start_daemon()
        status, catalog = self.management('account-models', query={'account_id': ['a']})
        self.assertEqual(status, 200, catalog)
        self.assertEqual(catalog['models'], ['fixture-model'])

    def assert_host_registration(self, registration):
        self.assertTrue(self.host_accepts_registration(registration), registration)
        routes = self.call('management.register', {})
        self.assertTrue(routes['ok'], routes)
        menus = [r for r in routes['result']['Resources'] if r.get('Menu')]
        self.assertEqual(len(menus), 1)
        self.assertEqual(menus[0]['Path'], '/status')
        self.assertIn('/healthz', [r['Path'] for r in routes['result']['Resources']])
        self.assertIn({'Method': 'POST', 'Path': '/v0/management/codex-window-refresh/v2/sync'},
                      routes['result']['Routes'])
        for endpoint in ('model-settings', 'prompt-generations'):
            for method in ('GET', 'POST'):
                self.assertIn({'Method': method, 'Path': '/v0/management/codex-window-refresh/v2/'+endpoint},
                              routes['result']['Routes'])
        self.assertIn({'Method': 'GET', 'Path': '/v0/management/codex-window-refresh/v2/generation-accounts'},
                      routes['result']['Routes'])
        self.assertNotIn({'Method': 'POST', 'Path': '/v0/management/codex-window-refresh/v2/generation-accounts'},
                         routes['result']['Routes'])
        self.assertIn({'Method': 'GET', 'Path': '/v0/management/codex-window-refresh/v2/task-history'},
                      routes['result']['Routes'])
        self.assertNotIn({'Method': 'POST', 'Path': '/v0/management/codex-window-refresh/v2/task-history'},
                         routes['result']['Routes'])

    @contextlib.contextmanager
    def cpa_resources(self, registration=None):
        # Contract fixture: actual native responses and daemon; public HTTP dispatch
        # is enabled only after the host metadata gate and resource registration.
        registration = registration or self.call('plugin.register', {'schema_version': 6})
        accepted = self.host_accepts_registration(registration)
        routes = self.call('management.register', {})['result']['Resources'] if accepted else []
        prefix = '/v0/resource/plugins/codex-window-refresh'
        paths = {prefix + r['Path'] for r in routes}
        bridge = self
        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(inner):
                if inner.path not in paths:
                    inner.send_response(404)
                    inner.end_headers()
                    return
                result = bridge.call('management.handle', {'Method': 'GET', 'Path': inner.path})['result']
                body = base64.b64decode(result['Body'])
                inner.send_response(result['StatusCode'])
                for name, values in result.get('Headers', {}).items():
                    for value in values:
                        inner.send_header(name, value)
                inner.send_header('Content-Length', str(len(body)))
                inner.end_headers()
                inner.wfile.write(body)
            def log_message(self, *args):
                pass
        with http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler) as server:
            worker = threading.Thread(target=lambda: server.serve_forever(poll_interval=.02))
            worker.start()
            try:
                yield f'http://127.0.0.1:{server.server_port}'
            finally:
                server.shutdown()
                worker.join()

    def test_registration_gate_and_installer_reject_live_but_unregistered_bridge(self):
        spec = importlib.util.spec_from_file_location('v2_registration_installer', ROOT/'scripts/install-v2.py')
        installer = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(installer)
        self.start_daemon()
        self.wait_activity(0)
        version = (ROOT/'release/v2-version.txt').read_text().strip()
        bridge_version = 'v' + self.rpc('bridge.hello')['result']['version']
        for omitted in ('Author', 'GitHubRepository'):
            broken = self.call('plugin.register', {'schema_version': 6})
            del broken['result']['metadata'][omitted]
            with self.cpa_resources(broken) as url:
                # The original installer accepted both live sockets in this state.
                with self.assertRaisesRegex(installer.InstallError, 'CPA 未注册'):
                    installer.probe(self.runtime, version, os.getpid(), bridge_version,
                                    url+'/v0/resource/plugins/codex-window-refresh/healthz')
        with self.cpa_resources() as url:
            ready = installer.probe(self.runtime, version, os.getpid(), bridge_version,
                                    url+'/v0/resource/plugins/codex-window-refresh/healthz')
            self.assertEqual(ready['version'], version)
            with contextlib.closing(http.client.HTTPConnection('127.0.0.1', int(url.rsplit(':', 1)[1]))) as conn:
                conn.request('GET', '/v0/resource/plugins/codex-window-refresh/status')
                response = conn.getresponse()
                self.assertEqual(response.status, 200)
                self.assertIn(b'<html', response.read().lower())

    def cleanup(self):
        self.release.set()
        self.model_close_error = False
        self.quota_cancel_failures = 0
        self.quota_cancel_release.set()
        if self.process:
            self.process.terminate()
            self.process.communicate(timeout=10)
        self.api.shutdown()
        self.assertEqual(self.calls, self.frees, 'host-owned buffer leak')
        self.assertEqual(self.models, self.expected_models, 'unexpected model execute side effect')
        self.assertFalse(self.model_streams, 'unreleased fixture model streams')
        self.assertFalse(self.quota_operations, 'unreleased fixture HTTP operations')
        # All native threads must be joined before dlclose. This is a C library,
        # without RTLD_NODELETE or a retained Go runtime.
        import _ctypes
        _ctypes.dlclose(self.lib._handle)
        if self.previous_runtime is None:
            os.environ.pop('CWR_V2_RUNTIME_DIR', None)
        else:
            os.environ['CWR_V2_RUNTIME_DIR'] = self.previous_runtime
        self.temp.cleanup()

    def free(self, ptr, size):
        self.frees += 1
        self.libc.free(ptr)

    def callback(self, ctx, method, request, size, result):
        self.calls += 1
        method = method.decode()
        req = json.loads(C.string_at(request, size))
        success = True
        if method.startswith('host.model.'):
            self.assertTrue(self.allow_models, 'model callback without explicit fixture opt-in')
            if method == 'host.model.execute_stream':
                self.models += 1
                self.model_requests.append(req)
                stream_id = f'fixture-model-{self.models}'
                self.model_streams[stream_id] = 0
                value = {'stream_id': stream_id, 'status_code': 200, 'headers': {}}
            elif method == 'host.model.stream_read':
                self.model_reads += 1
                self.assertIn(req['stream_id'], self.model_streams)
                if self.block_model_read:
                    self.entered.set()
                    self.release.wait(20)
                payload = ('data: '+json.dumps({'type': 'response.output_text.delta', 'delta': '["清风入怀","星光相伴"]'})+'\n\n'
                           'data: '+json.dumps({'type': 'response.completed', 'response': {'status': 'completed'}})+'\n\n')
                value = {'payload': base64.b64encode(payload.encode()).decode(), 'done': True,
                         'error': 'secret-upstream-token' if self.model_error else ''}
            elif method == 'host.model.stream_close':
                self.model_closes += 1
                self.assertIn(req['stream_id'], self.model_streams)
                if self.block_model_close:
                    self.entered.set()
                    self.release.wait(20)
                if self.model_close_error:
                    success = False
                    value = {'error': 'secret-close-token'}
                else:
                    del self.model_streams[req['stream_id']]
                    self.release.set()
                    value = {}
            else:
                return 1
        elif method.startswith('host.http.'):
            self.assertTrue(self.allow_quota_http, 'HTTP callback without fixture opt-in')
            if method == 'host.http.operation_open':
                self.quota_opened += 1
                operation = f'fixture-http-{self.quota_opened}'
                with self.quota_lock:
                    self.quota_operations[operation] = threading.Event()
                value = {'operation_id': operation}
            elif method == 'host.http.do':
                with self.quota_lock:
                    event = self.quota_operations[req['operation_id']]
                    self.quota_requests.append(req)
                if self.block_quota_http:
                    self.entered.set()
                    event.wait(10)
                value = {'StatusCode': 200, 'Body': base64.b64encode(b'{"rate_limit":{"allowed":true}}').decode()}
            elif method == 'host.http.cancel':
                with self.quota_lock:
                    self.quota_cancel_attempts += 1
                    failed = self.quota_cancel_failures > 0
                    if failed:
                        self.quota_cancel_failures -= 1
                if failed:
                    self.quota_cancel_entered.set()
                    self.quota_cancel_release.wait(10)
                    success = False
                    value = {'error': 'fixture-quota-cancel-failed'}
                else:
                    with self.quota_lock:
                        event = self.quota_operations.pop(req['operation_id'], None)
                    if event:
                        event.set()
                    value = {}
            else:
                return 1
        elif method == 'host.auth.models':
            if self.catalog_error:
                raw = json.dumps({'ok': False, 'error': {'code': 'host_callback_failed', 'message': self.catalog_error}}).encode()
                result.contents.ptr = self.libc.malloc(len(raw))
                result.contents.len = len(raw)
                C.memmove(result.contents.ptr, raw, len(raw))
                return 0
            value = {'credentials': [{'id': req['auth_index'], 'auth_index': req['auth_index'],
                                     'provider': 'codex', 'models': ['fixture-model']}]}
        elif method == 'host.auth.list':
            value = {'files': [{'id': name, 'auth_index': name, 'provider': 'codex'} for name in ('a', 'b')]}
            if self.antigravity_accounts:
                value['files'].append({'id': 'antig', 'auth_index': 'antig-index', 'provider': 'antigravity'})
        elif method == 'host.auth.get_runtime':
            if req['auth_index'] == 'slow':
                self.entered.set()
                self.release.wait(20)
            value = {'auth': {'id': req['auth_index'], 'auth_index': req['auth_index'], 'provider': 'codex'}}
            if self.antigravity_accounts and req['auth_index'] == 'antig-index':
                value = {'auth': {'id': 'antig', 'auth_index': 'antig-index', 'provider': 'antigravity',
                                  'email': 'fixture@example.invalid', 'project_id': 'fixture-project'}}
        elif method == 'host.auth.get':
            value = {'auth_index': req['auth_index'], 'json': {'type': 'codex', 'account_id': 'fixture-upstream'}}
            if self.allow_quota_http:
                value['json']['access_token'] = 'fixture-quota-token'
            if self.antigravity_accounts and req['auth_index'] == 'antig-index':
                value = {'auth_index': 'antig-index', 'json': {'type': 'antigravity', 'email': 'fixture@example.invalid',
                         'project_id': 'fixture-project', 'access_token': 'private-fixture-token', 'refresh_token': 'private-fixture-refresh'}}
        else:
            return 1
        raw = json.dumps({'ok': success, 'result': value}).encode()
        result.contents.ptr = self.libc.malloc(len(raw))
        result.contents.len = len(raw)
        C.memmove(result.contents.ptr, raw, len(raw))
        return 0

    def call(self, method, payload):
        raw = json.dumps(payload).encode()
        result = Buffer()
        status = self.api.call(method.encode(), C.c_char_p(raw), len(raw), C.byref(result))
        try:
            self.assertEqual(status, 0)
            return json.loads(C.string_at(result.ptr, result.len))
        finally:
            self.api.free(result.ptr, result.len)

    def rpc(self, method, payload=None, instance='', timeout_ms=15000):
        request = json.dumps({'protocol_version': 1, 'request_id': 'fixture', 'bridge_instance_id': instance,
                              'remaining_timeout_ms': timeout_ms, 'method': method, 'payload': payload or {}}).encode()
        with socket.socket(socket.AF_UNIX) as conn:
            conn.settimeout(15)
            conn.connect(str(self.runtime / 'bridge.sock'))
            conn.sendall(struct.pack('!I', len(request)) + request)
            size = struct.unpack('!I', self.read(conn, 4))[0]
            self.assertLessEqual(size, 4 << 20)
            return json.loads(self.read(conn, size))

    @staticmethod
    def read(conn, size):
        data = b''
        while len(data) < size:
            chunk = conn.recv(size-len(data))
            if not chunk:
                raise EOFError('truncated frame')
            data += chunk
        return data

    def start_daemon(self):
        self.process = subprocess.Popen([str(DAEMON), '--runtime-dir', str(self.runtime), '--data-dir', str(self.root/'data'), '--query-concurrency', '8'], stderr=subprocess.PIPE)
        deadline = time.monotonic()+5
        while time.monotonic()<deadline:
            if self.process.poll() is not None:
                self.fail(self.process.stderr.read().decode())
            if (self.runtime/'daemon.sock').exists():
                return
            time.sleep(.02)
        self.fail('daemon did not listen')

    def management(self, path, method='GET', body=None, key=None, query=None):
        request = {'Method': method, 'Path': '/v0/management/codex-window-refresh/v2/'+path,
                   'Headers': {'Authorization': ['Bearer must-not-forward']}, 'Query': query or {}}
        if body is not None:
            request['Body'] = base64.b64encode(json.dumps(body).encode()).decode()
        if key:
            request['Headers']['Idempotency-Key'] = [key]
        response = self.call('management.handle', request)
        self.assertTrue(response['ok'], response)
        result = response['result']
        return result['StatusCode'], json.loads(base64.b64decode(result['Body']))

    def stop_daemon(self):
        self.process.terminate()
        _, stderr = self.process.communicate(timeout=10)
        self.assertEqual(self.process.returncode, 0, stderr)
        self.process = None

    def wait_activity(self, active, unknown=0):
        deadline = time.monotonic()+5
        while time.monotonic()<deadline:
            status, overview = self.management('overview')
            activity = overview.get('activity', {})
            if (status == 200 and activity.get('connected') and
                    activity.get('observed_active_requests') == active and
                    activity.get('unknown_requests') == unknown):
                return activity
            time.sleep(.025)
        self.fail(f'activity did not converge: {overview}')

    def test_lifecycle_replay_ack_and_payload_minimization(self):
        hello = self.rpc('bridge.hello')['result']
        self.assertTrue(hello['events_supported'])
        instance = hello['bridge_instance_id']
        self.assertTrue(hello['host_process_id'])
        metadata = {'caller_scope': 'hashed-user', 'selected_auth_id': 'a'}
        # Larger than the management/RPC limit: the hook must skip this body,
        # return no request edits, and retain only its bounded observation.
        request = {'RequestID': 'request-1', 'Metadata': metadata,
                   'Headers': {'Authorization': ['Bearer secret-token']},
                   'Body': base64.b64encode(b'secret-prompt' * 400000).decode()}
        for method in ('request.intercept_before', 'request.intercept_after'):
            self.assertEqual(self.call(method, request), {'ok': True, 'result': {}})
        self.assertTrue(self.call('request.complete', {'RequestID': 'request-1', 'Metadata': None})['ok'])
        batch = self.rpc('bridge.events.pull', {'after': 0, 'limit': 128}, instance)['result']
        replay = self.rpc('bridge.events.pull', {'after': 0, 'limit': 128}, instance)['result']
        self.assertEqual(batch, replay)
        self.assertEqual([e['sequence'] for e in batch['events']], [1, 2, 3])
        self.assertEqual([e['kind'] for e in batch['events']], ['started', 'account_selected', 'completed'])
        self.assertFalse(batch['gap'])
        for event in batch['events']:
            self.assertLessEqual(set(event), {'sequence', 'kind', 'request_id', 'identity', 'account_id', 'at'})
        self.assertNotIn('secret', json.dumps(batch))
        self.assertTrue(self.rpc('bridge.events.ack', {'through': 3}, instance)['ok'])
        empty = self.rpc('bridge.events.pull', {'after': 3}, instance)['result']
        self.assertEqual(empty['events'], [])
        self.assertEqual(empty['acknowledged_sequence'], 3)
        self.assertTrue(self.rpc('bridge.events.pull', {'after': 0}, instance)['result']['gap'])
        self.assertFalse(self.rpc('bridge.events.ack', {'through': 4}, instance)['ok'])

    def test_lifecycle_capacity_loss_and_internal_request_filter(self):
        instance = self.rpc('bridge.hello')['result']['bridge_instance_id']
        for metadata in ({'caller_scope': 'internal', 'source': 'plugin_host_model_callback'},
                         {'caller_scope': 'probe', 'generate': False}):
            self.call('request.intercept_before', {'RequestID': 'excluded', 'Metadata': metadata})
        self.assertEqual(self.rpc('bridge.events.pull', {'after': 0}, instance)['result']['latest_sequence'], 0)
        for i in range(4100):
            self.call('request.intercept_before', {'RequestID': str(i), 'Metadata': {'caller_scope': 'one-user'}})
        batch = self.rpc('bridge.events.pull', {'after': 0}, instance)['result']
        self.assertTrue(batch['gap'])
        self.assertEqual(batch['loss_epoch'], 4)
        self.assertEqual(batch['earliest_sequence'], 5)
        self.assertEqual(batch['latest_sequence'], 4100)
        self.assertEqual(len(batch['events']), 128)

    @staticmethod
    def quota_headers(used='25'):
        return {'X-Codex-Allowed': ['true'], 'X-Codex-Primary-Used-Percent': [used],
                'X-Codex-Primary-Window-Minutes': ['300'], 'X-Codex-Primary-Reset-After-Seconds': ['3000']}

    def test_passive_headers_stream_and_websocket_are_bounded_and_leave_responses_untouched(self):
        instance = self.rpc('bridge.hello')['result']['bridge_instance_id']
        bindings = {'accounts': [{'id': 'a', 'auth_index': 'a', 'binding': 'one'}]}
        self.assertTrue(self.rpc('bridge.bindings.replace', bindings, instance)['ok'])
        select = {'RequestID': 'headers', 'Metadata': {'caller_scope': 'u', 'selected_auth_id': 'a'}}
        self.call('request.intercept_after', select)
        # Completion can race ahead of the actual downstream response hook.
        self.call('request.complete', {'RequestID': 'headers'})
        headers = self.quota_headers()
        headers['Authorization'] = ['secret-header']
        response = {'RequestID': 'headers', 'ResponseHeaders': headers, 'Body': base64.b64encode(b'secret-text'*500000).decode()}
        self.assertEqual(self.call('response.intercept_after', response), {'ok': True, 'result': {}})
        self.call('response.intercept_after', response)  # Repeated header snapshot.
        self.call('request.intercept_after', {'RequestID': 'stream', 'Metadata': {'selected_auth_id': 'a'}})
        for index in [-1, 0, 1, 100]:
            self.assertEqual(self.call('response.intercept_stream_chunk', {'RequestID': 'stream', 'ChunkIndex': index, 'ResponseHeaders': headers, 'Body': 'c2VjcmV0'}), {'ok': True, 'result': {}})
        self.call('request.intercept_after', {'RequestID': 'websocket', 'Metadata': {'selected_auth_id': 'a'}})
        frame = {'type': 'codex.rate_limits', 'rate_limits': {'allowed': True, 'primary': {'usedPercent': 40, 'windowMinutes': 300, 'resetAfterSeconds': 2900, 'private': 'secret-window'}, 'private': 'secret-rate'}, 'message': 'secret-text'}
        websocket = {'RequestID': 'websocket', 'Provider': 'codex', 'AuthID': 'a', 'EventType': 'codex.rate_limits', 'Payload': base64.b64encode(json.dumps(frame).encode()).decode()}
        self.assertEqual(self.call('websocket.response_event', websocket), {'ok': True, 'result': {}})
        websocket['AuthID'] = 'another'
        self.call('websocket.response_event', websocket)
        batch = self.rpc('bridge.events.pull', {'after': 0}, instance)['result']
        observations = [e for e in batch['events'] if e['kind'] == 'quota_observed']
        self.assertEqual(len(observations), 3)
        self.assertEqual(observations[0]['binding'], 'one')
        self.assertEqual(observations[0]['headers']['x-codex-primary-used-percent'], ['25'])
        self.assertEqual(observations[2]['quota']['rate_limits']['primary']['usedPercent'], 40)
        self.assertNotIn('secret', json.dumps(batch))
        self.assertEqual(batch, self.rpc('bridge.events.pull', {'after': 0}, instance)['result'])
        for change in [{'Provider': 'claude'}, {'Metadata': {'source': 'plugin_host_model_callback'}},
                       {'EventType': 'response.delta'}, {'Payload': 'not valid base64'},
                       {'Payload': base64.b64encode(b'{"type":"codex.rate_limits","rate_limits":{"allowed":"secret"}}').decode()}]:
            self.call('websocket.response_event', dict(websocket, AuthID='a', **change))
        self.assertEqual(batch['latest_sequence'], self.rpc('bridge.events.pull', {'after': 0}, instance)['result']['latest_sequence'])
        # Updating identity invalidates a previously selected response binding.
        bindings['accounts'][0]['binding'] = 'replacement'
        self.rpc('bridge.bindings.replace', bindings, instance)
        websocket['AuthID'] = 'a'
        self.call('websocket.response_event', websocket)
        self.assertEqual(batch['latest_sequence'], self.rpc('bridge.events.pull', {'after': 0}, instance)['result']['latest_sequence'])

    def test_passive_observation_reaches_database_without_an_upstream_query(self):
        self.start_daemon()
        deadline = time.monotonic()+5
        while time.monotonic()<deadline:
            hello = self.rpc('bridge.hello')['result']
            if hello['quota_observer']['accounts'] == 2 and hello['quota_observer']['ready']:
                break
            time.sleep(.025)
        self.assertEqual(hello['quota_observer']['accounts'], 2)
        self.call('request.intercept_after', {'RequestID': 'quota', 'Metadata': {'selected_auth_id': 'a'}})
        self.call('response.intercept_after', {'RequestID': 'quota', 'ResponseHeaders': self.quota_headers()})
        deadline = time.monotonic()+5
        while time.monotonic()<deadline:
            _, page = self.management('accounts')
            account = next((a for a in page['accounts'] if a['id'] == 'a'), {})
            if account.get('quota'):
                break
            time.sleep(.025)
        self.assertEqual(account['quota']['five_hour']['Remaining']['Value'], 75)
        self.assertEqual(account['quota']['source'], 'cpa_response_headers')
        observed_at = account['observed_at']
        self.stop_daemon()
        self.start_daemon()
        _, page = self.management('accounts')
        self.assertEqual(next(a for a in page['accounts'] if a['id'] == 'a')['observed_at'], observed_at)

    def test_daemon_ingests_offline_completion_and_bridge_reload_marks_unknown(self):
        self.start_daemon()
        self.wait_activity(0)
        started = {'RequestID': 'r1', 'Metadata': {'caller_scope': 'u1'}}
        self.call('request.intercept_before', started)
        activity = self.wait_activity(1)
        self.assertEqual(activity['observed_recent_users'], 1)
        self.assertFalse(activity['baseline_complete'])
        self.stop_daemon()
        self.call('request.complete', {'RequestID': 'r1'})
        self.start_daemon()
        activity = self.wait_activity(0)
        self.assertEqual(activity['observed_recent_users'], 1)
        self.call('request.intercept_before', {'RequestID': 'r2', 'Metadata': {'caller_scope': 'u2'}})
        self.wait_activity(1)
        hello = self.rpc('bridge.hello')['result']
        self.assertTrue(self.call('plugin.quiesce', {})['ok'])
        self.assertTrue(self.call('plugin.reconfigure', {'schema_version': 6})['ok'])
        reloaded = self.rpc('bridge.hello')['result']
        self.assertNotEqual(hello['bridge_instance_id'], reloaded['bridge_instance_id'])
        self.assertEqual(hello['host_process_id'], reloaded['host_process_id'])
        activity = self.wait_activity(0, 1)
        self.assertEqual(activity['status'], 'gap')
        self.assertEqual(activity['reason'], 'bridge_restarted')

    def test_protocol_guards_and_independent_native_reads(self):
        hello = self.rpc('bridge.hello')['result']
        self.assertTrue(hello['read_only'])
        instance = hello['bridge_instance_id']
        self.assertEqual(self.rpc('host.model.execute_stream', {}, instance)['error']['code'], 'invalid_payload')
        self.assertEqual(self.rpc('host.auth.list', {}, 'old-instance')['error']['code'], 'bridge_instance_changed')
        self.release.clear()
        result = []
        worker = threading.Thread(target=lambda: result.append(self.rpc('host.auth.get_runtime', {'auth_index': 'slow'}, instance)))
        worker.start()
        self.assertTrue(self.entered.wait(3))
        self.assertTrue(self.rpc('host.auth.list', {}, instance)['ok'])
        self.release.set()
        worker.join(5)
        self.assertFalse(worker.is_alive())
        self.assertTrue(result[0]['ok'])
        with socket.socket(socket.AF_UNIX) as conn:
            conn.settimeout(3)
            conn.connect(str(self.runtime/'bridge.sock'))
            conn.sendall(struct.pack('!I', (4 << 20)+1))
            size = struct.unpack('!I', self.read(conn, 4))[0]
            self.assertEqual(json.loads(self.read(conn, size))['error']['code'], 'invalid_frame')

    @staticmethod
    def model_request():
        return {'forced_provider': 'codex', 'auth_id': 'a', 'entry_protocol': 'openai-response',
                'exit_protocol': 'codex', 'stream': True, 'model': 'fixture-model',
                'body': base64.b64encode(b'{"model":"fixture-model","stream":true}').decode()}

    def test_manual_model_stream_pin_validation_ownership_and_redaction(self):
        hello = self.rpc('bridge.hello')['result']
        self.assertTrue(hello['prompt_generation_supported'])
        self.assertEqual(hello['prompt_generation_providers'], ['codex', 'antigravity'])
        instance = hello['bridge_instance_id']
        original = self.model_request()
        for field in ('forced_provider', 'auth_id', 'entry_protocol', 'exit_protocol', 'stream', 'model', 'body'):
            request = original.copy()
            del request[field]
            self.assertEqual(self.rpc('host.model.execute_stream', request, instance)['error']['code'], 'invalid_payload')
        for field, value in [('forced_provider', 'antigravity'), ('forced_provider', 'unknown'), ('auth_id', ''), ('auth_id', 'a'*257),
                             ('entry_protocol', 'chat'), ('exit_protocol', 'openai-response'),
                             ('stream', False), ('stream', 1), ('model', '   '), ('model', 'has space'),
                             ('model', 'has\ttab'), ('model', 'a'*201),
                             ('body', '!bad'), ('body', 'a'*65540), ('body', ''), ('body', 'A===')]:
            request = dict(original, **{field: value})
            self.assertEqual(self.rpc('host.model.execute_stream', request, instance)['error']['code'], 'invalid_payload')
        for method in ('host.model.execute', 'host.model.list', 'host.stream.close'):
            self.assertEqual(self.rpc(method, original, instance)['error']['code'], 'method_not_allowed')
        for method in ('host.model.stream_read', 'host.model.stream_close'):
            self.assertEqual(self.rpc(method, {'stream_id': 'other-plugin-stream'}, instance)['error']['code'], 'model_stream_unavailable')
            self.assertEqual(self.rpc(method, {'stream_id': ''}, instance)['error']['code'], 'invalid_payload')
        self.assertEqual(self.calls, 0)
        self.allow_models = True
        self.expected_models = 1
        opened = self.rpc('host.model.execute_stream', original, instance)
        self.assertTrue(opened['ok'], opened)
        stream = {'stream_id': opened['result']['stream_id']}
        self.assertEqual(self.model_requests, [original])
        self.assertTrue(self.rpc('host.model.stream_read', stream, instance)['ok'])
        self.model_error = True
        failed = self.rpc('host.model.stream_read', stream, instance)
        self.assertEqual(failed['error']['code'], 'host_callback_failed')
        self.assertNotIn('secret', json.dumps(failed))
        self.assertTrue(self.rpc('host.model.stream_close', stream, instance)['ok'])
        self.assertEqual(self.rpc('host.model.stream_close', stream, instance)['error']['code'], 'model_stream_unavailable')
        self.assertEqual((self.models, self.model_reads, self.model_closes), (1, 2, 1))

    def test_model_stream_quiesce_closes_and_unblocks_read_without_reexecute(self):
        instance = self.rpc('bridge.hello')['result']['bridge_instance_id']
        self.allow_models = True
        self.expected_models = 1
        stream = self.rpc('host.model.execute_stream', self.model_request(), instance)['result']['stream_id']
        self.block_model_read = True
        self.release.clear()
        replies = []
        worker = threading.Thread(target=lambda: replies.append(self.rpc('host.model.stream_read', {'stream_id': stream}, instance)))
        worker.start()
        self.assertTrue(self.entered.wait(3))
        started = time.monotonic()
        self.assertTrue(self.call('plugin.quiesce', {})['ok'])
        self.assertLess(time.monotonic()-started, 5)
        worker.join(3)
        self.assertFalse(worker.is_alive())
        self.assertTrue(replies[0]['ok'])
        self.assertEqual((self.models, self.model_reads, self.model_closes), (1, 1, 1))
        self.assertTrue(self.call('plugin.register', {'schema_version': 6})['ok'])
        new_instance = self.rpc('bridge.hello')['result']['bridge_instance_id']
        self.assertNotEqual(instance, new_instance)
        self.assertEqual(self.rpc('host.model.stream_read', {'stream_id': stream}, new_instance)['error']['code'], 'model_stream_unavailable')
        self.assertEqual(self.models, 1)

    def test_antigravity_stream_exact_provider_protocol_and_no_fallback(self):
        hello = self.rpc('bridge.hello')['result']
        instance = hello['bridge_instance_id']
        original = dict(self.model_request(), forced_provider='antigravity', auth_id='antig',
                        exit_protocol='openai-response', model='fixture-antigravity-model')
        for provider, exit_protocol in [('antigravity', 'codex'), ('codex', 'openai-response'),
                                        ('unknown', 'openai-response'), ('unknown', 'codex'),
                                        ('Antigravity', 'openai-response')]:
            request = dict(original, forced_provider=provider, exit_protocol=exit_protocol)
            self.assertEqual(self.rpc('host.model.execute_stream', request, instance)['error']['code'], 'invalid_payload')
        for field, value in [('entry_protocol', 'codex'), ('entry_protocol', 'openai-chat'),
                             ('stream', False), ('auth_id', ''), ('auth_id', 'antig\n'),
                             ('body', 'not-base64'), ('model', 'has space')]:
            self.assertEqual(self.rpc('host.model.execute_stream', dict(original, **{field: value}), instance)['error']['code'],
                             'invalid_payload')
        self.assertEqual((self.calls, self.models), (0, 0))
        self.allow_models = True
        self.expected_models = 1
        opened = self.rpc('host.model.execute_stream', original, instance)
        self.assertTrue(opened['ok'], opened)
        stream = {'stream_id': opened['result']['stream_id']}
        self.model_error = True
        failed = self.rpc('host.model.stream_read', stream, instance)
        self.assertEqual(failed['error']['code'], 'host_callback_failed')
        self.assertNotIn('secret', json.dumps(failed))
        self.assertTrue(self.rpc('host.model.stream_close', stream, instance)['ok'])
        self.assertEqual(self.model_requests, [original])
        self.assertEqual((self.models, self.model_reads, self.model_closes), (1, 1, 1))

    def test_model_stream_handles_bounded_and_quiesce_releases_abandoned_results(self):
        instance = self.rpc('bridge.hello')['result']['bridge_instance_id']
        self.allow_models = True
        self.expected_models = 8
        for _ in range(8):
            self.assertTrue(self.rpc('host.model.execute_stream', self.model_request(), instance)['ok'])
        self.assertEqual(self.rpc('host.model.execute_stream', self.model_request(), instance)['error']['code'], 'bridge_busy')
        self.assertTrue(self.rpc('host.auth.list', {}, instance)['ok'])
        self.assertTrue(self.call('plugin.quiesce', {})['ok'])
        self.assertEqual((self.models, self.model_reads, self.model_closes), (8, 0, 8))

    def test_model_stream_cleanup_keeps_native_ownership_until_callback_returns(self):
        instance = self.rpc('bridge.hello')['result']['bridge_instance_id']
        self.allow_models = True
        self.expected_models = 1
        self.assertTrue(self.rpc('host.model.execute_stream', self.model_request(), instance)['ok'])
        self.block_model_close = True
        self.release.clear()
        started = time.monotonic()
        paused = self.call('plugin.quiesce', {})
        self.assertFalse(paused['ok'])
        self.assertEqual(paused['error']['code'], 'bridge_still_draining')
        self.assertLess(time.monotonic()-started, 6)
        self.assertTrue(self.entered.is_set())
        self.assertTrue((self.runtime/'bridge.sock').exists())
        self.release.set()
        self.assertTrue(self.call('plugin.quiesce', {})['ok'])
        self.assertEqual((self.models, self.model_reads, self.model_closes), (1, 0, 1))

    def test_serial_model_budget_blocks_new_calls_until_old_stream_is_closed(self):
        instance = self.rpc('bridge.hello')['result']['bridge_instance_id']
        self.allow_models = True
        self.expected_models = 2
        request = dict(self.model_request(), model_serial=True)
        for invalid in (False, 1, 'true'):
            result = self.rpc('host.model.execute_stream', dict(request, model_serial=invalid), instance)
            self.assertEqual(result['error']['code'], 'invalid_payload')
        stream = self.rpc('host.model.execute_stream', request, instance)['result']['stream_id']
        refused = self.rpc('host.model.execute_stream', request, instance)
        self.assertEqual(refused['error']['code'], 'model_budget_busy')
        self.assertEqual(self.models, 1)
        self.model_close_error = True
        self.assertFalse(self.rpc('host.model.stream_close', {'stream_id': stream}, instance)['ok'])
        self.assertEqual(self.rpc('host.model.execute_stream', request, instance)['error']['code'], 'model_budget_busy')
        self.assertEqual(self.models, 1)
        self.model_close_error = False
        deadline = time.monotonic()+4
        while time.monotonic() < deadline:
            opened = self.rpc('host.model.execute_stream', request, instance)
            if opened.get('ok'):
                break
            self.assertEqual(opened['error']['code'], 'model_budget_busy')
            time.sleep(.05)
        self.assertTrue(opened['ok'], opened)
        self.assertEqual(self.models, 2)
        self.assertTrue(all('model_serial' not in request for request in self.model_requests))
        self.assertTrue(self.rpc('host.model.stream_close', {'stream_id': opened['result']['stream_id']}, instance)['ok'])

    def test_failed_model_close_keeps_ownership_and_throttles_cleanup_without_reexecute(self):
        instance = self.rpc('bridge.hello')['result']['bridge_instance_id']
        self.allow_models = True
        self.expected_models = 1
        stream = self.rpc('host.model.execute_stream', self.model_request(), instance)['result']['stream_id']
        self.model_close_error = True
        result = self.rpc('host.model.stream_close', {'stream_id': stream}, instance)
        self.assertEqual(result['error']['code'], 'host_callback_failed')
        self.assertNotIn('secret', json.dumps(result))
        started = time.monotonic()
        paused = self.call('plugin.quiesce', {})
        self.assertFalse(paused['ok'])
        self.assertEqual(paused['error']['code'], 'bridge_still_draining')
        self.assertLess(time.monotonic()-started, 6)
        self.assertIn(stream, self.model_streams)
        self.assertTrue((self.runtime/'bridge.sock').exists())
        self.assertLessEqual(self.model_closes, 7)
        self.model_close_error = False
        self.assertTrue(self.call('plugin.quiesce', {})['ok'])
        self.assertGreaterEqual(self.model_closes, 2)
        self.assertEqual((self.models, self.model_reads), (1, 0))

    def test_model_settings_save_and_fake_candidate_generation_survive_restart(self):
        self.start_daemon()
        self.wait_activity(0)
        status, settings = self.management('model-settings')
        self.assertEqual(status, 200)
        self.assertEqual(settings['revision'], 0)
        settings['config'].update(default_model='fixture-model', default_prompt='海'*2000, five_hour_model='five-hour-fixture',
                                  five_hour_prompt='五小时提示词', weekly_model='weekly-fixture',
                                  weekly_prompt='每周提示词', prompt_mode='random', phrases=['静候花开', '清风徐来'])
        request = {'revision': settings['revision'], 'config': settings['config']}
        status, saved = self.management('model-settings', 'POST', request)
        self.assertEqual(status, 200)
        self.assertEqual(saved['revision'], 1)
        status, _ = self.management('model-settings', 'POST', request)
        self.assertEqual(status, 409)
        self.assertEqual(self.models, 0)
        self.allow_models = True
        self.expected_models = 1
        request = {'account_id': 'a', 'model': 'fixture-model', 'count': 2}
        deadline = time.monotonic()+5
        while time.monotonic()<deadline:
            status, accepted = self.management('prompt-generations', 'POST', request, 'candidate-exactly-once')
            if status == 202:
                break
            self.assertEqual(status, 409)
            time.sleep(.025)
        self.assertEqual(status, 202, accepted)
        job_id = accepted['job_id']
        deadline = time.monotonic()+5
        while time.monotonic()<deadline:
            status, result = self.management('prompt-generations', query={'job_id': [job_id]})
            self.assertEqual(status, 200)
            if result['phase'] not in ('queued', 'running'):
                break
            time.sleep(.025)
        self.assertEqual(result['phase'], 'completed', result)
        self.assertEqual(result['candidates'], ['清风入怀', '星光相伴'])
        self.assertEqual((self.models, self.model_reads, self.model_closes), (1, 1, 1))
        self.assertEqual(self.model_requests[0]['auth_id'], 'a')
        self.assertEqual(self.model_requests[0]['forced_provider'], 'codex')
        self.assertEqual(self.model_requests[0]['exit_protocol'], 'codex')
        status, replay = self.management('prompt-generations', 'POST', request, 'candidate-exactly-once')
        self.assertEqual(status, 202)
        self.assertEqual(replay['job_id'], job_id)
        self.stop_daemon()
        self.start_daemon()
        status, restored = self.management('model-settings')
        self.assertEqual(status, 200)
        self.assertEqual(restored['config'], saved['config'])
        status, replay = self.management('prompt-generations', 'POST', request, 'candidate-exactly-once')
        self.assertEqual(status, 202)
        self.assertEqual(replay['job_id'], job_id)
        self.assertEqual(replay['phase'], 'completed')
        status, after = self.management('model-settings')
        self.assertEqual(after['config']['phrases'], ['静候花开', '清风徐来'])
        self.assertEqual((self.models, self.model_reads, self.model_closes), (1, 1, 1))

    def test_inflight_model_generation_crash_recovers_unknown_without_reexecute(self):
        self.inflight_model_generation_crash('a')

    def test_inflight_antigravity_generation_crash_recovers_unknown_without_reexecute(self):
        self.antigravity_accounts = True
        self.inflight_model_generation_crash('antig')

    def inflight_model_generation_crash(self, account_id):
        self.start_daemon()
        deadline = time.monotonic()+5
        while time.monotonic()<deadline:
            status, overview = self.management('overview')
            if status == 200 and overview.get('inventory', {}).get('status') == 'ready':
                break
            time.sleep(.025)
        self.assertEqual(overview['inventory']['status'], 'ready')
        self.allow_models = True
        self.expected_models = 1
        self.block_model_read = True
        self.release.clear()
        request = {'account_id': account_id, 'model': 'fixture-model', 'count': 2,
                   'prompt': '生成自然的中文问候，避免重复。'}
        status, accepted = self.management('prompt-generations', 'POST', request, 'interrupted-candidate')
        self.assertEqual(status, 202, accepted)
        self.assertTrue(self.entered.wait(5))
        job_id = accepted['job_id']
        status, inflight = self.management('prompt-generations', query={'job_id': [job_id]})
        self.assertEqual(status, 200)
        self.assertEqual(inflight['phase'], 'running')
        self.process.kill()
        self.process.communicate(timeout=5)
        self.process = None
        self.assertTrue(self.call('plugin.quiesce', {})['ok'])
        self.assertEqual((self.models, self.model_reads, self.model_closes), (1, 1, 1))
        self.assertTrue(self.call('plugin.register', {'schema_version': 6})['ok'])
        self.start_daemon()
        deadline = time.monotonic()+5
        while time.monotonic()<deadline:
            status, restored = self.management('prompt-generations', query={'job_id': [job_id]})
            if status == 200:
                break
            self.assertEqual(status, 503)
            time.sleep(.025)
        self.assertEqual(status, 200)
        self.assertEqual(restored['phase'], 'unknown')
        self.assertEqual(restored['candidates'], [])
        self.assertEqual(restored['error_code'], 'generation_interrupted')
        status, replay = self.management('prompt-generations', 'POST', request, 'interrupted-candidate')
        self.assertEqual(status, 202)
        self.assertEqual(replay['job_id'], job_id)
        self.assertEqual(replay['phase'], 'unknown')
        time.sleep(.3)
        self.assertEqual((self.models, self.model_reads, self.model_closes), (1, 1, 1))

    def wait_generation(self, job_id):
        deadline = time.monotonic()+5
        while time.monotonic()<deadline:
            status, result = self.management('prompt-generations', query={'job_id': [job_id]})
            self.assertIn(status, (200, 503), result)
            if status == 200 and result['phase'] not in ('queued', 'running'):
                return result
            time.sleep(.025)
        self.fail(f'generation did not finish: {result}')

    def test_antigravity_generation_directory_pin_idempotence_and_restart_without_codex_pollution(self):
        self.antigravity_accounts = True
        self.start_daemon()
        self.wait_activity(0)
        status, directory = self.management('generation-accounts')
        self.assertEqual(status, 200)
        self.assertEqual(directory['accounts'], [
            {'id': 'a', 'provider': 'codex', 'status': 'available'},
            {'id': 'b', 'provider': 'codex', 'status': 'available'},
            {'id': 'antig', 'provider': 'antigravity', 'status': 'available'},
        ])
        self.assertEqual(set(directory), {'instance_id', 'accounts'})
        for secret in ('fixture@example.invalid', 'fixture-project', 'private-fixture', 'access_token', 'refresh_token', 'auth_index'):
            self.assertNotIn(secret, json.dumps(directory))
        self.assertEqual(self.management('generation-accounts', 'POST', {})[0], 404)
        deadline = time.monotonic()+5
        while time.monotonic()<deadline:
            status, overview = self.management('overview')
            if status == 200 and overview.get('inventory', {}).get('status') == 'ready':
                break
            time.sleep(.025)
        self.assertEqual(overview['inventory']['status'], 'ready')
        self.assertEqual(overview['dashboard']['total'], 2)
        self.assertEqual(overview['capabilities']['prompt_generation_providers'], ['codex', 'antigravity'])
        self.assertTrue(overview['capabilities']['editable_generation_prompt'])
        status, accounts = self.management('accounts')
        self.assertEqual(status, 200)
        self.assertEqual([account['id'] for account in accounts['accounts']], ['a', 'b'])
        status, settings = self.management('model-settings')
        self.assertEqual(status, 200)
        self.allow_models = True
        self.expected_models = 1
        request = {'account_id': 'antig', 'model': 'fixture-antigravity-model', 'count': 2,
                   'prompt': '只生成简短自然的中文问候。\n不要提问。'}
        status, accepted = self.management('prompt-generations', 'POST', request, 'antigravity-exactly-once')
        self.assertEqual(status, 202, accepted)
        job_id = accepted['job_id']
        result = self.wait_generation(job_id)
        self.assertEqual(result['phase'], 'completed', result)
        self.assertEqual(result['candidates'], ['清风入怀', '星光相伴'])
        self.assertEqual((self.models, self.model_reads, self.model_closes), (1, 1, 1))
        execution = self.model_requests[0]
        self.assertEqual(execution['auth_id'], 'antig')
        self.assertEqual(execution['forced_provider'], 'antigravity')
        self.assertEqual(execution['entry_protocol'], 'openai-response')
        self.assertEqual(execution['exit_protocol'], 'openai-response')
        self.assertEqual(execution['model'], 'fixture-antigravity-model')
        self.assertTrue(execution['stream'])
        body = json.loads(base64.b64decode(execution['body']))
        self.assertEqual(body['model'], execution['model'])
        self.assertEqual(body['input'][0]['content'][0]['text'], request['prompt'])
        self.assertIn('exactly 2', body['instructions'])
        self.assertIn('JSON array', body['instructions'])
        status, replay = self.management('prompt-generations', 'POST', request, 'antigravity-exactly-once')
        self.assertEqual((status, replay['job_id']), (202, job_id))
        self.assertEqual(self.management('prompt-generations', 'POST', dict(request, model='another-model'),
                                         'antigravity-exactly-once')[0], 409)
        self.assertEqual(self.management('prompt-generations', 'POST', dict(request, prompt='另一种生成要求'),
                                         'antigravity-exactly-once')[0], 409)
        self.stop_daemon()
        self.start_daemon()
        result = self.wait_generation(job_id)
        self.assertEqual(result['phase'], 'completed')
        status, replay = self.management('prompt-generations', 'POST', request, 'antigravity-exactly-once')
        self.assertEqual((status, replay['job_id']), (202, job_id))
        self.assertEqual(replay['phase'], 'completed')
        status, after = self.management('model-settings')
        self.assertEqual(status, 200)
        self.assertEqual(after['config'], settings['config'])
        self.assertEqual((self.models, self.model_reads, self.model_closes), (1, 1, 1))

    def test_antigravity_model_error_stays_unknown_and_never_falls_back_or_retries(self):
        self.antigravity_accounts = True
        self.start_daemon()
        self.wait_activity(0)
        self.allow_models = True
        self.expected_models = 1
        self.model_error = True
        request = {'account_id': 'antig', 'model': 'fixture-antigravity-model', 'count': 2}
        status, accepted = self.management('prompt-generations', 'POST', request, 'antigravity-unknown')
        self.assertEqual(status, 202, accepted)
        job_id = accepted['job_id']
        result = self.wait_generation(job_id)
        self.assertEqual(result['phase'], 'unknown', result)
        self.assertEqual(result['error_code'], 'generation_outcome_unknown')
        self.assertEqual(result['candidates'], [])
        self.assertNotIn('secret', json.dumps(result))
        self.assertEqual((self.models, self.model_reads, self.model_closes), (1, 1, 1))
        self.assertEqual(self.model_requests[0]['forced_provider'], 'antigravity')
        self.model_error = False
        self.stop_daemon()
        self.start_daemon()
        self.assertEqual(self.wait_generation(job_id)['phase'], 'unknown')
        status, replay = self.management('prompt-generations', 'POST', request, 'antigravity-unknown')
        self.assertEqual((status, replay['job_id'], replay['phase']), (202, job_id, 'unknown'))
        time.sleep(.3)
        self.assertEqual((self.models, self.model_reads, self.model_closes), (1, 1, 1))

    def wait_wake_fixture_ready(self):
        deadline = time.monotonic()+5
        while time.monotonic()<deadline:
            status, overview = self.management('overview')
            if status == 200 and overview.get('inventory', {}).get('status') == 'ready':
                return overview
            time.sleep(.025)
        self.fail(f'wake fixture inventory did not become ready: {overview}')

    def test_packaged_wake_capability_metadata_preserves_protocol_and_generation(self):
        import tarfile
        if os.environ.get('CWR_V2_TEST_ARCHIVE'):
            package = Path(os.environ['CWR_V2_TEST_ARCHIVE'])
        else:
            output = self.root/'wake-package'
            subprocess.run(['python3', str(ROOT/'scripts/package-v2.py'), '--commit', '0'*40,
                            '--epoch', '0', '--output', str(output)], check=True, stdout=subprocess.PIPE)
            archives = list(output.glob('*.tar.gz'))
            self.assertEqual(len(archives), 1)
            package = archives[0]
        with tarfile.open(package, 'r:gz') as archive:
            members = [member for member in archive.getmembers() if member.name.endswith('/BUILD-INFO.json')]
            self.assertEqual(len(members), 1)
            info = json.load(archive.extractfile(members[0]))
        self.assertIs(info['model_execution'], True)
        self.assertIs(info['wake'], True)
        self.assertEqual(info['wake_providers'], ['codex'])
        self.assertIs(info['prompt_generation'], True)
        self.assertEqual(info['prompt_generation_providers'], ['codex', 'antigravity'])
        self.assertIs(info['editable_generation_prompt'], True)
        self.assertEqual((info['host_abi'], info['rpc_schema'], info['protocol_version']), (1, 6, 1))
        self.assertIs(info['real_account_validation'], False)
        self.assertEqual(self.models, 0)

    def configure_wake_fixture_model(self):
        status, settings = self.management('model-settings')
        self.assertEqual(status, 200)
        settings['config'].update(default_model='fixture-wake-model', default_prompt='你好，请简短回应。', prompt_mode='fixed')
        status, saved = self.management('model-settings', 'POST',
                                        {'revision': settings['revision'], 'config': settings['config']})
        self.assertEqual(status, 200, saved)
        return saved

    def test_wake_settings_defaults_validation_and_restart_never_send_models(self):
        self.start_daemon()
        self.wait_wake_fixture_ready()
        hello = self.rpc('bridge.hello')['result']
        self.assertTrue(hello['wake_supported'])
        routes = self.call('management.register', {})['result']['Routes']
        for endpoint in ('wake-settings', 'wake-jobs'):
            for method in ('GET', 'POST'):
                self.assertIn({'Method': method, 'Path': '/v0/management/codex-window-refresh/v2/'+endpoint}, routes)
        status, settings = self.management('wake-settings')
        self.assertEqual(status, 200)
        self.assertEqual(settings['revision'], 0)
        self.assertEqual(settings['config'], {
            'enabled': False, 'on_five_hour_reset': False, 'on_weekly_reset': False,
            'timezone': 'Asia/Shanghai', 'random_delay': {'min_seconds': 5, 'max_seconds': 60},
            'allowed_hours': ['08:00-23:00'], 'accounts_per_user': 1, 'max_accounts': 10000,
            'daily_enabled': False, 'daily_window': '09:00-09:10', 'daily_target': 1,
        })
        for request in ({'config': settings['config']}, {'revision': 0},
                        {'revision': 0, 'config': dict(settings['config'], extra=True)},
                        {'revision': 0, 'config': dict(settings['config'], daily_window='23:00-01:00')},
                        {'revision': 0, 'config': dict(settings['config'], accounts_per_user=101)}):
            status, rejected = self.management('wake-settings', 'POST', request)
            self.assertEqual((status, rejected.get('error')), (400, 'invalid_wake_settings'))
        config = dict(settings['config'], allowed_hours=[], accounts_per_user=2, max_accounts=10,
                      daily_target=3, daily_window='10:00-10:20')
        status, saved = self.management('wake-settings', 'POST', {'revision': 0, 'config': config})
        self.assertEqual(status, 200, saved)
        self.assertEqual(saved['revision'], 1)
        status, conflict = self.management('wake-settings', 'POST', {'revision': 0, 'config': config})
        self.assertEqual((status, conflict.get('error')), (409, 'wake_settings_conflict'))
        status, page = self.management('wake-jobs', query={'limit': ['1']})
        self.assertEqual((status, page['jobs']), (200, []))
        self.assertEqual(self.management('wake-jobs', query={'limit': ['101']})[0], 400)
        self.stop_daemon()
        self.start_daemon()
        self.wait_wake_fixture_ready()
        status, restored = self.management('wake-settings')
        self.assertEqual((status, restored['revision'], restored['config']), (200, 1, config))
        self.assertEqual((self.models, self.model_reads, self.model_closes), (0, 0, 0))

    def test_skipped_wakes_remain_informational_and_do_not_count_as_errors(self):
        import datetime
        import sqlite3
        self.start_daemon()
        self.wait_wake_fixture_ready()
        status, before = self.management('overview')
        self.assertEqual(status, 200)
        self.stop_daemon()
        stamp = datetime.datetime.now(datetime.timezone.utc).isoformat(timespec='microseconds').replace('+00:00', 'Z')
        with sqlite3.connect(self.root/'data'/'backend.sqlite') as database:
            account = database.execute('SELECT a.id,a.auth_index,b.binding FROM accounts a '
                                       'JOIN account_bindings b ON a.id=b.account_id WHERE a.id=?', ('a',)).fetchone()
            self.assertIsNotNone(account)
            for index, code in enumerate(['window_already_active', 'account_disabled', 'legacy-unrecognized-reason']):
                database.execute('INSERT INTO wake_jobs(id,account_id,auth_index,binding,reason,cycle,phase,'
                                 'scheduled_at,created_at,updated_at,error_code) '
                                 "VALUES(?,?,?,?,'five_hour_reset',?,'skipped',?,?,?,?)",
                                 ('skip-fixture-' + str(index), *account, 'cycle-' + str(index), stamp, stamp, stamp, code))
        self.start_daemon()
        self.wait_wake_fixture_ready()
        status, after = self.management('overview')
        self.assertEqual(status, 200)
        self.assertEqual(after['dashboard']['errors'], before['dashboard']['errors'])
        events = [event for event in after['dashboard']['recent_events'] if event.get('job_id', '').startswith('skip-fixture-')]
        self.assertEqual(len(events), 3)
        self.assertTrue(all(event['severity'] == 'info' and event['phase'] == 'skipped' for event in events))
        status, page = self.management('task-history', query={'search': ['skip-fixture-'], 'status': ['skipped']})
        self.assertEqual((status, page['total'], page['status_counts']['failed'], page['status_counts']['unknown']), (200, 3, 0, 0))
        self.assertEqual((self.models, self.model_reads, self.model_closes), (0, 0, 0))

    def test_task_history_proxies_persistent_automatic_records_and_readonly_filters(self):
        import datetime
        import sqlite3
        self.start_daemon()
        self.wait_wake_fixture_ready()
        self.stop_daemon()
        stamp = datetime.datetime.now(datetime.timezone.utc).isoformat(timespec='microseconds').replace('+00:00', 'Z')
        with sqlite3.connect(self.root/'data'/'backend.sqlite') as database:
            for job_id, trigger, automatic in [('history-fixture-auto', 'periodic', 1),
                                                ('history-fixture-manual', 'manual_single', 0)]:
                database.execute('INSERT INTO jobs(id,idempotency_key,request_hash,target,phase,expanded,created_at,updated_at) '
                                 "VALUES(?,?,?,'a','completed',1,?,?)", (job_id, job_id, 'fixture-hash', stamp, stamp))
                database.execute("INSERT INTO job_items(job_id,account_id,phase) VALUES(?,'a','completed')", (job_id,))
                database.execute('INSERT INTO query_plans VALUES(?,?,?,?,?)', (job_id, trigger, automatic, 0, 0))
            account = database.execute('SELECT a.id,a.auth_index,b.binding FROM accounts a '
                                       'JOIN account_bindings b ON a.id=b.account_id WHERE a.id=?', ('a',)).fetchone()
            self.assertIsNotNone(account)
            database.execute('INSERT INTO wake_jobs(id,account_id,auth_index,binding,reason,cycle,phase,'
                             'scheduled_at,created_at,updated_at,model,prompt) '
                             "VALUES(?,?,?,?,'five_hour_reset','history-cycle','unknown',?,?,?,?,?)",
                             ('history-fixture-wake', *account, stamp, stamp, stamp,
                              'fixture-history-model', 'history-secret-prompt-must-not-leak'))
        self.start_daemon()
        self.wait_wake_fixture_ready()
        status, page = self.management('task-history', query={'search': ['history-fixture'], 'limit': ['1'], 'page': ['1']})
        self.assertEqual(status, 200, page)
        self.assertEqual((page['total'], page['limit'], page['page'], len(page['records'])), (3, 1, 1, 1))
        self.assertEqual(page['status_counts']['unknown'], 1)
        self.assertIsInstance(page['view_revision'], int)
        self.assertTrue(page['instance_id'])
        first_id = page['records'][0]['job_id']
        status, second = self.management('task-history', query={'search': ['history-fixture'], 'limit': ['1'], 'page': ['2']})
        self.assertEqual((status, second['page'], second['total']), (200, 2, 3))
        self.assertNotEqual(first_id, second['records'][0]['job_id'])
        today = datetime.datetime.now(datetime.timezone.utc).replace(hour=0, minute=0, second=0, microsecond=0)
        query = {'search': ['history-fixture'], 'type': ['wake'], 'source': ['automatic'],
                 'trigger': ['five_hour_reset'], 'status': ['unknown'],
                 'from': ['2000-01-01T00:00:00Z'], 'to': ['2100-01-01T00:00:00Z'],
                 'today_from': [today.isoformat()], 'today_to': [(today + datetime.timedelta(days=1)).isoformat()]}
        status, automatic = self.management('task-history', query=query)
        self.assertEqual((status, automatic['total']), (200, 1), automatic)
        record = automatic['records'][0]
        self.assertEqual((record['job_id'], record['source'], record['phase']),
                         ('history-fixture-wake', 'automatic', 'unknown'))
        self.assertEqual(record.get('finished_at', ''), '')
        raw = json.dumps(automatic)
        self.assertNotIn('history-secret-prompt-must-not-leak', raw)
        self.assertNotIn('auth_index', raw)
        self.assertNotIn('binding', raw)
        status, completed = self.management('task-history', query={
            'search': ['history-fixture-auto'], 'type': ['query'], 'source': ['automatic'], 'status': ['completed']})
        self.assertEqual((status, completed['total']), (200, 1), completed)
        self.assertEqual(completed['records'][0]['finished_at'], stamp)
        self.assertEqual(completed['records'][0]['started_at'], '')
        self.assertEqual(self.management('task-history', 'POST', {})[0], 404)
        for invalid in ({'source': ['invalid']}, {'page': ['2', '3']}, {'limit': ['101']},
                        {'from': ['invalid']}, {'type': ['invalid']}, {'today_from': ['invalid']}):
            self.assertEqual(self.management('task-history', query=invalid)[0], 400, invalid)
        self.assertEqual((self.models, self.model_reads, self.model_closes), (0, 0, 0))

    def test_manual_wake_readonly_preflight_failure_is_durable_and_never_sends_models(self):
        self.antigravity_accounts = True
        self.start_daemon()
        self.wait_wake_fixture_ready()
        self.configure_wake_fixture_model()
        self.query_delay(0, 0)
        for request in ({}, {'account_id': 'a', 'model': 'arbitrary'}, {'account_id': 'a', 'force': True}):
            self.assertEqual(self.management('wake-jobs', 'POST', request, 'wake-invalid')[0], 400)
        self.assertEqual(self.management('wake-jobs', 'POST', {'account_id': 'a'})[0], 400)
        status, rejected = self.management('wake-jobs', 'POST', {'account_id': 'antig'}, 'antig-cannot-wake')
        self.assertEqual((status, rejected.get('error')), (409, 'wake_account_not_found'))
        request = {'account_id': 'a'}
        status, accepted = self.management('wake-jobs', 'POST', request, 'wake-safe-preflight')
        self.assertEqual(status, 202, accepted)
        job_id = accepted['job_id']
        deadline = time.monotonic()+5
        while time.monotonic()<deadline:
            status, result = self.management('wake-jobs', query={'job_id': [job_id]})
            self.assertEqual(status, 200)
            status, queries = self.management('jobs')
            self.assertEqual(status, 200)
            preflight = [job for job in queries['jobs'] if job.get('trigger') == 'wake_preflight' and job['phase'] == 'failed']
            if result['phase'] == 'checking' and preflight:
                break
            time.sleep(.025)
        self.assertEqual(result['phase'], 'checking', result)
        self.assertTrue(preflight, queries)
        self.assertEqual(result.get('hold_reason'), 'quota_unknown')
        self.assertEqual((self.models, self.model_reads, self.model_closes), (0, 0, 0))
        for sensitive in ('binding', 'auth_index', 'fixture-wake-model', '你好，请简短回应。', 'must-not-forward'):
            self.assertNotIn(sensitive, json.dumps(result, ensure_ascii=False))
        status, replay = self.management('wake-jobs', 'POST', request, 'wake-safe-preflight')
        self.assertEqual((status, replay['job_id']), (202, job_id))
        self.assertEqual(self.management('wake-jobs', 'POST', {'account_id': 'b'}, 'wake-safe-preflight')[0], 409)
        self.stop_daemon()
        self.start_daemon()
        self.wait_wake_fixture_ready()
        status, restored = self.management('wake-jobs', 'POST', request, 'wake-safe-preflight')
        self.assertEqual((status, restored['job_id'], restored['phase']), (202, job_id, 'checking'))
        self.assertEqual((self.models, self.model_reads, self.model_closes), (0, 0, 0))

    def test_interrupted_wake_recovers_unknown_and_does_not_resend_or_release_account(self):
        import datetime
        import sqlite3
        self.start_daemon()
        self.wait_wake_fixture_ready()
        self.configure_wake_fixture_model()
        self.stop_daemon()
        stamp = datetime.datetime.now(datetime.timezone.utc).isoformat(timespec='microseconds').replace('+00:00', 'Z')
        request = {'account_id': 'a'}
        digest = hashlib.sha256(json.dumps(request, separators=(',', ':')).encode()).hexdigest()
        with sqlite3.connect(self.root/'data'/'backend.sqlite') as database:
            account = database.execute('SELECT a.id,a.auth_index,b.binding FROM accounts a '
                                       'JOIN account_bindings b ON a.id=b.account_id WHERE a.id=?', ('a',)).fetchone()
            self.assertIsNotNone(account)
            database.execute('INSERT INTO wake_jobs(id,account_id,auth_index,binding,reason,cycle,phase,'
                             'idempotency_key,request_hash,scheduled_at,created_at,updated_at,model,prompt) '
                             "VALUES(?,?,?,?,'manual',?,'sending',?,?,?,?,?,?,?)",
                             ('interrupted-wake-fixture', *account, 'wake-interrupted', 'wake-interrupted', digest,
                              stamp, stamp, stamp, 'fixture-wake-model', '你好，请简短回应。'))
        self.start_daemon()
        self.wait_wake_fixture_ready()
        status, restored = self.management('wake-jobs', query={'job_id': ['interrupted-wake-fixture']})
        self.assertEqual((status, restored['phase']), (200, 'unknown'), restored)
        status, replay = self.management('wake-jobs', 'POST', request, 'wake-interrupted')
        self.assertEqual((status, replay['job_id'], replay['phase']), (202, 'interrupted-wake-fixture', 'unknown'))
        self.assertEqual(self.management('wake-jobs', 'POST', request, 'wake-new-key-must-not-resend')[0], 429)
        time.sleep(.5)
        status, after = self.management('wake-jobs', query={'job_id': ['interrupted-wake-fixture']})
        self.assertEqual((status, after['phase']), (200, 'unknown'), after)
        self.assertEqual((self.models, self.model_reads, self.model_closes), (0, 0, 0))

    def emit_wake_fixture_quota(self, request_id, reset_seconds):
        deadline = time.monotonic()+5
        while time.monotonic()<deadline:
            observer = self.rpc('bridge.hello')['result']['quota_observer']
            if observer['ready'] and observer['accounts'] == 2:
                break
            time.sleep(.025)
        self.assertTrue(observer['ready'])
        headers = self.quota_headers('1')
        headers.update({'X-Codex-Primary-Reset-After-Seconds': [str(reset_seconds)],
                        'X-Codex-Secondary-Used-Percent': ['0'],
                        'X-Codex-Secondary-Window-Minutes': ['10080'],
                        'X-Codex-Secondary-Reset-After-Seconds': ['604800']})
        self.call('request.intercept_after', {'RequestID': request_id, 'Metadata': {'selected_auth_id': 'a'}})
        self.call('response.intercept_after', {'RequestID': request_id, 'ResponseHeaders': headers})
        self.call('request.complete', {'RequestID': request_id})
        expected_state = 'expired' if reset_seconds == 0 else 'active'
        deadline = time.monotonic()+5
        while time.monotonic()<deadline:
            status, page = self.management('accounts')
            self.assertEqual(status, 200)
            account = next(account for account in page['accounts'] if account['id'] == 'a')
            snapshot = account.get('quota', {})
            if (snapshot.get('source') == 'cpa_response_headers' and
                    snapshot.get('five_hour', {}).get('State') == expected_state and
                    snapshot.get('weekly', {}).get('Remaining', {}).get('Value') == 100):
                return snapshot
            time.sleep(.025)
        self.fail(f'passive wake quota did not converge: {account}')

    def test_manual_wake_saved_prompt_exact_send_and_readonly_window_verification(self):
        self.start_daemon()
        self.wait_wake_fixture_ready()
        saved = self.configure_wake_fixture_model()
        self.emit_wake_fixture_quota('wake-expired-evidence', 0)
        self.assertEqual(self.models, 0)
        self.allow_models = True
        self.expected_models = 1
        status, accepted = self.management('wake-jobs', 'POST', {'account_id': 'a'}, 'wake-exact-saved-prompt')
        self.assertEqual(status, 202, accepted)
        job_id = accepted['job_id']
        deadline = time.monotonic()+5
        while time.monotonic()<deadline:
            status, result = self.management('wake-jobs', query={'job_id': [job_id]})
            self.assertEqual(status, 200)
            if result['phase'] in ('verifying', 'completed', 'failed', 'unknown', 'skipped'):
                break
            time.sleep(.025)
        self.assertEqual(result['phase'], 'verifying', result)
        self.assertEqual((self.models, self.model_reads, self.model_closes), (1, 1, 1))
        request = self.model_requests[0]
        self.assertEqual((request['forced_provider'], request['auth_id'], request['entry_protocol'], request['exit_protocol']),
                         ('codex', 'a', 'openai-response', 'codex'))
        self.assertEqual(request['model'], saved['config']['default_model'])
        body = json.loads(base64.b64decode(request['body']))
        self.assertEqual(body, {'model': saved['config']['default_model'], 'stream': True, 'store': False,
                               'instructions': "You are a concise coding assistant. Answer the user's request directly. No tools, file access, or command execution are available; do not claim otherwise.",
                               'tool_choice': 'none',
                               'input': [{'role': 'user', 'content': [{'type': 'input_text',
                                          'text': saved['config']['default_prompt']}]}]})
        self.assertNotIn('tools', body)
        status, replay = self.management('wake-jobs', 'POST', {'account_id': 'a'}, 'wake-exact-saved-prompt')
        self.assertEqual((status, replay['job_id']), (202, job_id))
        self.emit_wake_fixture_quota('wake-active-evidence', 3000)
        deadline = time.monotonic()+5
        while time.monotonic()<deadline:
            status, verified = self.management('wake-jobs', query={'job_id': [job_id]})
            self.assertEqual(status, 200)
            if verified['phase'] == 'completed':
                break
            time.sleep(.025)
        self.assertEqual(verified['phase'], 'completed', verified)
        self.assertEqual(verified.get('error_code'), 'window_verified')
        self.assertEqual((self.models, self.model_reads, self.model_closes), (1, 1, 1))

    def test_wake_model_error_unknown_blocks_candidate_and_new_wake_across_restart(self):
        self.start_daemon()
        self.wait_wake_fixture_ready()
        self.configure_wake_fixture_model()
        self.emit_wake_fixture_quota('wake-error-expired-evidence', 0)
        self.allow_models = True
        self.expected_models = 1
        self.model_error = True
        request = {'account_id': 'a'}
        status, accepted = self.management('wake-jobs', 'POST', request, 'wake-model-error')
        self.assertEqual(status, 202, accepted)
        job_id = accepted['job_id']
        deadline = time.monotonic()+5
        while time.monotonic()<deadline:
            status, result = self.management('wake-jobs', query={'job_id': [job_id]})
            self.assertEqual(status, 200)
            if result['phase'] == 'unknown':
                break
            time.sleep(.025)
        self.assertEqual(result['phase'], 'unknown', result)
        self.assertEqual(result.get('error_code'), 'wake_outcome_unknown')
        self.assertNotIn('secret', json.dumps(result))
        self.assertEqual((self.models, self.model_reads, self.model_closes), (1, 1, 1))
        self.assertEqual(self.management('wake-jobs', 'POST', request, 'wake-model-error-new-key')[0], 429)
        status, rejected = self.management('prompt-generations', 'POST',
                                          {'account_id': 'a', 'model': 'fixture-model', 'count': 2},
                                          'candidate-must-not-bypass-unknown-wake')
        self.assertEqual((status, rejected.get('error')), (409, 'generation_account_unavailable'))
        self.model_error = False
        self.stop_daemon()
        self.start_daemon()
        self.wait_wake_fixture_ready()
        status, replay = self.management('wake-jobs', 'POST', request, 'wake-model-error')
        self.assertEqual((status, replay['job_id'], replay['phase']), (202, job_id, 'unknown'))
        self.assertEqual(self.management('wake-jobs', 'POST', request, 'wake-model-error-new-key-after-restart')[0], 429)
        time.sleep(.5)
        self.assertEqual((self.models, self.model_reads, self.model_closes), (1, 1, 1))

    def test_event_control_survives_all_host_callback_slots_being_blocked(self):
        hello = self.rpc('bridge.hello')['result']
        instance = hello['bridge_instance_id']
        self.release.clear()
        results = []
        threads = [threading.Thread(target=lambda: results.append(self.rpc('host.auth.get_runtime', {'auth_index': 'slow'}, instance))) for _ in range(hello['max_host_calls'])]
        for thread in threads: thread.start()
        try:
            deadline = time.monotonic()+3
            while time.monotonic()<deadline:
                hello = self.rpc('bridge.hello')['result']
                if hello['active_host_calls'] == hello['max_host_calls']: break
                time.sleep(.01)
            self.assertEqual(hello['active_host_calls'], hello['max_host_calls'])
            self.assertEqual(self.rpc('host.auth.list', {}, instance)['error']['code'], 'bridge_busy')
            self.call('request.intercept_before', {'RequestID': 'live-control', 'Metadata': {'caller_scope': 'u'}})
            batch = self.rpc('bridge.events.pull', {'after': 0}, instance)['result']
            self.assertEqual(batch['events'][0]['request_id'], 'live-control')
            self.assertTrue(self.rpc('bridge.events.ack', {'through': batch['latest_sequence']}, instance)['ok'])
        finally:
            self.release.set()
            for thread in threads: thread.join(5)
        self.assertTrue(all(not thread.is_alive() for thread in threads))
        self.assertTrue(all(reply['ok'] for reply in results))

    def test_update_script_installs_real_package_restarts_daemon_and_rolls_back(self):
        # Real packaged executables, actual native callbacks, Unix IPC and SQLite;
        # systemd actions are driven locally so this never touches host services.
        spec = importlib.util.spec_from_file_location('v2_install_smoke', ROOT / 'scripts/install-v2.py')
        installer = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(installer)
        version = (ROOT / 'release/v2-version.txt').read_text().strip()
        bridge_version = 'v' + (ROOT/'native/bridge/version.h').read_text().split('"')[1]
        packages = self.root / 'packages'
        if os.environ.get('CWR_V2_TEST_ARCHIVE'):
            archive = Path(os.environ['CWR_V2_TEST_ARCHIVE'])
            sums = Path(os.environ['CWR_V2_TEST_SUMS'])
        else:
            subprocess.run(['python3', str(ROOT / 'scripts/package-v2.py'), '--commit', '0'*40,
                            '--epoch', '0', '--output', str(packages)], check=True, stdout=subprocess.PIPE)
            archive = packages / f'codex-window-refresh-v2_{version}_linux_{"arm64" if os.uname().machine == "aarch64" else "amd64"}.tar.gz'
            sums = packages / 'SHA256SUMS'
            sums.write_bytes(archive.with_name(archive.name+'.sha256').read_bytes())
        cpa = self.root / 'cpa'
        cpa.mkdir()
        config = cpa / 'config.yaml'
        original = 'port: 8317\napi-keys: [fixture-preserved]\nplugins:\n  enabled: false\n  configs:\n    other: {enabled: true}\n'
        config.write_text(original)
        v1state = cpa / 'window-refresh-state'
        v1state.mkdir()
        (v1state / 'state.json').write_text('{"legacy":"discard-this-data"}')
        (cpa/'window-refresh.yaml').write_text('enabled: false\n')
        (cpa/'window-refresh.env').write_text('CPA_MANAGEMENT_KEY=legacy-only\n')
        (cpa/'plugins').mkdir()
        (cpa/'plugins/codex-window-refresh-v0.1.0-rc.28.so').write_bytes(b'old plugin')
        (cpa/'plugins/other-plugin.so').write_bytes(b'preserve other plugin')
        (cpa/'auth').mkdir()
        (cpa/'auth/account.json').write_text('{"preserve":"CPA account"}')
        user = pwd.getpwuid(os.getuid()).pw_name
        plan = dict(directory=cpa, binary=ROOT/'bin/window-refreshd', config=config,
                    plugins=installer.common.load_yaml(original)[0]['plugins'], wrapper={}, text=original,
                    plugin_dir=cpa/'plugins', uid=os.getuid(), gid=os.getgid(), user=user,
                    data={'auth-dir': str(cpa/'auth')}, service=dict(name='fixture-cpa.service', scope='system'))
        actual_paths = installer.paths(plan)
        actual_paths['unit'] = self.root/'units/backend.service'
        actual_paths['dropin'] = self.root/'units/cpa.d/v2.conf'
        old_dropin = actual_paths['dropin'].with_name('60-window-refresh.conf')
        old_dropin.parent.mkdir(parents=True)
        old_dropin.write_text('# Managed by codex-window-refresh installer\n[Service]\nEnvironmentFile="' + str(cpa/'window-refresh.env') + '"\n')
        actions = []
        real_run = subprocess.run
        real_directory = installer.common.directory
        def run_probe(command, *args, **kwargs):
            if command[0] == 'runuser': command = command[4:]
            if command[0] == 'systemctl':
                return subprocess.CompletedProcess(command, 0, stdout='', stderr='')
            return real_run(command, *args, **kwargs)
        def service(service, action, *names):
            actions.append((service['name'], action))
            if action == 'stop':
                if service['name'] == 'fixture-cpa.service':
                    self.assertTrue(self.call('plugin.quiesce', {})['ok'])
                elif self.process:
                    self.stop_daemon()
            if action == 'start':
                if service['name'] == 'fixture-cpa.service':
                    self.runtime = actual_paths['runtime']
                    os.environ['CWR_V2_RUNTIME_DIR'] = str(self.runtime)
                    self.assertTrue(self.call('plugin.reconfigure', {'schema_version': 6})['ok'])
                else:
                    line = next(line for line in actual_paths['unit'].read_text().splitlines() if line.startswith('ExecStart='))
                    self.process = subprocess.Popen(shlex.split(line.removeprefix('ExecStart=')), stderr=subprocess.PIPE)
            return ''
        args = type('Args', (), dict(version=version, package=str(archive), checksums=str(sums), restart=True))()
        arch = 'arm64' if os.uname().machine == 'aarch64' else 'amd64'
        with contextlib.ExitStack() as stack:
            plan['cpa_url'] = stack.enter_context(self.cpa_resources())
            stack.enter_context(mock.patch.object(installer, 'paths', return_value=actual_paths))
            stack.enter_context(mock.patch.object(installer.common, 'service_run', side_effect=service))
            stack.enter_context(mock.patch.object(installer.common, 'detect_processes', return_value=[dict(uid=os.getuid(), pid=os.getpid())]))
            # Simulate root file ownership in this unprivileged temporary tree.
            stack.enter_context(mock.patch.object(installer.common.os, 'chown'))
            stack.enter_context(mock.patch.object(installer.common.os, 'fchown'))
            stack.enter_context(mock.patch.object(installer.common, 'directory', side_effect=lambda path, mode, uid, gid, strict=False: real_directory(path, mode, os.getuid() if uid == 0 else uid, os.getgid() if gid == 0 else gid, strict)))
            stack.enter_context(mock.patch.object(installer.common, 'private_file', side_effect=lambda path: path))
            stack.enter_context(mock.patch.object(installer.subprocess, 'run', side_effect=run_probe))
            real_initial_ready = installer.wait_ready
            def reject_first_activation(*arguments, **options):
                real_initial_ready(*arguments, **options)
                raise installer.InstallError('injected first activation failure')
            with mock.patch.object(installer, 'wait_ready', side_effect=reject_first_activation):
                with self.assertRaises(installer.InstallError): installer.install(args, plan, arch)
            self.assertTrue((v1state/'state.json').exists())
            self.assertTrue((cpa/'window-refresh.yaml').exists())
            self.assertTrue(old_dropin.exists())
            self.assertTrue((cpa/'plugins/codex-window-refresh-v0.1.0-rc.28.so').exists())
            installer.install(args, plan, arch)
            self.assertEqual(installer.probe(self.runtime, version, bridge_version=bridge_version, resource_url=installer.cpa_resource_url(plan))['version'], version)
            self.assertFalse(v1state.exists())
            self.assertFalse((cpa/'window-refresh.yaml').exists())
            self.assertFalse((cpa/'window-refresh.env').exists())
            self.assertFalse(old_dropin.exists())
            self.assertFalse((cpa/'plugins/codex-window-refresh-v0.1.0-rc.28.so').exists())
            self.assertFalse((actual_paths['root']/'backups').exists())
            self.query_delay(0, 0)
            status, accepted = self.management('jobs', 'POST', {'kind': 'quota_query'}, 'update-preserves-job')
            self.assertEqual(status, 202)
            job_id = accepted['job_id']
            deadline = time.monotonic()+5
            while time.monotonic()<deadline:
                _, job = self.management('jobs', query={'job_id': [job_id]})
                if job['phase'] == 'failed': break
                time.sleep(.025)
            self.assertEqual(job['phase'], 'failed')
            data, _ = installer.common.load_yaml(config.read_text())
            plan.update(text=config.read_text(), plugins=data['plugins'], wrapper=data['plugins']['configs'][installer.common.PLUGIN])
            args.restart = False
            actions.clear()
            old_instance = installer.probe(self.runtime, version, bridge_version=bridge_version, resource_url=installer.cpa_resource_url(plan))['instance_id']
            stale_release = actual_paths['root']/'releases/v0.2.0-rc.1'
            stale_release.mkdir()
            (stale_release/'old-binary').write_bytes(b'obsolete backend')
            installer.install(args, plan, arch)
            ready = installer.probe(self.runtime, version, bridge_version=bridge_version, resource_url=installer.cpa_resource_url(plan))
            self.assertNotEqual(old_instance, ready['instance_id'])
            self.assertFalse(stale_release.exists())
            self.assertFalse(any(name == 'fixture-cpa.service' for name, _ in actions), actions)
            _, replay = self.management('jobs', 'POST', {'kind': 'quota_query'}, 'update-preserves-job')
            self.assertEqual(replay['job_id'], job_id)
            # A cleanup error happens after activation: retain the new process
            # and pending targets, then finish deletion on the next deployment.
            stale_release.mkdir()
            (stale_release/'old-binary').write_bytes(b'obsolete backend')
            with mock.patch.object(installer, 'cleanup_deployment', side_effect=OSError('injected cleanup failure')):
                with self.assertRaisesRegex(installer.InstallError, '新版本已运行'):
                    installer.install(args, plan, arch)
            installer.probe(self.runtime, version, bridge_version=bridge_version, resource_url=installer.cpa_resource_url(plan))
            self.assertTrue(json.loads(actual_paths['record'].read_text())['cleanup_pending'])
            installer.install(args, plan, arch)
            self.assertFalse(stale_release.exists())
            self.assertTrue(json.loads(actual_paths['record'].read_text())['cleanup_complete'])
            # Exercise an activation failure after files and SQLite changed.
            real_wait_ready = installer.wait_ready
            def failed_health(*arguments, **options):
                real_wait_ready(*arguments, **options)
                import sqlite3
                with sqlite3.connect(actual_paths['data']/'backend.sqlite') as database:
                    database.execute('PRAGMA user_version=999')
                raise installer.InstallError('injected health failure')
            with mock.patch.object(installer, 'wait_ready', side_effect=failed_health):
                with self.assertRaises(installer.InstallError): installer.install(args, plan, arch)
            deadline = time.monotonic()+5
            while time.monotonic()<deadline:
                try:
                    installer.probe(self.runtime, version, bridge_version=bridge_version, resource_url=installer.cpa_resource_url(plan))
                    break
                except Exception:
                    time.sleep(.025)
            _, replay = self.management('jobs', 'POST', {'kind': 'quota_query'}, 'update-preserves-job')
            self.assertEqual(replay['job_id'], job_id)
            import sqlite3
            with sqlite3.connect(actual_paths['data']/'backend.sqlite') as database:
                self.assertEqual(database.execute('PRAGMA user_version').fetchone()[0], 0)
            self.assertFalse(v1state.exists())
            self.assertEqual((cpa/'auth/account.json').read_text(), '{"preserve":"CPA account"}')
            self.assertEqual((cpa/'plugins/other-plugin.so').read_bytes(), b'preserve other plugin')
            self.assertIn('fixture-preserved', config.read_text())
            self.assertTrue(data['plugins']['configs']['other']['enabled'])

    def test_native_frontend_durable_query_and_daemon_restart(self):
        status, _ = self.management('overview')
        self.assertEqual(status, 503)
        self.start_daemon()
        page = self.call('management.handle', {'Method': 'GET', 'Path': '/v0/resource/plugins/codex-window-refresh/status'})['result']
        self.assertEqual(page['StatusCode'], 200)
        self.assertIn('账号额度', base64.b64decode(page['Body']).decode())
        self.query_delay(0, 0)
        status, accepted = self.management('jobs', 'POST', {'kind': 'quota_query'}, 'persistent-key')
        self.assertEqual(status, 202)
        job_id = accepted['job_id']
        deadline = time.monotonic()+5
        while time.monotonic()<deadline:
            status, job = self.management('jobs', query={'job_id': [job_id]})
            if job['phase'] in ('completed', 'failed'):
                break
            time.sleep(.05)
        self.assertEqual(job['phase'], 'failed')
        self.assertEqual(len(job['items']), 2)
        self.assertTrue(all(i['error_code']=='quota_credential_missing' for i in job['items']))
        self.process.terminate()
        _, stderr = self.process.communicate(timeout=10)
        self.assertEqual(self.process.returncode, 0, stderr)
        self.process = None
        self.start_daemon()
        status, replay = self.management('jobs', 'POST', {'kind': 'quota_query'}, 'persistent-key')
        self.assertEqual(status, 202)
        self.assertEqual(replay['job_id'], job_id)
        self.assertTrue(replay['existing'])
        status, restored = self.management('jobs', query={'job_id': [job_id]})
        self.assertEqual(restored['phase'], 'failed')
        self.assertTrue(self.call('plugin.quiesce', {})['ok'])
        self.assertFalse((self.runtime/'bridge.sock').exists())
        self.assertTrue(self.call('plugin.reconfigure', {'schema_version': 6})['ok'])
        status, _ = self.management('overview')
        self.assertEqual(status, 200)

    def query_delay(self, minimum, maximum):
        status, settings = self.management('settings')
        self.assertEqual(status, 200)
        settings['config']['random_delay'] = {'min_seconds': minimum, 'max_seconds': maximum}
        status, saved = self.management('settings', 'POST', {'revision': settings['revision'], 'config': settings['config']})
        self.assertEqual(status, 200)
        return saved

    def test_query_settings_and_persisted_delay_never_dispatch_models(self):
        self.start_daemon()
        status, settings = self.management('settings')
        self.assertEqual(status, 200)
        self.assertFalse(settings['config']['automatic'])
        self.assertFalse(settings['config']['on_five_hour_expiry'])
        self.assertFalse(settings['config']['on_weekly_expiry'])
        self.assertEqual(settings['config']['random_delay'], {'min_seconds': 5, 'max_seconds': 60})
        saved = self.query_delay(60, 60)
        status, accepted = self.management('jobs', 'POST', {'kind': 'quota_query'}, 'delayed-batch')
        self.assertEqual(status, 202)
        deadline = time.monotonic()+5
        while time.monotonic()<deadline:
            status, job = self.management('jobs', query={'job_id': [accepted['job_id']]})
            if job.get('items'):
                break
            time.sleep(.02)
        self.assertEqual(len(job['items']), 2)
        self.assertTrue(all(item['phase']=='queued' and item['scheduled_at'] for item in job['items']))
        schedule = {item['account_id']: item['scheduled_at'] for item in job['items']}
        self.assertEqual(self.models, 0)
        status, _ = self.management('settings', 'POST', {'revision': 0, 'config': saved['config']})
        self.assertEqual(status, 409)
        self.process.terminate()
        self.process.communicate(timeout=10)
        self.process = None
        self.start_daemon()
        status, restored = self.management('jobs', query={'job_id': [accepted['job_id']]})
        self.assertEqual(status, 200)
        self.assertEqual(schedule, {item['account_id']: item['scheduled_at'] for item in restored['items']})
        status, immediate = self.management('jobs', 'POST', {'kind': 'quota_query', 'account_id': 'a'}, 'promote-single')
        self.assertEqual(status, 202)
        deadline = time.monotonic()+5
        while time.monotonic()<deadline:
            _, single = self.management('jobs', query={'job_id': [immediate['job_id']]})
            if single['phase']=='failed':
                break
            time.sleep(.02)
        self.assertEqual(single['phase'], 'failed')
        self.assertEqual(single['items'][0]['error_code'], 'quota_credential_missing')
        _, batch = self.management('jobs', query={'job_id': [accepted['job_id']]})
        self.assertEqual(next(item for item in batch['items'] if item['account_id']=='a')['phase'], 'failed')
        self.assertEqual(next(item for item in batch['items'] if item['account_id']=='b')['phase'], 'queued')
        self.assertEqual(self.models, 0)

    def test_dashboard_sync_and_filters_do_not_create_quota_or_model_jobs(self):
        self.start_daemon()
        deadline = time.monotonic()+5
        while time.monotonic()<deadline:
            status, overview = self.management('overview')
            if status == 200 and overview['inventory']['status'] == 'ready':
                break
            time.sleep(.02)
        self.assertEqual(status, 200)
        self.assertEqual(overview['inventory']['status'], 'ready')
        summary = overview['dashboard']
        self.assertEqual(summary['total'], 2)
        self.assertEqual(summary['available'], 2)
        self.assertEqual(summary['disabled'], 0)
        self.assertEqual(summary['capacity'], 0)
        self.assertEqual(summary['unknown_quota'], 2)
        self.assertEqual(summary['tasks'], {'active': 0, 'querying': 0})
        self.assertEqual(summary['recent_jobs'], [])
        self.assertEqual(summary['recent_events'][0]['kind'], 'inventory')
        previous_sync = overview['inventory']['last_success_at']
        status, accepted = self.management('sync', 'POST')
        self.assertEqual(status, 202)
        self.assertEqual(accepted['status'], 'queued')
        deadline = time.monotonic()+5
        while time.monotonic()<deadline:
            _, overview = self.management('overview')
            if overview['inventory']['last_success_at'] != previous_sync:
                break
            time.sleep(.02)
        self.assertNotEqual(overview['inventory']['last_success_at'], previous_sync)
        status, page = self.management('accounts', query={'search': ['A'], 'status': ['available'], 'limit': ['6']})
        self.assertEqual(status, 200)
        self.assertEqual(page['total'], 2)
        self.assertEqual(page['filter_total'], 1)
        self.assertEqual([account['id'] for account in page['accounts']], ['a'])
        status, page = self.management('accounts', query={'status': ['disabled'], 'limit': ['6']})
        self.assertEqual(status, 200)
        self.assertEqual(page['filter_total'], 0)
        self.assertEqual(page['accounts'], [])
        status, jobs = self.management('jobs')
        self.assertEqual(status, 200)
        self.assertEqual(jobs['jobs'], [])
        self.assertEqual(self.models, 0)
        self.assertEqual(self.management('sync')[0], 404)

    def test_proxy_preserves_body_query_and_status_but_strips_credentials(self):
        captured = {}
        class Handler(http.server.BaseHTTPRequestHandler):
            def do_POST(inner):
                captured['path'] = inner.path
                captured['headers'] = inner.headers
                captured['body'] = inner.rfile.read(int(inner.headers['Content-Length']))
                inner.send_response(409)
                inner.send_header('Content-Type', 'application/octet-stream')
                inner.send_header('Content-Length', '2')
                inner.end_headers()
                inner.wfile.write(b'\x00\xff')
            def log_message(self, *args):
                pass
        with socketserver.UnixStreamServer(str(self.runtime/'daemon.sock'), Handler) as server:
            thread = threading.Thread(target=server.serve_forever)
            thread.start()
            try:
                raw = b'{"fixture":"\\u4e2d\\n\\u0000"}'
                response = self.call('management.handle', {
                    'Method': 'POST', 'Path': '/v0/management/codex-window-refresh/v2/jobs',
                    'Headers': {'Authorization': ['Bearer never-forward'], 'Idempotency-Key': ['stable-key']},
                    'Query': {'job_id': ['space & question?']}, 'Body': base64.b64encode(raw).decode(),
                })['result']
                self.assertEqual(response['StatusCode'], 409)
                self.assertEqual(base64.b64decode(response['Body']), b'\x00\xff')
                self.assertEqual(response['Headers']['Content-Type'], ['application/octet-stream'])
                self.assertEqual(captured['body'], raw)
                self.assertEqual(captured['path'], '/api/v2/jobs?job_id=space%20%26%20question%3F')
                self.assertNotIn('Authorization', captured['headers'])
                self.assertEqual(captured['headers']['Idempotency-Key'], 'stable-key')
            finally:
                server.shutdown()
                thread.join()

    def test_quiesce_retains_ownership_until_host_callback_returns(self):
        instance = self.rpc('bridge.hello')['result']['bridge_instance_id']
        self.release.clear()
        result = []
        worker = threading.Thread(target=lambda: result.append(self.rpc('host.auth.get_runtime', {'auth_index': 'slow'}, instance)))
        worker.start()
        self.assertTrue(self.entered.wait(3))
        paused = self.call('plugin.quiesce', {})
        self.assertFalse(paused['ok'])
        self.assertEqual(paused['error']['code'], 'bridge_still_draining')
        self.assertTrue((self.runtime/'bridge.sock').exists())
        self.release.set()
        worker.join(5)
        self.assertFalse(worker.is_alive())
        self.assertTrue(self.call('plugin.quiesce', {})['ok'])
        self.assertFalse((self.runtime/'bridge.sock').exists())

if __name__ == '__main__':
    unittest.main(verbosity=2)
