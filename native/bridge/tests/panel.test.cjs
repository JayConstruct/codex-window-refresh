'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const {ViewTracker, apiBase, readPanelKey, age, resetText, windowView, accountStatus, requestID, validateQueryDraft, configDraft, queueText} = require('../../../internal/v2/daemon/web/panel.js');
const script = fs.readFileSync(path.resolve(__dirname, '../../../internal/v2/daemon/web/panel.js'), 'utf8');
const view = (instance = 'a', revision = 1) => ({instance_id: instance, view_revision: revision});
const origin = 'http://127.0.0.1:8317';
const location = {origin, host: '127.0.0.1:8317', pathname: '/v0/resource/plugins/codex-window-refresh/status'};
const overview = (revision = 1) => ({...view('a', revision), read_only: true, accounts_revision: revision, query_concurrency: 8,
  inventory: {status: 'ready', last_success_at: new Date().toISOString()}, capabilities: {quota_query: true, passive_quota: true},
  dashboard: {total: 24, available: 20, disabled: 2, unavailable: 2, capacity: 16, unknown_quota: 4, errors: 2, recent_jobs: [], recent_events: []}});
const accountPage = (revision = 1, accounts = [{id: 'codex-01', status: 'available'}]) => ({...view('a', revision), accounts_revision: revision, accounts, next_after: '', total: accounts.length});

test('a slow earlier read cannot overwrite a later read or an accepted write', () => {
  const state = new ViewTracker();
  const early = state.begin('accounts'), late = state.begin('accounts');
  assert.equal(state.accept('accounts', late, view('a', 20)), true);
  assert.equal(state.accept('accounts', early, view('a', 10)), false);
  const pending = state.begin('accounts'); state.barrier();
  assert.equal(state.accept('accounts', pending, view('a', 21)), false);
});
test('retired instances and older unknown responses cannot roll back a restart', () => {
  const state = new ViewTracker();
  assert.equal(state.accept('overview', state.begin('overview'), view('a', 30)), true);
  const old = state.begin('accounts');
  assert.equal(state.accept('overview', state.begin('overview'), view('b', 1)), true);
  assert.equal(state.accept('accounts', old, view('a', 40)), false);
  assert.equal(state.accept('accounts', old, view('unseen-older-instance', 90)), false);
  assert.equal(state.accept('accounts', state.begin('accounts'), view('b', 2)), true);
});
test('versions are monotonic per resource and independent across resources', () => {
  const state = new ViewTracker();
  assert.equal(state.accept('job:x', state.begin('job:x'), view('a', 50)), true);
  assert.equal(state.accept('accounts', state.begin('accounts'), view('a', 40)), true);
  assert.equal(state.accept('accounts', state.begin('accounts'), view('a', 39)), false);
  assert.equal(state.accept('accounts', state.begin('accounts'), view('a', NaN)), false);
});
test('management requests preserve proxy prefixes and reject cross-origin paths', () => {
  assert.equal(apiBase(location), '/v0/management/codex-window-refresh/v2');
  assert.equal(apiBase({...location, pathname: '/cpa' + location.pathname}), '/cpa/v0/management/codex-window-refresh/v2');
  assert.throws(() => apiBase({...location, pathname: '/other-page'}), /invalid_endpoint/);
  assert.throws(() => apiBase({...location, pathname: '//attacker.example' + location.pathname}), /invalid_endpoint/);
});
test('saved host authentication is read-only and restricted to the same mount', () => {
  const entries = new Map([['cli-proxy-auth', JSON.stringify({state: {managementKey: 'local-test-key', apiBase: origin + '/cpa'}})]]);
  const context = {location, localStorage: {getItem: name => entries.get(name) ?? null}};
  assert.equal(readPanelKey(context, '/cpa/v0/management/codex-window-refresh/v2'), 'local-test-key');
  assert.equal(readPanelKey(context, '/v0/management/codex-window-refresh/v2'), '');
  entries.set('cli-proxy-auth', JSON.stringify({state: {managementKey: 'local-test-key', apiBase: 'https://another.example'}}));
  assert.equal(readPanelKey(context, '/v0/management/codex-window-refresh/v2'), '');
  entries.set('managementKey', 'legacy-test-key'); entries.set('apiBase', origin);
  assert.equal(readPanelKey(context, '/v0/management/codex-window-refresh/v2'), 'legacy-test-key');
});
test('obfuscated host credentials are decoded but damaged storage is not reused', () => {
  const context = {location, navigator: {userAgent: 'fixture'}, TextEncoder, TextDecoder, atob};
  const raw = new TextEncoder().encode(JSON.stringify({state: {managementKey: 'test-only', apiBase: origin}}));
  const mask = new TextEncoder().encode('cli-proxy-api-webui::secure-storage|' + location.host + '|fixture');
  for (let index = 0; index < raw.length; index++) raw[index] ^= mask[index % mask.length];
  let value = 'enc::v1::' + Buffer.from(raw).toString('base64');
  context.localStorage = {getItem: name => name === 'cli-proxy-auth' ? value : null};
  assert.equal(readPanelKey(context, apiBase(location)), 'test-only');
  value = 'enc::v1::bad';
  assert.equal(readPanelKey(context, apiBase(location)), '');
});
test('quota bars show remaining percent without treating tokens as percentages', () => {
  const snapshot = {source: 'cpa_wham_usage', five_hour: {Remaining: {Value: 82, Unit: 'percent'}, ResetAt: '2026-10-17T13:00:00Z', ConfirmedAt: '2026-10-17T11:59:00Z'}, fields: {five_hour: {source: 'cpa_response_headers', observed_at: '2026-10-17T12:00:00Z'}}};
  const displayed = windowView(snapshot, 'five_hour', Date.parse('2026-10-17T12:00:00Z'));
  assert.equal(displayed.percent, 82); assert.equal(displayed.source, '正常业务响应');
  assert.match(displayed.reset, /1 小时/); assert.equal(displayed.observedAt, '2026-10-17T12:00:00Z');
  snapshot.five_hour.Remaining = {Value: 2000, Unit: 'tokens'};
  assert.equal(windowView(snapshot, 'five_hour').percent, null);
  assert.equal(windowView(snapshot, 'five_hour').amount, '2000 tokens');
  snapshot.five_hour.Remaining = {Value: 101, Unit: 'percent'};
  assert.equal(windowView(snapshot, 'five_hour').percent, null);
  assert.equal(windowView(null, 'weekly').percent, null);
});
test('past reset times await verification and disabled is not confused with depleted', () => {
  assert.match(resetText('2026-10-17T11:00:00Z', Date.parse('2026-10-17T12:00:00Z')), /待核实/);
  assert.equal(resetText('0001-01-01T00:00:00Z'), '重置时间未知');
  assert.equal(age(undefined), '尚未观测');
  assert.equal(accountStatus({status: 'disabled', quota: {permission: 'blocked'}}).label, '已禁用');
  assert.equal(accountStatus({status: 'available', quota: {permission: 'blocked'}}).label, '额度受限');
  assert.equal(accountStatus({status: 'unavailable'}).label, '暂不可用');
});
test('HTTP deployments can use getRandomValues when randomUUID is unavailable', () => {
  assert.equal(requestID({getRandomValues: bytes => bytes.fill(3)}), '03'.repeat(16));
  assert.equal(requestID({randomUUID() { throw new Error('unavailable'); }, getRandomValues: bytes => bytes.fill(4)}), '04'.repeat(16));
  assert.throws(() => requestID({}), /secure_random_unavailable/);
});

function panel(respond, options = {}) {
  class Element {
    constructor(tag = 'div') {
      this.tagName = tag; this.children = []; this.value = ''; this.checked = false; this.disabled = false; this.hidden = false; this.dataset = {};
      this.attributes = new Map(); this.listeners = new Map(); this._text = ''; this.parentNode = null;
      this.classList = {toggle() {}};
    }
    set textContent(value) { this._text = String(value); this.children = []; }
    get textContent() { return this._text + this.children.map(child => child.textContent).join(''); }
    replaceChildren(...children) { this._text = ''; this.children = []; this.append(...children); }
    append(...children) {
      for (const child of children) {
        if (child.parentNode) child.parentNode.children = child.parentNode.children.filter(item => item !== child);
        child.parentNode = this; this.children.push(child);
      }
    }
    setAttribute(name, value) { this.attributes.set(name, value); }
    removeAttribute(name) { this.attributes.delete(name); }
    addEventListener(name, callback) { this.listeners.set(name, callback); }
  }
  const elements = new Map(), timers = new Map(), calls = [], events = new Map();
  let nextTimer = 0;
  const get = id => { if (!elements.has(id)) elements.set(id, new Element()); return elements.get(id); };
  get('accounts-filter').value = 'all'; get('key').value = 'fixture-key';
  const nav = ['overview', 'accounts', 'query', 'wake', 'models', 'history', 'diagnostics'].map(name => { const node = new Element('button'); node.dataset.page = name; return node; });
  const panels = nav.map(node => { const panel = new Element(); panel.dataset.view = node.dataset.page; return panel; });
  const walk = node => [node, ...node.children.flatMap(walk)];
  const context = vm.createContext({
    document: {getElementById: get, createElement: tag => new Element(tag), createTextNode: text => { const node = new Element('#text'); node.textContent = text; return node; },
      querySelectorAll: selector => {
        if (selector === '[data-page]') return nav;
        if (selector === '[data-view]') return panels;
        const nodes = [...new Set([...elements.values()].flatMap(walk))];
        if (selector.startsWith('.')) return nodes.filter(node => node.className === selector.slice(1));
        const attribute = {'[data-reset-at]': 'resetAt', '[data-observed-at]': 'observedAt', '[data-queue-phase]': 'queuePhase'}[selector];
        return attribute ? nodes.filter(node => Object.hasOwn(node.dataset, attribute)) : [];
      }},
    location: options.location || location, navigator: {userAgent: 'fixture'},
    localStorage: {getItem: name => options.storage?.[name] ?? null},
    fetch: async (url, settings) => {
      calls.push({url, ...settings}); const response = await respond(url, settings, calls);
      return {ok: response[0] < 400, status: response[0], json: async () => response[1]};
    },
    AbortController, URL, TextEncoder, TextDecoder, Uint8Array, atob,
    crypto: options.crypto || {randomUUID: () => 'fixture-idempotency-key'},
    addEventListener: (name, callback) => events.set(name, callback),
    setTimeout: (fn, delay) => { timers.set(++nextTimer, {fn, delay}); return nextTimer; }, clearTimeout: id => timers.delete(id),
  });
  vm.runInContext(script, context);
  async function tick(delay) {
    const entry = [...timers.entries()].find(([, timer]) => timer.delay === delay);
    assert.ok(entry, 'missing timer ' + delay); timers.delete(entry[0]); await entry[1].fn();
  }
  return {get, timers, calls, nav, panels, events, tick};
}
function basic(url) { return url.endsWith('/overview') ? [200, overview()] : [200, accountPage()]; }

test('opening without host credentials does not send requests or show sample data', () => {
  const ui = panel(basic);
  assert.equal(ui.calls.length, 0); assert.match(ui.get('accounts').textContent, /连接 CPA/);
  assert.equal(ui.get('query').disabled, true); assert.equal(ui.get('login-section').hidden, false);
  assert.equal(ui.get('accounts').textContent.includes('codex-01'), false);
});
test('connection shows global metrics, hides credentials and starts automatic refresh', async () => {
  const ui = panel(basic); await ui.get('connect').onclick();
  assert.equal(ui.get('key').value, ''); assert.equal(ui.get('login-section').hidden, true);
  assert.equal(ui.get('metric-total').textContent, '24'); assert.equal(ui.get('metric-capacity').textContent, '16 / 20');
  assert.equal(ui.get('five-hour-query').disabled, true); assert.equal(ui.get('auto-wake').checked, false);
  assert.ok([...ui.timers.values()].some(timer => timer.delay === 5000));
  assert.equal(ui.calls.every(call => call.redirect === 'error' && call.credentials === 'omit'), true);
});
test('account paging sends its revision and reloads the first page on conflict', async () => {
  let reads = 0;
  const ui = panel(url => {
    if (url.endsWith('/overview')) return [200, overview()];
    if (url.includes('after=')) return [409, {error: 'view_changed'}];
    reads++;
    return [200, {...accountPage(reads, [{id: reads === 1 ? 'old-first' : 'new-first', status: 'available'}]), next_after: reads === 1 ? 'cursor-a' : ''}];
  });
  await ui.get('connect').onclick(); await ui.get('next').onclick();
  assert.ok(ui.calls.some(call => call.url.endsWith('/accounts?limit=6&after=cursor-a&revision=1')));
  assert.equal(reads, 2); assert.match(ui.get('accounts').textContent, /new-first/); assert.equal(ui.get('next').disabled, true);
});
test('refresh stops after CPA authentication expires without retrying authentication', async () => {
  let overviews = 0;
  const ui = panel(url => url.endsWith('/overview') ? (++overviews === 1 ? [200, overview()] : [401, {error: 'private-error'}]) : [200, accountPage()]);
  await ui.get('connect').onclick(); await ui.tick(5000);
  assert.match(ui.get('connection').textContent, /认证失效/); assert.equal(ui.get('query').disabled, true);
  assert.equal(ui.timers.size, 0); assert.equal(ui.get('error').textContent.includes('private-error'), false);
});
test('background quota changes reload the table after the overview revision advances', async () => {
  let overviews = 0, accounts = 0;
  const ui = panel(url => url.endsWith('/overview') ? [200, overview(++overviews)] : [200, accountPage(++accounts, [{id: accounts === 1 ? 'initial' : 'updated', status: 'available'}])]);
  await ui.get('connect').onclick(); await ui.tick(5000);
  assert.equal(accounts, 2); assert.match(ui.get('accounts').textContent, /updated/);
  assert.equal(ui.calls.filter(call => call.method === 'POST').length, 0);
});
test('a failed account read is retried even when the overview revision stops changing', async () => {
  let overviews = 0, accountReads = 0;
  const ui = panel(url => {
    if (url.endsWith('/overview')) return [200, overview(++overviews === 1 ? 1 : 2)];
    accountReads++;
    if (accountReads === 2) return [503, {error: 'storage_unavailable'}];
    return [200, accountPage(accountReads === 1 ? 1 : 2, [{id: accountReads === 1 ? 'original' : 'recovered', status: 'available'}])];
  });
  await ui.get('connect').onclick(); await ui.tick(5000);
  assert.match(ui.get('accounts').textContent, /original/); assert.equal(ui.get('error').hidden, false);
  await ui.tick(5000);
  assert.equal(accountReads, 3); assert.match(ui.get('accounts').textContent, /recovered/); assert.equal(ui.get('error').hidden, true);
});
test('automatic refresh preserves the current cursor and uses filtered totals', async () => {
  let revision = 1;
  const ui = panel(url => {
    if (url.endsWith('/overview')) return [200, overview(revision)];
    if (url.includes('after=')) return [200, {...accountPage(revision, [{id: 'codex-07', status: 'available'}]), offset: 6, total: 24, filter_total: 8}];
    return [200, {...accountPage(revision), next_after: 'codex-06', total: 24, filter_total: 8}];
  });
  await ui.get('connect').onclick(); await ui.get('next').onclick(); revision = 2; await ui.tick(5000);
  assert.ok(ui.calls.some(call => call.url.endsWith('&after=codex-06&revision=2')));
  assert.equal(ui.get('accounts-page-info').textContent, '7–7 / 8');
  assert.match(ui.get('accounts').textContent, /codex-07/);
});
test('inventory failure is never described as a successful synchronization', async () => {
  const failed = overview(); failed.inventory = {status: 'unavailable', error_code: 'inventory_sync_failed'};
  failed.dashboard.recent_events = [{kind: 'inventory', phase: 'unavailable'}];
  const ui = panel(url => url.endsWith('/overview') ? [200, failed] : [200, accountPage()]);
  await ui.get('connect').onclick();
  assert.match(ui.get('recent-events').textContent, /同步失败/);
  assert.equal(ui.get('recent-events').textContent.includes('同步完成'), false);
});
test('manual single query is immediate, carries no model inputs and retries the same key', async () => {
  let writes = 0;
  const ui = panel((url, settings) => {
    if (url.endsWith('/overview')) return [200, overview()];
    if (settings.method === 'POST') {
      writes++; if (writes === 1) throw new Error('fixture network failure');
      return [202, {job_id: 'query-one', instance_id: 'a'}];
    }
    if (url.includes('/jobs?')) return [200, {...view(), job_id: 'query-one', account_id: 'codex-01', phase: 'completed', items: [{phase: 'completed'}]}];
    return [200, accountPage()];
  });
  await ui.get('connect').onclick();
  const row = ui.get('accounts').children[0], button = row.children[5].children[0];
  await button.onclick();
  const requests = ui.calls.filter(call => call.method === 'POST');
  assert.equal(requests.length, 2); assert.equal(requests[0].body, requests[1].body);
  assert.equal(requests[0].headers['Idempotency-Key'], requests[1].headers['Idempotency-Key']);
  assert.deepEqual(JSON.parse(requests[0].body), {kind: 'quota_query', account_id: 'codex-01'});
  assert.match(ui.get('notice').textContent, /立即处理/);
});
test('bulk action uses the durable query endpoint and never emits a model request', async () => {
  const ui = panel((url, settings) => settings.method === 'POST' ? [202, {job_id: 'bulk', instance_id: 'a'}] : url.includes('/jobs?') ? [200, {...view(), job_id: 'bulk', phase: 'running', items: []}] : basic(url));
  await ui.get('connect').onclick(); await ui.get('query').onclick();
  const requests = ui.calls.filter(call => call.method === 'POST');
  assert.equal(requests.length, 1); assert.deepEqual(JSON.parse(requests[0].body), {kind: 'quota_query', account_id: ''});
  assert.match(ui.get('notice').textContent, /关闭页面不会取消/);
  assert.ok([...ui.timers.values()].some(timer => timer.delay === 1500));
});
test('query authentication failure is not automatically retried', async () => {
  const ui = panel((url, settings) => settings.method === 'POST' ? [401, {error: 'unauthorized'}] : basic(url));
  await ui.get('connect').onclick(); await ui.get('query').onclick();
  assert.equal(ui.calls.filter(call => call.method === 'POST').length, 1);
  assert.equal(ui.get('query').disabled, true); assert.equal(ui.timers.size, 0);
});
test('a missing cryptographic source cannot leave querying permanently disabled', async () => {
  const ui = panel(basic, {crypto: {}}); await ui.get('connect').onclick(); await ui.get('query').onclick();
  assert.equal(ui.get('query').disabled, false);
  assert.match(ui.get('error').textContent, /安全的任务标识/);
  assert.equal(ui.calls.filter(call => call.method === 'POST').length, 0);
});
test('unconfirmed jobs retain the same intent across a later click and reconnect', async () => {
  let writes = 0, identifiers = 0;
  const ui = panel((url, settings) => {
    if (settings.method === 'POST') {
      if (++writes <= 2) throw new Error('lost acceptance response');
      return [202, {job_id: 'accepted-before-loss', instance_id: 'a'}];
    }
    if (url.includes('/jobs?')) return [200, {...view(), job_id: 'accepted-before-loss', phase: 'completed', items: []}];
    return basic(url);
  }, {crypto: {randomUUID: () => 'fixture-key-' + ++identifiers}});
  await ui.get('connect').onclick(); await ui.get('query').onclick();
  assert.match(ui.get('error').textContent, /尚未确认/); assert.equal(ui.get('query').textContent, '重试批量查询');
  ui.get('disconnect').onclick(); ui.get('key').value = 'fixture-key'; await ui.get('connect').onclick();
  await ui.get('query').onclick();
  const requests = ui.calls.filter(call => call.method === 'POST');
  assert.equal(requests.length, 3); assert.equal(identifiers, 1);
  assert.equal(new Set(requests.map(request => request.headers['Idempotency-Key'])).size, 1);
});
test('sync queues only CPA inventory refresh, with no account/model payload', async () => {
  const ui = panel((url, settings) => settings.method === 'POST' ? [202, {status: 'queued'}] : basic(url));
  await ui.get('connect').onclick(); await ui.get('sync').onclick();
  const request = ui.calls.find(call => call.method === 'POST');
  assert.ok(request.url.endsWith('/sync')); assert.equal(request.body, undefined);
  assert.match(ui.get('notice').textContent, /不查询上游额度/);
});
test('paging waits for the sync account read and then advances with the same cursor', async () => {
  let revision = 1, finishAccountRead;
  const ui = panel((url, settings) => {
    if (settings.method === 'POST') { revision = 2; return [202, {status: 'queued'}]; }
    if (url.endsWith('/overview')) return [200, {...overview(revision), accounts_revision: 1}];
    if (url.includes('after=')) return [200, {...accountPage(1, [{id: 'codex-07', status: 'available'}]), ...view('a', revision), total: 24}];
    const response = [200, {...accountPage(1), ...view('a', revision), next_after: 'codex-06', total: 24}];
    return revision === 2 ? new Promise(resolve => { finishAccountRead = () => resolve(response); }) : response;
  });
  await ui.get('connect').onclick();
  const sync = ui.get('sync').onclick();
  await flush();
  assert.equal(typeof finishAccountRead, 'function');
  assert.equal(ui.get('sync').disabled, true);
  assert.equal(ui.get('next').disabled, true);
  const reads = ui.calls.length;
  await ui.get('next').onclick();
  assert.equal(ui.calls.length, reads, 'An in-flight account read must prevent paging');
  assert.match(ui.get('accounts').textContent, /codex-01/);
  finishAccountRead(); await sync;
  assert.equal(ui.get('sync').disabled, false);
  assert.equal(ui.get('next').disabled, false);
  await ui.get('next').onclick();
  assert.ok(ui.calls.some(call => call.url.endsWith('/accounts?limit=6&after=codex-06&revision=1')));
  assert.match(ui.get('accounts').textContent, /codex-07/);
});
test('search and filter apply to the backend account pool, not only the current page', async () => {
  const ui = panel(basic); await ui.get('connect').onclick();
  ui.get('accounts-search').value = 'another & account'; ui.get('accounts-filter').value = 'disabled';
  ui.get('accounts-search').listeners.get('input')(); await ui.tick(200);
  assert.ok(ui.calls.some(call => call.url.endsWith('/accounts?limit=6&search=another%20%26%20account&status=disabled')));
});
test('same-origin saved authentication connects automatically on opening', async () => {
  const ui = panel(basic, {storage: {'cli-proxy-auth': JSON.stringify({state: {managementKey: 'fixture-auto-key', apiBase: origin}})}});
  for (let turn = 0; turn < 10; turn++) await new Promise(resolve => setImmediate(resolve));
  assert.equal(ui.get('login-section').hidden, true);
  assert.equal(ui.calls[0].headers.Authorization, 'Bearer fixture-auto-key');
  assert.ok([...ui.timers.values()].some(timer => timer.delay === 5000));
});
test('disconnect clears timers and disables stale per-account actions', async () => {
  const ui = panel(basic); await ui.get('connect').onclick(); ui.get('disconnect').onclick();
  assert.equal(ui.timers.size, 0); assert.equal(ui.get('query').disabled, true);
  const rowButton = ui.get('accounts').children[0].children[5].children[0];
  assert.equal(rowButton.disabled, true); const reads = ui.calls.length; await rowButton.onclick(); assert.equal(ui.calls.length, reads);
});
test('navigation moves the same table without losing current account data', async () => {
  const ui = panel(basic); await ui.get('connect').onclick();
  const accountNav = ui.nav.find(node => node.dataset.page === 'accounts'); accountNav.listeners.get('click')();
  assert.equal(ui.get('account-card').parentNode, ui.get('accounts-page-slot'));
  assert.equal(ui.get('page-title').textContent, '账号额度');
  assert.equal(ui.panels.find(node => node.dataset.view === 'overview').hidden, true);
  assert.equal(ui.panels.find(node => node.dataset.view === 'accounts').hidden, false);
});
test('account IDs and server failures remain plain text, never injected HTML', async () => {
  const malicious = '<img src=x onerror=alert(1)>';
  const ui = panel(url => url.endsWith('/overview') ? [200, overview()] : [200, accountPage(1, [{id: malicious, status: 'available'}])]);
  await ui.get('connect').onclick();
  assert.match(ui.get('accounts').textContent, /<img src=x/);
  assert.equal(script.includes('innerHTML'), false);
  assert.equal(script.includes('observed_recent_users'), false);
});

const queryConfig = () => ({timezone: 'Asia/Shanghai', automatic: false, interval_seconds: 300, on_five_hour_expiry: false,
  on_weekly_expiry: false, random_delay: {min_seconds: 5, max_seconds: 60}, excluded_hours: []});
const querySettings = (revision = 1, config = queryConfig(), viewRevision = revision) => ({...view('a', viewRevision), revision, config});
function configuredOverview(settings = querySettings(), revision = settings.view_revision) {
  return {...overview(revision), capabilities: {quota_query: true, query_settings: true, automatic_quota_query: true, randomized_quota_query: true, model_execution: false},
    query_settings: {revision: settings.revision, config: settings.config}};
}
function edit(ui, id, value) {
  const node = ui.get(id);
  if (typeof value === 'boolean') node.checked = value; else node.value = value;
  node.listeners.get('input')();
}
const submitSettings = ui => ui.get('query-settings-form').onsubmit({preventDefault() {}});
const flush = async () => { for (let turn = 0; turn < 8; turn++) await new Promise(resolve => setImmediate(resolve)); };

test('query validation rejects invalid numbers, timezones, spans and overlapping hours', () => {
  const draft = configDraft(queryConfig());
  assert.deepEqual(validateQueryDraft(draft).errors, {});
  for (const interval of ['', '29', '3601', '30.5', '3e2']) assert.ok(validateQueryDraft({...draft, interval}).errors.interval);
  for (const delayMin of ['-1', '86401', 'NaN']) assert.ok(validateQueryDraft({...draft, delayMin}).errors.delayMin);
  assert.ok(validateQueryDraft({...draft, delayMin: '30', delayMax: '29'}).errors.delayMax);
  assert.ok(validateQueryDraft({...draft, delayMax: '86401'}).errors.delayMax);
  for (const timezone of ['', 'Local', 'Mars/Colony', '+08:00']) assert.ok(validateQueryDraft({...draft, timezone}).errors.timezone);
  for (const excluded of ['24:00-08:00', '00:00-24:01', '8:00-09:00', '08:00-08:00', '00:00-01:00,', '23:00-02:00, 01:00-03:00']) assert.ok(validateQueryDraft({...draft, excluded}).errors.excluded, excluded);
  const valid = validateQueryDraft({...draft, interval: '30', delayMin: '0', delayMax: '86400', timezone: 'UTC', excluded: '23:00-02:00, 12:00-13:00, 22:00-23:00'});
  assert.deepEqual(valid.errors, {}); assert.equal(valid.config.excluded_hours.length, 3);
  assert.deepEqual(validateQueryDraft({...draft, excluded: '00:00-24:00'}).errors, {});
});
test('overview settings populate editor without extra reads or writes and model controls stay disabled', async () => {
  const ui = panel(url => url.endsWith('/overview') ? [200, configuredOverview()] : [200, accountPage()]);
  await ui.get('connect').onclick();
  ui.nav.find(node => node.dataset.page === 'query').listeners.get('click')(); await flush();
  assert.equal(ui.get('settings-interval').value, '300'); assert.equal(ui.get('settings-fields').disabled, false);
  assert.equal(ui.get('settings-save').disabled, true); assert.equal(ui.get('five-hour-query').disabled, false);
  assert.equal(ui.get('auto-wake').disabled, true); assert.equal(ui.get('auto-wake').checked, false);
  assert.match(ui.get('account-query-note').textContent, /按账号随机延迟/);
  assert.equal(ui.calls.some(call => call.url.endsWith('/settings') || call.method === 'POST'), false);
});
test('home switches stage a draft, retain saved state and never save on navigation', async () => {
  const ui = panel(url => url.endsWith('/overview') ? [200, configuredOverview()] : [200, accountPage()]);
  await ui.get('connect').onclick(); ui.get('five-hour-query').checked = true; await ui.get('five-hour-query').onchange();
  assert.equal(ui.get('page-title').textContent, '额度查询'); assert.equal(ui.get('settings-five-hour').checked, true);
  assert.equal(ui.get('five-hour-query').checked, false); assert.equal(ui.get('settings-save').disabled, false);
  assert.match(ui.get('settings-diff').textContent, /关闭 → 开启/);
  ui.nav.find(node => node.dataset.page === 'overview').listeners.get('click')();
  await ui.tick(5000);
  assert.equal(ui.get('settings-five-hour').checked, true); assert.equal(ui.get('settings-status').textContent, '未保存草稿');
  assert.equal(ui.calls.some(call => call.method === 'POST'), false);
});
test('polling updates saved settings without overwriting dirty draft or allowing stale saves', async () => {
  let settings = querySettings();
  const ui = panel(url => url.endsWith('/overview') ? [200, configuredOverview(settings)] : [200, accountPage(settings.view_revision)]);
  await ui.get('connect').onclick(); edit(ui, 'settings-interval', '90');
  await ui.tick(5000); assert.equal(ui.get('settings-interval').value, '90');
  settings = querySettings(2, {...queryConfig(), interval_seconds: 180, on_weekly_expiry: true});
  await ui.tick(5000);
  assert.equal(ui.get('settings-interval').value, '90'); assert.equal(ui.get('weekly-query').checked, true);
  assert.equal(ui.get('settings-conflict').hidden, false); assert.equal(ui.get('settings-save').disabled, true);
  await submitSettings(ui); assert.equal(ui.calls.some(call => call.method === 'POST'), false);
});
test('clean drafts follow actual settings polling without a write', async () => {
  let settings = querySettings();
  const ui = panel(url => url.endsWith('/overview') ? [200, configuredOverview(settings)] : [200, accountPage(settings.view_revision)]);
  await ui.get('connect').onclick(); settings = querySettings(2, {...queryConfig(), automatic: true, interval_seconds: 120});
  await ui.tick(5000);
  assert.equal(ui.get('settings-interval').value, '120'); assert.equal(ui.get('settings-automatic').checked, true);
  assert.equal(ui.get('settings-save').disabled, true); assert.equal(ui.calls.some(call => call.method === 'POST'), false);
});
test('invalid editor fields cannot be saved even with direct form submission', async () => {
  const ui = panel(url => url.endsWith('/overview') ? [200, configuredOverview()] : [200, accountPage()]);
  await ui.get('connect').onclick(); edit(ui, 'settings-interval', '20'); edit(ui, 'settings-delay-max', '4');
  edit(ui, 'settings-timezone', 'bad/timezone'); edit(ui, 'settings-excluded', '10:00-10:00');
  await submitSettings(ui);
  assert.equal(ui.get('settings-save').disabled, true); assert.equal(ui.get('settings-validation').hidden, false);
  assert.equal(ui.get('settings-timezone').attributes.get('aria-invalid'), 'true');
  assert.equal(ui.calls.some(call => call.method === 'POST'), false);
});
test('settings use explicit optimistic save with only revision/config and saved switches update', async () => {
  let settings = querySettings();
  const ui = panel((url, options) => {
    if (url.endsWith('/settings') && options.method === 'POST') {
      const body = JSON.parse(options.body); settings = querySettings(2, body.config); return [200, settings];
    }
    return url.endsWith('/overview') ? [200, configuredOverview(settings)] : [200, accountPage(settings.view_revision)];
  });
  await ui.get('connect').onclick(); edit(ui, 'settings-automatic', true); edit(ui, 'settings-weekly', true);
  edit(ui, 'settings-interval', '60'); edit(ui, 'settings-excluded', '23:00-07:00, 12:00-13:00');
  await submitSettings(ui);
  const writes = ui.calls.filter(call => call.method === 'POST'); assert.equal(writes.length, 1);
  assert.deepEqual(JSON.parse(writes[0].body), {revision: 1, config: {...queryConfig(), automatic: true, on_weekly_expiry: true, interval_seconds: 60, excluded_hours: ['23:00-07:00', '12:00-13:00']}});
  assert.equal(ui.get('settings-save').disabled, true); assert.equal(ui.get('weekly-query').checked, true);
  assert.match(ui.get('settings-message').textContent, /已保存并应用/);
  assert.equal(ui.get('auto-wake').disabled, true);
});
test('409 keeps draft and requires explicit reload/discard before a new save', async () => {
  let settings = querySettings(), conflicting = true;
  const ui = panel((url, options) => {
    if (url.endsWith('/settings')) {
      if (options.method === 'POST' && conflicting) {
        settings = querySettings(2, {...queryConfig(), interval_seconds: 180}); return [409, {error: 'settings_conflict'}];
      }
      if (options.method === 'POST') { settings = querySettings(3, JSON.parse(options.body).config); }
      return [200, settings];
    }
    return url.endsWith('/overview') ? [200, configuredOverview(settings)] : [200, accountPage(settings.view_revision)];
  });
  await ui.get('connect').onclick(); edit(ui, 'settings-interval', '90'); await submitSettings(ui);
  assert.equal(ui.get('settings-interval').value, '90'); assert.equal(ui.get('settings-conflict').hidden, false);
  await ui.tick(5000); assert.equal(ui.get('settings-interval').value, '90');
  await submitSettings(ui); assert.equal(ui.calls.filter(call => call.method === 'POST').length, 1);
  await ui.get('settings-reload').onclick(); assert.equal(ui.get('settings-interval').value, '180');
  assert.equal(ui.get('settings-conflict').hidden, true); assert.equal(ui.get('settings-save').disabled, true);
  conflicting = false; edit(ui, 'settings-interval', '120'); await submitSettings(ui);
  assert.equal(JSON.parse(ui.calls.filter(call => call.method === 'POST')[1].body).revision, 2);
});
test('failed storage or invalid settings responses preserve the editable draft', async () => {
  for (const [status, code] of [[400, 'invalid_query_settings'], [503, 'storage_unavailable']]) {
    const ui = panel((url, options) => options.method === 'POST' ? [status, {error: code}] : url.endsWith('/overview') ? [200, configuredOverview()] : [200, accountPage()]);
    await ui.get('connect').onclick(); edit(ui, 'settings-interval', '90'); await submitSettings(ui);
    assert.equal(ui.get('settings-interval').value, '90'); assert.equal(ui.get('settings-save').disabled, false);
    assert.equal(ui.get('settings-conflict').hidden, true); assert.match(ui.get('settings-message').textContent, status === 400 ? /无效/ : /存储/);
    assert.equal(ui.calls.filter(call => call.method === 'POST').length, 1);
  }
});
test('lost settings save response does not retry writes and requires explicit verification', async () => {
  const ui = panel((url, options) => {
    if (options.method === 'POST') throw new Error('lost write response');
    return url.endsWith('/overview') ? [200, configuredOverview()] : [200, accountPage()];
  });
  await ui.get('connect').onclick(); edit(ui, 'settings-interval', '90'); await submitSettings(ui);
  assert.equal(ui.calls.filter(call => call.method === 'POST').length, 1);
  assert.equal(ui.get('settings-save').disabled, true); assert.match(ui.get('settings-message').textContent, /尚未确认/);
});
test('opening query editor fetches missing config only for capable backends', async () => {
  const withoutConfig = configuredOverview(); delete withoutConfig.query_settings;
  const ui = panel(url => url.endsWith('/overview') ? [200, withoutConfig] : url.endsWith('/settings') ? [200, querySettings()] : [200, accountPage()]);
  await ui.get('connect').onclick(); assert.equal(ui.calls.some(call => call.url.endsWith('/settings')), false);
  ui.nav.find(node => node.dataset.page === 'query').listeners.get('click')(); await flush();
  assert.equal(ui.calls.filter(call => call.url.endsWith('/settings')).length, 1); assert.equal(ui.get('settings-interval').value, '300');
  const legacy = panel(basic); await legacy.get('connect').onclick();
  legacy.nav.find(node => node.dataset.page === 'query').listeners.get('click')(); await flush();
  assert.equal(legacy.calls.some(call => call.url.endsWith('/settings')), false); assert.equal(legacy.get('settings-fields').disabled, true);
});
test('queue-only view changes reload accounts and failed reads do not commit their revision', async () => {
  let revision = 1, reads = 0;
  const due = new Date(Date.now() + 120000).toISOString();
  const ui = panel(url => {
    if (url.endsWith('/overview')) return [200, {...configuredOverview(querySettings(), revision), accounts_revision: 1}];
    reads++;
    if (reads === 2) return [200, {...view('a', 2), accounts_revision: 1, accounts: 'invalid'}];
    return [200, {...accountPage(1, [{id: 'codex-01', status: 'available', pending_query: revision > 1 ? {phase: 'queued', scheduled_at: due, trigger: 'manual_batch', hold_reason: 'excluded_hours'} : undefined}]), ...view('a', revision)}];
  });
  await ui.get('connect').onclick(); revision = 2; await ui.tick(5000);
  assert.equal(ui.get('error').hidden, false); await ui.tick(5000); assert.equal(reads, 3);
  assert.match(ui.get('accounts').textContent, /等待查询 · 手动批量 · 处于排除时段/);
  assert.match(ui.get('accounts').textContent, /后计划查询/);
  assert.equal(ui.get('accounts').children[0].children[5].children[0].textContent, '立即查询');
  assert.equal(ui.get('accounts').children[0].children[5].children[0].disabled, false);
});
test('queue countdown, trigger and hold reasons appear in task summaries and item detail', async () => {
  const due = new Date(Date.now() + 90000).toISOString();
  const job = {...view(), id: 'queued-job', phase: 'queued', trigger: 'five_hour_expiry', scheduled_at: due, waiting: 1, total: 1,
    items: [{account_id: 'codex-01', phase: 'queued', trigger: 'five_hour_expiry', scheduled_at: due, hold_reason: 'automatic_query_disabled'}]};
  const data = configuredOverview(); data.dashboard.recent_jobs = [job];
  const ui = panel(url => url.endsWith('/overview') ? [200, data] : [200, accountPage()]);
  await ui.get('connect').onclick();
  assert.match(ui.get('tasks').textContent, /5 小时到期.*等待 1/);
  assert.match(ui.get('history-list').textContent, /自动查询规则已关闭/);
  assert.match(ui.get('history-list').textContent, /后计划查询/);
  assert.match(queueText({...job, scheduled_at: '2000-01-01T00:00:00Z'}), /等待后台执行/);
  assert.equal(queueText({phase: 'running', trigger: 'manual_single'}), '查询中 · 手动单账号');
});
test('overall running jobs with future due times or waiting items are rendered as waiting queries', async () => {
  for (const scheduling of [{waiting: 2}, {scheduled_at: new Date(Date.now() + 120000).toISOString()}, {items: [{account_id: 'codex-01', phase: 'queued', scheduled_at: '2000-01-01T00:00:00Z', hold_reason: 'excluded_hours'}]}]) {
    const data = configuredOverview();
    data.dashboard.recent_jobs = [{id: 'delayed', phase: 'running', trigger: 'periodic', total: 2, ...scheduling}];
    const ui = panel(url => url.endsWith('/overview') ? [200, data] : [200, accountPage()]);
    await ui.get('connect').onclick();
    assert.match(ui.get('tasks').textContent, /等待查询/); assert.match(ui.get('job').textContent, /等待查询/);
    if (scheduling.scheduled_at) assert.match(ui.get('tasks').textContent, /后计划查询/);
  }
});
test('mixed batches remain querying while other items wait for random delay or exclusions', async () => {
  const due = new Date(Date.now() + 120000).toISOString();
  for (const scheduling of [{querying: 1, waiting: 2}, {items: [{phase: 'running', account_id: 'codex-01'}, {phase: 'queued', account_id: 'codex-02', scheduled_at: due, hold_reason: 'excluded_hours'}]}]) {
    const data = configuredOverview();
    data.dashboard.recent_jobs = [{id: 'mixed', phase: 'running', trigger: 'manual_batch', scheduled_at: due, total: 3, ...scheduling}];
    const ui = panel(url => url.endsWith('/overview') ? [200, data] : [200, accountPage()]);
    await ui.get('connect').onclick();
    const heading = ui.get('tasks').children[0].children[0];
    assert.match(heading.textContent, /查询中/); assert.doesNotMatch(heading.textContent, /等待查询/);
    assert.match(ui.get('job').textContent, /查询中/);
    assert.match(ui.get('tasks').textContent, /等待 [12]/);
    assert.match(ui.get('tasks').textContent, /后计划查询/);
    if (scheduling.items) assert.match(ui.get('history-list').textContent, /处于排除时段/);
  }
});
test('overdue automatic jobs held by excluded hours show waiting with the hold reason', async () => {
  for (const scheduling of [{querying: 0, waiting: 2}, {items: [{phase: 'queued', account_id: 'codex-01', hold_reason: 'excluded_hours'}]}, {}]) {
    const data = configuredOverview();
    data.dashboard.recent_jobs = [{id: 'excluded', phase: 'running', trigger: 'periodic', scheduled_at: '2000-01-01T00:00:00Z', hold_reason: 'excluded_hours', total: 2, ...scheduling}];
    const ui = panel(url => url.endsWith('/overview') ? [200, data] : [200, accountPage()]);
    await ui.get('connect').onclick();
    assert.match(ui.get('tasks').children[0].children[0].textContent, /等待查询/);
    assert.match(ui.get('tasks').textContent, /处于排除时段/);
    assert.match(ui.get('tasks').textContent, /已到计划时间，等待后台执行/);
    assert.match(ui.get('job').textContent, /等待查询/);
  }
});
test('server-supported timezone aliases do not block overview polling or explicit settings reload', async () => {
  let settings = querySettings(1, {...queryConfig(), timezone: 'Factory'}), reads = 0;
  const ui = panel(url => {
    if (url.endsWith('/overview')) { reads++; return [200, configuredOverview(settings)]; }
    if (url.endsWith('/settings')) return [200, settings];
    return [200, accountPage(settings.view_revision)];
  });
  await ui.get('connect').onclick();
  assert.equal(ui.get('login-section').hidden, true); assert.equal(ui.get('settings-timezone').value, 'Factory');
  assert.equal(ui.get('metric-total').textContent, '24'); assert.equal(ui.get('error').hidden, true);
  await ui.tick(5000); assert.equal(reads, 2); assert.equal(ui.get('error').hidden, true);
  await ui.get('settings-reload').onclick();
  assert.equal(ui.get('settings-timezone').value, 'Factory'); assert.match(ui.get('settings-message').textContent, /已重新加载/);
  edit(ui, 'settings-interval', '90');
  if (validateQueryDraft(configDraft(settings.config)).errors.timezone) {
    assert.equal(ui.get('settings-validation').hidden, false); assert.equal(ui.get('settings-save').disabled, true);
    await submitSettings(ui); assert.equal(ui.calls.some(call => call.method === 'POST'), false);
  }
  settings = querySettings(2, {...queryConfig(), timezone: 'UTC'});
  await ui.tick(5000); assert.equal(ui.get('settings-timezone').value, 'Factory');
  await ui.get('settings-reload').onclick();
  assert.equal(ui.get('settings-timezone').value, 'UTC'); assert.equal(ui.get('settings-validation').hidden, true);
});
test('a backend restart keeps a dirty draft and requires explicit reload before saving', async () => {
  let instance = 'a';
  const ui = panel(url => url.endsWith('/overview') ? [200, {...configuredOverview(), instance_id: instance}] : [200, {...accountPage(), instance_id: instance}]);
  await ui.get('connect').onclick(); edit(ui, 'settings-interval', '90'); instance = 'b';
  await ui.tick(5000);
  assert.equal(ui.get('settings-interval').value, '90'); assert.equal(ui.get('settings-conflict').hidden, false);
  assert.equal(ui.get('settings-save').disabled, true); await submitSettings(ui);
  assert.equal(ui.calls.some(call => call.method === 'POST'), false);
});
test('settings reads cannot roll back a newer config accepted from polling', async () => {
  let settings = querySettings(), resolveRead;
  const ui = panel(url => {
    if (url.endsWith('/settings')) return new Promise(resolve => { resolveRead = resolve; });
    return url.endsWith('/overview') ? [200, configuredOverview(settings)] : [200, accountPage(settings.view_revision)];
  });
  await ui.get('connect').onclick(); const loading = ui.get('settings-reload').onclick();
  settings = querySettings(2, {...queryConfig(), interval_seconds: 120});
  await ui.tick(5000); resolveRead([200, querySettings()]); await loading;
  assert.equal(ui.get('settings-interval').value, '120'); assert.equal(ui.get('settings-status').textContent, '版本 2');
});
test('randomized bulk submission sends no client delay, schedule, settings or model fields', async () => {
  const ui = panel((url, options) => options.method === 'POST' ? [202, {job_id: 'bulk', instance_id: 'a'}] : url.includes('/jobs?') ? [200, {...view(), id: 'bulk', phase: 'queued', waiting: 2, trigger: 'manual_batch', items: []}] : url.endsWith('/overview') ? [200, configuredOverview()] : [200, accountPage()]);
  await ui.get('connect').onclick(); await ui.get('query').onclick();
  const posts = ui.calls.filter(call => call.method === 'POST');
  assert.equal(posts.length, 1); assert.deepEqual(JSON.parse(posts[0].body), {kind: 'quota_query', account_id: ''});
  assert.match(ui.get('notice').textContent, /每个账号由后台随机延迟并持久排队/);
  assert.equal(ui.calls.some(call => /model|wake|completion/.test(call.url)), false);
});
