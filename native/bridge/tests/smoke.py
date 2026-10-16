"""Actual dlopen -> private native RPC -> Go daemon -> SQLite smoke tests.

The fake host exposes local fixture accounts with missing credentials, so this
suite cannot issue upstream HTTP or model requests. Successful HTTP normalization
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
        self.assertTrue(registration['ok'], registration)
        self.assert_host_registration(registration)

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

    def assert_host_registration(self, registration):
        self.assertTrue(self.host_accepts_registration(registration), registration)
        routes = self.call('management.register', {})
        self.assertTrue(routes['ok'], routes)
        menus = [r for r in routes['result']['Resources'] if r.get('Menu')]
        self.assertEqual(len(menus), 1)
        self.assertEqual(menus[0]['Path'], '/status')
        self.assertIn('/healthz', [r['Path'] for r in routes['result']['Resources']])

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
        version = 'v' + self.rpc('bridge.hello')['result']['version']
        for omitted in ('Author', 'GitHubRepository'):
            broken = self.call('plugin.register', {'schema_version': 6})
            del broken['result']['metadata'][omitted]
            with self.cpa_resources(broken) as url:
                # The original installer accepted both live sockets in this state.
                with self.assertRaisesRegex(installer.InstallError, 'CPA 未注册'):
                    installer.probe(self.runtime, version, os.getpid(), version,
                                    url+'/v0/resource/plugins/codex-window-refresh/healthz')
        with self.cpa_resources() as url:
            ready = installer.probe(self.runtime, version, os.getpid(), version,
                                    url+'/v0/resource/plugins/codex-window-refresh/healthz')
            self.assertEqual(ready['version'], version)
            with contextlib.closing(http.client.HTTPConnection('127.0.0.1', int(url.rsplit(':', 1)[1]))) as conn:
                conn.request('GET', '/v0/resource/plugins/codex-window-refresh/status')
                response = conn.getresponse()
                self.assertEqual(response.status, 200)
                self.assertIn(b'<html', response.read().lower())

    def cleanup(self):
        self.release.set()
        if self.process:
            self.process.terminate()
            self.process.communicate(timeout=10)
        self.api.shutdown()
        self.assertEqual(self.calls, self.frees, 'host-owned buffer leak')
        self.assertEqual(self.models, 0, 'model side effect')
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
        if 'model' in method:
            self.models += 1
            return 1
        if method == 'host.auth.list':
            value = {'files': [{'id': name, 'auth_index': name, 'provider': 'codex'} for name in ('a', 'b')]}
        elif method == 'host.auth.get_runtime':
            if req['auth_index'] == 'slow':
                self.entered.set()
                self.release.wait(20)
            value = {'auth': {'id': req['auth_index'], 'auth_index': req['auth_index'], 'provider': 'codex'}}
        elif method == 'host.auth.get':
            value = {'auth_index': req['auth_index'], 'json': {'type': 'codex', 'account_id': 'fixture-upstream'}}
        else:
            return 1
        raw = json.dumps({'ok': True, 'result': value}).encode()
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

    def rpc(self, method, payload=None, instance=''):
        request = json.dumps({'protocol_version': 1, 'request_id': 'fixture', 'bridge_instance_id': instance,
                              'remaining_timeout_ms': 15000, 'method': method, 'payload': payload or {}}).encode()
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
        self.assertEqual(self.rpc('host.model.execute_stream', {}, instance)['error']['code'], 'method_not_allowed')
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
