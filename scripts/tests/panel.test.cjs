'use strict';
const {test} = require('node:test');
const assert = require('node:assert/strict');
const {createSession, createEditor, resetTime, countdown, apiBase, readPanelKey, quotaFailure} = require('../../internal/plugin/web/panel.js');
const response = (value, status = 200, type = 'application/json') => ({ok: status >= 200 && status < 300, status, headers: {get: () => type}, json: async () => value});
test('quota errors show safe stages and distinguish management authentication from upstream rejection', () => {
  const management = quotaFailure({last_error: 'quota_management_auth_failed', diagnostic: {stage: 'management_http', http_status: 403, elapsed_ms: 120}});
  const upstream = quotaFailure({last_error: 'quota_upstream_forbidden', diagnostic: {stage: 'upstream_http', http_status: 403, elapsed_ms: 80}});
  assert.match(management, /后台管理密钥认证失败/); assert.match(management, /管理接口 · HTTP 403 · 120 毫秒/);
  assert.match(upstream, /上游拒绝访问/); assert.match(upstream, /上游响应 · HTTP 403/);
  assert.match(quotaFailure({last_error: 'quota_host_operation_failed'}), /接口兼容性/);
  assert.match(quotaFailure({last_error: 'quota_query_timeout'}), /查询超时/);
  assert.equal(quotaFailure({last_error: 'private-token', diagnostic: {stage: 'private-proxy', http_status: 999, elapsed_ms: -1}}), '可手动重试');
  assert.equal(quotaFailure({last_error: '__proto__', diagnostic: {stage: 'constructor'}}), '可手动重试');
});
test('host connection check timeout is read-only and never retries', async () => {
  let expire, duration, calls = 0;
  const session = createSession(async (_, options) => {calls++; return new Promise((_, reject) => options.signal.addEventListener('abort', () => reject(new Error('private network detail'))));}, null, {setTimeout: (fn, ms) => {expire=fn;duration=ms;return 1;}, clearTimeout: () => {}});
  session.setKey('test-key');
  const pending = session.request('/quota-connection-check', 'POST');
  assert.equal(duration, 15000); expire();
  await assert.rejects(pending, {code: 'timeout'}); assert.equal(calls, 1); session.disconnect();
});
function policy() {
  return {schema_version: 1, enabled: false, timezone: 'Asia/Shanghai', activity: {lookback: '1800s', accounts_per_user: 1, max_accounts: 10, identity_source: 'host_caller_scope'}, ordinary_rules: {allowed_hours: ['08:00-23:00'], allow_outside_hours_when_users_active: true}, reset_rules: {five_hour: {enabled: true, random_delay: {min: '0s', max: '600s'}}, weekly: {enabled: true, random_delay: {min: '0s', max: '600s'}, bypass_activity_limit: true, bypass_allowed_hours: true}}, daily_rules: [], execution: {model: 'test', prompt: 'Reply OK.', request_timeout: '60s', verification_capacity_hold: '900s', verify_delays: ['5s', '30s', '120s']}, account_overrides: {existing: {weekly_enabled: false, allowed_hours: ['09:00-18:00']}}};
}
function deferred() { let resolve, reject; const promise = new Promise((ok, fail) => {resolve = ok; reject = fail;}); return {promise, resolve, reject}; }
function panelContext(entries = {}, origin = 'https://cpa.test') {
  return {localStorage: {getItem: name => entries[name] ?? null}, location: new URL(origin), navigator: {userAgent: 'fixture browser'}, TextEncoder, TextDecoder, atob};
}
function obfuscated(value, context) {
  const data = new TextEncoder().encode(JSON.stringify(value));
  const mask = new TextEncoder().encode('cli-proxy-api-webui::secure-storage|' + context.location.host + '|' + context.navigator.userAgent);
  return 'enc::v1::' + Buffer.from(data.map((byte, i) => byte ^ mask[i % mask.length])).toString('base64');
}
test('CPA saved auth state and legacy keys are reused without modifying storage', () => {
  for (const encoded of [false, true]) {
    for (const legacy of [false, true]) {
      const context = panelContext();
      const entries = legacy ? {managementKey: '  管理-key  ', apiBase: 'https://cpa.test'}
        : {'cli-proxy-auth': {state: {managementKey: '  管理-key  ', apiBase: 'https://cpa.test', rememberPassword: true}, version: 0}};
      for (const name of Object.keys(entries)) entries[name] = encoded ? obfuscated(entries[name], context) : JSON.stringify(entries[name]);
      const reads = [];
      context.localStorage = {getItem: name => {reads.push(name); return entries[name] ?? null;}};
      assert.equal(readPanelKey(context, apiBase({origin: context.location.origin, pathname: '/v0/resource/plugins/codex-window-refresh/status'})), '管理-key');
      assert.ok(reads.every(name => ['cli-proxy-auth', 'managementKey', 'apiBase', 'apiUrl'].includes(name)));
    }
  }
  assert.equal(readPanelKey(panelContext({managementKey: 'legacy-raw-key'}), '/v0/management/codex-window-refresh'), 'legacy-raw-key');
});
test('saved credentials for another server or proxy prefix are never reused', () => {
  const base = '/cpa/v0/management/codex-window-refresh';
  for (const apiBase of ['https://other.test/cpa', 'http://cpa.test/cpa', 'https://cpa.test/other', 'https://cpa.test', 'https://user@cpa.test/cpa', 'https://cpa.test/cpa?x=1', 'https://cpa.test/cpa#x', 'invalid']) {
    const entries = {'cli-proxy-auth': JSON.stringify({state: {managementKey: 'secret', apiBase}})};
    assert.equal(readPanelKey(panelContext(entries), base), '', apiBase);
    assert.equal(readPanelKey(panelContext({managementKey: 'secret', apiUrl: JSON.stringify(apiBase)}), base), '', apiBase);
  }
  assert.equal(readPanelKey(panelContext({'cli-proxy-auth': JSON.stringify({state: {managementKey: 'secret', apiBase: 'https://cpa.test/cpa/'}})}), base), 'secret');
});
test('missing, blocked, malformed or damaged CPA storage leaves manual login available', () => {
  const base = '/v0/management/codex-window-refresh';
  for (const entry of [null, '{}', '{bad', 'null', '[]', 'enc::v1::bad!', 'enc::v1::AAAA', JSON.stringify({state: {managementKey: '\nsecret'}}), JSON.stringify({state: {managementKey: 123}})]) {
    assert.equal(readPanelKey(panelContext({'cli-proxy-auth': entry}), base), '');
  }
  const context = panelContext();
  Object.defineProperty(context, 'localStorage', {get: () => {throw new Error('storage denied');}});
  assert.equal(readPanelKey(context, base), '');
  const mismatched = panelContext({managementKey: obfuscated('secret'.repeat(20), panelContext())}, 'https://cpa.test:8443');
  assert.equal(readPanelKey(mismatched, base), '');
});
test('authenticated requests use the fixed same-origin API and omit cookies and redirects', async () => {
  const calls = [], session = createSession(async (url, options) => {calls.push([url, options]); return response({ok: true});});
  session.setKey('private-test-key');
  await session.request('/summary');
  assert.equal(calls[0][0], '/v0/management/codex-window-refresh/summary');
  assert.equal(calls[0][1].headers.Authorization, 'Bearer private-test-key');
  assert.equal(calls[0][1].credentials, 'omit'); assert.equal(calls[0][1].redirect, 'error'); assert.equal(calls[0][1].cache, 'no-store');
  await assert.rejects(session.request('https://other.invalid'), {code: 'invalid_endpoint'});
  await assert.rejects(session.request('/summary', 'DELETE'), {code: 'invalid_endpoint'});
  assert.equal(calls.length, 1); session.disconnect();
});
test('authentication rejection disconnects, aborts peers, and never reads the arbitrary response body', async () => {
  const peer = deferred(); let peerSignal, bodies = 0;
  const session = createSession(async (_, options) => {
    if (!peerSignal) {peerSignal = options.signal; return peer.promise;}
    return {...response(null, 403), json: async () => {bodies++; throw new Error('secret body');}};
  });
  session.setKey('test-key');
  const pending = session.request('/accounts');
  await assert.rejects(session.request('/summary'), {code: 'authentication', status: 403});
  assert.equal(session.connected(), false); assert.equal(peerSignal.aborted, true); assert.equal(bodies, 0);
  peer.resolve(response({old: 'account'})); await assert.rejects(pending, {code: 'stale'});
  await assert.rejects(session.request('/summary'), {code: 'disconnected'});
});
test('a late response from the old key cannot publish after reconnect, including deferred JSON decode', async () => {
  const json = deferred(); let count = 0;
  const session = createSession(async () => (++count === 1 ? {...response(null), json: () => json.promise} : response({new: true})));
  session.setKey('first-key'); const first = session.request('/accounts');
  await new Promise(resolve => setImmediate(resolve));
  session.setKey('second-key'); assert.deepEqual(await session.request('/summary'), {new: true});
  json.resolve({old: 'private account'}); await assert.rejects(first, {code: 'stale'}); session.disconnect();
});
test('write timeout leaves the result unconfirmed and does not retry', async () => {
  let timeout, duration, count = 0;
  const session = createSession(async (_, options) => {count++; return new Promise((_, reject) => options.signal.addEventListener('abort', () => reject(new Error('aborted'))));}, null, {setTimeout: (callback, ms) => {timeout = callback; duration = ms; return 1;}, clearTimeout: () => {}});
  session.setKey('test-key'); const pending = session.request('/config', 'POST', {config: policy(), revision: 'rev'});
  assert.equal(duration, 120000); timeout(); await assert.rejects(pending, {code: 'write_unconfirmed'}); assert.equal(count, 1); assert.equal(session.connected(), true); session.disconnect();
});
test('read timeout is distinct from writes and malformed success data is rejected', async () => {
  let timeout, duration;
  const session = createSession(async (_, options) => new Promise((_, reject) => options.signal.addEventListener('abort', () => reject(new Error('aborted')))), null, {setTimeout: (callback, ms) => {timeout = callback; duration = ms; return 1;}, clearTimeout: () => {}});
  session.setKey('test-key'); const pending = session.request('/summary'); assert.equal(duration, 15000); timeout(); await assert.rejects(pending, {code: 'timeout'}); session.disconnect();
  const malformed = createSession(async () => response('login page', 200, 'text/html')); malformed.setKey('test-key'); await assert.rejects(malformed.request('/summary'), {code: 'invalid_response'}); malformed.disconnect();
});
test('configuration conflict preserves the authenticated session and the complete draft', async () => {
  const editor = createEditor(); editor.load({config: policy(), revision: 'before'}, true); editor.update('activity.accounts_per_user', 2);
  const session = createSession(async () => response('arbitrary server text', 409)); session.setKey('test-key');
  await assert.rejects(session.request('/config', 'POST', {config: editor.value(), revision: editor.revision()}), {code: 'conflict'});
  assert.equal(session.connected(), true); assert.equal(editor.value().activity.accounts_per_user, 2); assert.equal(editor.revision(), 'before'); assert.equal(editor.dirty(), true); session.disconnect();
});
test('form edits preserve advanced fields, overrides and original input objects', () => {
  const original = policy(), editor = createEditor(); editor.load({config: original, revision: 'r1'}, true);
  editor.update('activity.accounts_per_user', 1.5); editor.update('ordinary_rules.allowed_hours', ['09:00-22:00']); editor.pause('existing', true);
  assert.deepEqual(editor.value().execution, original.execution);
  assert.deepEqual(editor.value().account_overrides.existing, {...original.account_overrides.existing, paused: true});
  assert.equal(original.activity.accounts_per_user, 1); assert.equal(original.account_overrides.existing.paused, undefined);
  const copy = editor.value(); copy.execution.model = 'mutated'; assert.equal(editor.value().execution.model, 'test');
});
test('quota controls preserve model wake settings and use only the quota endpoint', async()=>{
  const initial={...policy(),quota_query:{automatic:true,manual:true,interval:'30s'}},editor=createEditor();editor.load({config:initial,revision:'r1'},true);
  editor.update('quota_query.automatic',false);editor.update('quota_query.manual',false);editor.update('quota_query.interval','120s');
  assert.equal(editor.value().enabled,false);assert.deepEqual(editor.value().execution,initial.execution);
  const calls=[],session=createSession(async(url,options)=>{calls.push([url,options]);return response({status:'queued'},202)});session.setKey('test-key');
  await session.request('/quota-query','POST');assert.equal(calls.length,1);assert.equal(calls[0][0],'/v0/management/codex-window-refresh/quota-query');assert.equal(calls[0][1].body,undefined);session.disconnect();
});
test('account IDs are literal keys and cannot alter object prototypes', () => {
  const editor = createEditor(); editor.load({config: policy(), revision: 'r1'}, true); editor.pause('__proto__', true); editor.pause('constructor', true);
  const overrides = editor.value().account_overrides;
  assert.equal(Object.hasOwn(overrides, '__proto__'), true); assert.equal(overrides.__proto__.paused, true); assert.equal(Object.hasOwn(overrides, 'constructor'), true);
  assert.equal({}.paused, undefined);
});
test('dirty drafts and unapplied JSON cannot be overwritten by read-only refresh', () => {
  const editor = createEditor(); editor.load({config: policy(), revision: 'r1'}, true); editor.update('timezone', 'UTC');
  assert.equal(editor.load({config: policy(), revision: 'r2'}, false), false); assert.equal(editor.value().timezone, 'UTC'); assert.equal(editor.revision(), 'r1');
  editor.markRaw(); assert.throws(() => editor.update('enabled', true), {code: 'unapplied_json'}); assert.throws(() => editor.pause('account', true), {code: 'unapplied_json'});
  assert.equal(editor.load({config: policy(), revision: 'r2'}, false), false);
  editor.load({config: policy(), revision: 'r2'}, true); assert.equal(editor.dirty(), false); assert.equal(editor.rawDirty(), false);
});
test('saving establishes a new revision without erasing edits made while a write was in flight', () => {
  const editor = createEditor(); editor.load({config: policy(), revision: 'r1'}, true); editor.update('enabled', true);
  const saved = editor.value(), version = editor.version(); editor.update('activity.accounts_per_user', 3);
  editor.saved({config: saved, revision: 'r2'}, version);
  assert.equal(editor.revision(), 'r2'); assert.equal(editor.value().activity.accounts_per_user, 3); assert.equal(editor.dirty(), true);
  const later = editor.version(); editor.saved({config: editor.value(), revision: 'r3'}, later); assert.equal(editor.dirty(), false);
});
test('disconnect cleanup removes draft data and optimistic revision', () => {
  const editor = createEditor(); editor.load({config: policy(), revision: 'private-revision'}, true); editor.pause('private-account', true); editor.markRaw(); editor.clear();
  assert.equal(editor.value(), null); assert.equal(editor.revision(), ''); assert.equal(editor.dirty(), false); assert.equal(editor.rawDirty(), false);
});
test('advanced draft rejects malformed structures before the renderer sees them', () => {
  const editor = createEditor(); editor.load({config: policy(), revision: 'r1'}, true);
  assert.throws(() => editor.replace({enabled: true}), {code: 'invalid_config'});
  assert.throws(() => editor.replace({...policy(), daily_rules: 'wrong'}), {code: 'invalid_config'});
  assert.throws(() => editor.replace({...policy(), account_overrides: {account: null}}), {code: 'invalid_config'});
  assert.equal(editor.value().enabled, false);
});
test('reset countdown uses actual evidence and never invents a new window', () => {
  const now = Date.parse('2026-10-11T00:00:00Z');
  assert.equal(resetTime('0001-01-01T00:00:00Z'), null); assert.equal(resetTime('invalid'), null);
  assert.equal(countdown('', now), '重置时间未知'); assert.equal(countdown('2026-10-11T00:10:00Z', now), '00:10:00');
  assert.equal(countdown('2026-10-10T00:00:00Z', now), '已到重置时间 · 等待最新证据');
  assert.equal(countdown('2026-10-12T01:01:01Z', now), '1 天 01:01:01');
});
test('reverse-proxy prefixes stay on the page origin, including malicious double slash input', () => {
  assert.equal(apiBase({origin: 'https://cpa.test', pathname: '/cpa/v0/resource/plugins/codex-window-refresh/status'}), '/cpa/v0/management/codex-window-refresh');
  assert.equal(apiBase({origin: 'https://cpa.test', pathname: '/v0/resource/plugins/codex-window-refresh/status'}), '/v0/management/codex-window-refresh');
  assert.throws(() => apiBase({origin: 'https://cpa.test', pathname: '//evil.invalid/v0/resource/plugins/codex-window-refresh/status'}), {code: 'invalid_endpoint'});
  assert.throws(() => apiBase({origin: 'https://cpa.test', pathname: '/wrong'}), {code: 'invalid_endpoint'});
});
