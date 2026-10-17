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
test('account badges distinguish depletion, restrictions and host state without guessing recovery', () => {
  const exhausted = {status: 'available', quota: {permission: 'allowed', five_hour: {ResetAt: '2000-01-01T00:00:00Z', Remaining: {Value: 0, Unit: 'percent'}}}};
  assert.equal(accountStatus(exhausted).label, '额度耗尽');
  assert.equal(accountStatus({...exhausted, status: 'disabled', display_status: 'quota_exhausted'}).label, '已禁用');
  assert.equal(accountStatus({...exhausted, status: 'binding_unavailable'}).label, '身份待核实');
  assert.equal(accountStatus({...exhausted, status: 'unavailable'}).label, '暂不可用');
  assert.equal(accountStatus({status: 'available', quota: {weekly: {Remaining: {Value: 0, Unit: 'percent'}}}}).label, '额度耗尽');
  assert.equal(accountStatus({status: 'available', display_status: 'quota_exhausted'}).label, '额度耗尽');
  assert.equal(accountStatus({status: 'available', display_status: 'quota_restricted'}).label, '额度受限');
  assert.equal(accountStatus({status: 'available', quota: {five_hour: {Remaining: {Value: '0', Unit: 'percent'}}}}).label, '正常');
  assert.equal(accountStatus({status: 'unexpected'}).label, '状态未知');
});
test('full five-hour and weekly placeholders never start a display countdown as time passes', () => {
  const observed = Date.parse('2026-10-17T12:00:00.750Z');
  for (const [name, seconds] of [['five_hour', 18000], ['weekly', 604800]]) {
    const window = {ResetAt: new Date(observed + seconds * 1000).toISOString(), ConfirmedAt: new Date(observed).toISOString(), ResetAfterSeconds: seconds, Remaining: {Value: 100, Unit: 'percent'}};
    for (const elapsed of [0, 1000, 60000, seconds * 1000 + 1000]) {
      assert.equal(windowView({[name]: window}, name, observed + elapsed).reset, '');
    }
    delete window.ResetAfterSeconds;
    window.ResetAt = new Date(Math.floor(observed / 1000) * 1000 + seconds * 1000).toISOString();
    assert.equal(windowView({[name]: window}, name, observed + 60000).reset, '');
  }
});
test('non-full countdowns remain visible independently of percentage, state and window length', () => {
  const observed = Date.parse('2026-10-17T12:00:00Z');
  for (const [name, full] of [['five_hour', 18000], ['weekly', 604800]]) {
    for (const seconds of [0, 1, full - 1, full + 1]) {
      const window = {ResetAt: new Date(observed + seconds * 1000).toISOString(), ConfirmedAt: new Date(observed).toISOString(), ResetAfterSeconds: seconds, State: 'unknown', Remaining: {Value: 100, Unit: 'percent'}};
      assert.equal(windowView({[name]: window}, name, observed).reset, resetText(window.ResetAt, observed));
      delete window.ResetAfterSeconds;
      assert.equal(windowView({[name]: window}, name, observed).reset, resetText(window.ResetAt, observed));
    }
  }
});
test('reported countdown takes precedence over rounded absolute timestamps and global observations', () => {
  const observed = Date.parse('2026-10-17T12:00:00Z');
  for (const [name, full] of [['five_hour', 18000], ['weekly', 604800]]) {
    const window = {ResetAt: new Date(observed + (full - 1) * 1000).toISOString(), ConfirmedAt: new Date(observed).toISOString(), ResetAfterSeconds: full};
    const snapshot = {[name]: window, observed_at: new Date(observed + 60000).toISOString()};
    assert.equal(windowView(snapshot, name, observed).reset, '');
    window.ResetAt = new Date(observed + full * 1000).toISOString(); window.ResetAfterSeconds = full - 1;
    assert.equal(windowView(snapshot, name, observed).reset, resetText(window.ResetAt, observed));
    delete window.ResetAfterSeconds;
    assert.equal(windowView(snapshot, name, observed).reset, '');
    delete window.ConfirmedAt;
    snapshot.fields = {[name]: {observed_at: new Date(observed).toISOString()}};
    assert.equal(windowView(snapshot, name, observed).reset, '');
  }
});
test('HTTP deployments can use getRandomValues when randomUUID is unavailable', () => {
  assert.equal(requestID({getRandomValues: bytes => bytes.fill(3)}), '03'.repeat(16));
  assert.equal(requestID({randomUUID() { throw new Error('unavailable'); }, getRandomValues: bytes => bytes.fill(4)}), '04'.repeat(16));
  assert.throws(() => requestID({}), /secure_random_unavailable/);
});

function panel(respond, options = {}) {
  class Element {
    constructor(tag = 'div') {
      this.nodeType = tag === '#text' ? 3 : 1;
      this.tagName = tag; this.children = []; this.value = ''; this.checked = false; this.disabled = false; this.hidden = false; this.dataset = {};
      this.attributes = new Map(); this.listeners = new Map(); this._text = ''; this.parentNode = null;
      this.classList = {toggle() {}};
    }
    set textContent(value) { this._text = String(value); this.children = []; }
    get textContent() { return this._text + this.children.map(child => child.textContent).join(''); }
    get childNodes() { return this.children; }
    replaceChildren(...children) { this._text = ''; this.children = []; this.append(...children); }
    append(...children) {
      for (const child of children) {
        if (child.parentNode) child.parentNode.children = child.parentNode.children.filter(item => item !== child);
        child.parentNode = this; this.children.push(child);
      }
    }
    setAttribute(name, value) { this.attributes.set(name, value); }
    getAttributeNames() { return [...this.attributes.keys()]; }
    getAttribute(name) { return this.attributes.get(name) ?? null; }
    hasAttribute(name) { return this.attributes.has(name); }
    removeAttribute(name) { this.attributes.delete(name); }
    addEventListener(name, callback) { this.listeners.set(name, callback); }
    insertBefore(child, before) {
      if (child.parentNode) child.parentNode.removeChild(child);
      const index = before ? this.children.indexOf(before) : this.children.length;
      this._text = ''; child.parentNode = this; this.children.splice(index, 0, child);
    }
    removeChild(child) { this.children.splice(this.children.indexOf(child), 1); child.parentNode = null; return child; }
  }
  const elements = new Map(), timers = new Map(), calls = [], events = new Map(), storage = options.storage || {};
  let nextTimer = 0;
  const get = id => { if (!elements.has(id)) elements.set(id, new Element()); return elements.get(id); };
  get('accounts-filter').value = 'all'; get('key').value = 'fixture-key';
  const nav = ['overview', 'accounts', 'query', 'wake', 'models', 'history', 'diagnostics'].map(name => { const node = new Element('button'); node.dataset.page = name; return node; });
  const panels = nav.map(node => { const panel = new Element(); panel.dataset.view = node.dataset.page; return panel; });
  const walk = node => [node, ...node.children.flatMap(walk)];
  const context = vm.createContext({
    document: {documentElement: get('document-root'), getElementById: get, createElement: tag => new Element(tag), createTextNode: text => { const node = new Element('#text'); node.textContent = text; return node; },
      querySelectorAll: selector => {
        if (selector === '[data-page]') return nav;
        if (selector === '[data-view]') return panels;
        const nodes = [...new Set([...elements.values()].flatMap(walk))];
        if (selector.startsWith('.')) return nodes.filter(node => node.className === selector.slice(1));
        const attribute = {'[data-reset-at]': 'resetAt', '[data-observed-at]': 'observedAt', '[data-queue-phase]': 'queuePhase'}[selector];
        return attribute ? nodes.filter(node => Object.hasOwn(node.dataset, attribute)) : [];
      }},
    location: options.location || location, navigator: {userAgent: 'fixture'},
    localStorage: {getItem: name => storage[name] ?? null, setItem: (name, value) => { if (options.storageBlocked) throw new Error('storage unavailable'); storage[name] = value; }},
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
  return {get, timers, calls, nav, panels, events, tick, storage};
}
function basic(url) { return url.endsWith('/overview') ? [200, overview()] : [200, accountPage()]; }

test('account rendering and refresh keep full placeholders out of the countdown timer', async () => {
  const observed = Date.now(), stamp = new Date(observed).toISOString();
  const quota = (short, week) => ({five_hour: {ResetAfterSeconds: short, ResetAt: new Date(observed + short * 1000).toISOString(), ConfirmedAt: stamp, Remaining: {Value: 100, Unit: 'percent'}}, weekly: {ResetAfterSeconds: week, ResetAt: new Date(observed + week * 1000).toISOString(), ConfirmedAt: stamp, Remaining: {Value: 100, Unit: 'percent'}}});
  let revision = 0;
  const snapshots = [quota(18000, 604800), quota(17999, 604800), quota(18000, 604799), quota(18000, 604800)];
  const ui = panel(url => url.endsWith('/overview') ? [200, overview(++revision)] : [200, accountPage(revision, [{id: 'countdown-account', status: 'available', quota: snapshots[Math.min(revision - 1, snapshots.length - 1)]}])]);
  const countdowns = node => (Object.hasOwn(node.dataset, 'resetAt') ? [node] : []).concat(node.children.flatMap(countdowns));
  await ui.get('connect').onclick();
  assert.equal(countdowns(ui.get('accounts')).length, 0);
  assert.equal(ui.get('accounts').textContent.includes('100%'), true);
  await ui.tick(5000);
  assert.equal(countdowns(ui.get('accounts')).length, 1);
  assert.match(countdowns(ui.get('accounts'))[0].textContent, /后重置/);
  await ui.tick(5000);
  assert.equal(countdowns(ui.get('accounts')).length, 1);
  assert.match(countdowns(ui.get('accounts'))[0].textContent, /后重置/);
  await ui.tick(5000);
  assert.equal(countdowns(ui.get('accounts')).length, 0);
  await ui.tick(5000);
  assert.equal(countdowns(ui.get('accounts')).length, 0);
  assert.equal(ui.calls.some(call => call.method === 'POST'), false);
});

test('sidebar collapse is local, reversible and restores its mount-specific preference', async () => {
  const storage = {}, ui = panel(basic, {storage});
  assert.equal(ui.get('app-sidebar').dataset.collapsed, 'false');
  assert.equal(ui.get('sidebar-toggle').attributes.get('aria-expanded'), 'true');
  ui.get('sidebar-toggle').onclick();
  assert.equal(ui.get('app-sidebar').dataset.collapsed, 'true');
  assert.equal(ui.get('sidebar-toggle').attributes.get('aria-label'), '展开侧栏');
  assert.equal(ui.get('sidebar-toggle').attributes.get('aria-expanded'), 'false');
  assert.equal(storage['cwr-v2-sidebar:/v0/management/codex-window-refresh/v2'], 'true');
  assert.equal(ui.calls.length, 0);
  const restored = panel(basic, {storage});
  assert.equal(restored.get('app-sidebar').dataset.collapsed, 'true');
  restored.get('sidebar-toggle').onclick();
  assert.equal(restored.get('app-sidebar').dataset.collapsed, 'false');
  assert.equal(restored.get('sidebar-toggle').attributes.get('aria-expanded'), 'true');
  assert.equal(restored.get('sidebar-toggle-label').textContent, '收起侧栏');
  assert.equal(restored.calls.length, 0);
  const otherMount = panel(basic, {storage, location: {...location, pathname: '/another' + location.pathname}});
  assert.equal(otherMount.get('app-sidebar').dataset.collapsed, 'false');
});
test('sidebar remains usable when browser storage is blocked and navigation keeps accessible labels', () => {
  const ui = panel(basic, {storageBlocked: true});
  assert.doesNotThrow(() => ui.get('sidebar-toggle').onclick());
  assert.equal(ui.get('app-sidebar').dataset.collapsed, 'true');
  for (const item of ui.nav) {
    assert.ok(item.attributes.get('aria-label')); assert.ok(item.attributes.get('title'));
  }
  ui.nav.find(node => node.dataset.page === 'query').listeners.get('click')();
  assert.equal(ui.get('page-title').textContent, '额度查询');
  assert.equal(ui.get('app-sidebar').dataset.collapsed, 'true');
  assert.equal(ui.calls.length, 0);
});

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
test('account updates reuse rows and buttons while refreshing quota, labels and event handlers', async () => {
  let revision = 0;
  const ui = panel(url => url.endsWith('/overview') ? [200, overview(++revision)] : [200, accountPage(revision, [{id: 'stable-account', status: revision === 1 ? 'available' : 'disabled', pending_query: revision === 1 ? {phase: 'queued', trigger: 'manual_batch'} : undefined}])]);
  await ui.get('connect').onclick();
  const row = ui.get('accounts').children[0], action = row.children[5], button = action.children[0];
  await ui.tick(5000);
  assert.equal(ui.get('accounts').children[0], row);
  assert.equal(row.children[5].children[0], button);
  assert.match(row.textContent, /已禁用/);
  assert.equal(action.children.length, 1);
  assert.equal(ui.calls.some(call => call.method === 'POST'), false);
});
test('query history keeps expanded results and the same summary while task progress changes', async () => {
  let revision = 1;
  const summary = () => ({id: 'stable-history', phase: 'running', total: 1, completed: revision > 1 ? 1 : 0, failed: 0});
  const ui = panel(url => url.includes('/jobs?job_id=') ? [200, {...view('a', revision), job_id: 'stable-history', phase: 'running', items: [{account_id: 'one', phase: 'running'}]}] : url.endsWith('/overview') ? [200, {...overview(revision), dashboard: {...overview().dashboard, recent_jobs: [summary()]}}] : [200, accountPage()]);
  await ui.get('connect').onclick();
  const button = ui.get('history-list').children[0].children.at(-1); await button.onclick();
  const card = ui.get('history-list').children[0], details = card.children.find(node => node.tagName === 'details');
  const heading = details.children[0]; details.open = true;
  revision++; await ui.tick(1500);
  assert.equal(ui.get('history-list').children[0], card);
  assert.equal(card.children.find(node => node.tagName === 'details'), details);
  assert.equal(details.children[0], heading); assert.equal(details.open, true);
});
test('background account reads do not disable pagination and obsolete errors cannot reset a newer page', async () => {
  let reads = 0, revision = 0, releaseBackground, releasePage;
  const response = () => [200, {...accountPage(revision), next_after: 'cursor'}];
  const ui = panel(url => {
    if (url.endsWith('/overview')) return [200, overview(++revision)];
    reads++;
    if (reads === 2) return new Promise(resolve => { releaseBackground = () => resolve([409, {error: 'view_changed'}]); });
    if (reads === 3) return new Promise(resolve => { releasePage = () => resolve(response()); });
    return response();
  });
  await ui.get('connect').onclick();
  const polling = ui.tick(5000); await flush();
  assert.equal(ui.get('next').disabled, false);
  const paging = ui.get('next').onclick(); await flush();
  assert.equal(ui.get('next').disabled, true);
  releaseBackground(); await polling;
  assert.equal(ui.get('next').disabled, true);
  assert.equal(reads, 3, 'An obsolete background failure reset the new page');
  releasePage(); await paging;
  assert.equal(ui.get('next').disabled, false);
});
test('periodic status reads update disabled, exhausted and recovered badges without requesting quota', async () => {
  let revision = 0;
  const accounts = [{id: 'status-account', status: 'available'}, {id: 'status-account', status: 'disabled'},
    {id: 'status-account', status: 'available', display_status: 'quota_exhausted', quota: {five_hour: {Remaining: {Value: 0, Unit: 'percent'}}}},
    {id: 'status-account', status: 'available', display_status: 'available', quota: {five_hour: {Remaining: {Value: 100, Unit: 'percent'}}}}];
  const ui = panel(url => url.endsWith('/overview') ? [200, {...overview(++revision), inventory: {status: 'ready', refresh_interval_seconds: 30}, dashboard: {...overview().dashboard, normal: 18, quota_exhausted: 1, quota_restricted: 1}}] : [200, accountPage(revision, [accounts[Math.min(revision - 1, 3)]])]);
  await ui.get('connect').onclick(); assert.match(ui.get('accounts').textContent, /正常/);
  await ui.tick(5000); assert.match(ui.get('accounts').textContent, /已禁用/);
  await ui.tick(5000); assert.match(ui.get('accounts').textContent, /额度耗尽/);
  await ui.tick(5000); assert.match(ui.get('accounts').textContent, /正常/);
  assert.match(ui.get('inventory').textContent, /30 秒/);
  assert.match(ui.get('metric-total-detail').textContent, /18 正常.*1 额度耗尽.*1 额度受限/);
  assert.equal(ui.calls.some(call => call.method === 'POST'), false);
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
test('home switches save only their selected rule immediately without leaving overview', async () => {
  for (const [id, field, editor] of [['five-hour-query', 'on_five_hour_expiry', 'settings-five-hour'], ['weekly-query', 'on_weekly_expiry', 'settings-weekly']]) {
    let settings = querySettings();
    const ui = panel((url, options) => {
      if (url.endsWith('/settings') && options.method === 'POST') {
        const request = JSON.parse(options.body);
        assert.deepEqual(Object.keys(request).sort(), ['config', 'revision']);
        assert.equal(request.revision, settings.revision);
        settings = querySettings(settings.revision + 1, request.config); return [200, settings];
      }
      return url.endsWith('/overview') ? [200, configuredOverview(settings)] : [200, accountPage(settings.view_revision)];
    });
    await ui.get('connect').onclick();
    for (const checked of [true, false]) {
      ui.get(id).checked = checked; await ui.get(id).onchange();
      assert.equal(ui.get('page-title').textContent, '运行总览');
      assert.equal(ui.get(id).checked, checked); assert.equal(ui.get(editor).checked, checked);
      assert.equal(ui.get(id).disabled, false); assert.equal(ui.get('settings-save').disabled, true);
      assert.deepEqual(settings.config, {...queryConfig(), [field]: checked});
      assert.match(ui.get('rules-message').textContent, /已.*并保存/);
      assert.equal(ui.get('auto-wake').disabled, true);
    }
    assert.equal(ui.calls.filter(call => call.method === 'POST').length, 2);
    await ui.get(id).onchange(); await ui.tick(5000);
    assert.equal(ui.calls.filter(call => call.method === 'POST').length, 2);
  }
});
test('home save preserves dirty editor fields but never sends them to the backend', async () => {
  let settings = querySettings();
  const ui = panel((url, options) => {
    if (url.endsWith('/settings') && options.method === 'POST') {
      settings = querySettings(settings.revision + 1, JSON.parse(options.body).config); return [200, settings];
    }
    return url.endsWith('/overview') ? [200, configuredOverview(settings)] : [200, accountPage(settings.view_revision)];
  });
  await ui.get('connect').onclick(); edit(ui, 'settings-interval', '20'); edit(ui, 'settings-delay-min', '17'); edit(ui, 'settings-weekly', true);
  ui.get('five-hour-query').checked = true; await ui.get('five-hour-query').onchange();
  assert.deepEqual(settings.config, {...queryConfig(), on_five_hour_expiry: true});
  assert.equal(ui.get('settings-interval').value, '20'); assert.equal(ui.get('settings-delay-min').value, '17');
  assert.equal(ui.get('settings-weekly').checked, true); assert.equal(ui.get('settings-five-hour').checked, true);
  assert.equal(ui.get('settings-conflict').hidden, true); assert.equal(ui.get('settings-save').disabled, true);
  assert.match(ui.get('settings-message').textContent, /仍为草稿/);
  edit(ui, 'settings-interval', '90'); await submitSettings(ui);
  const request = JSON.parse(ui.calls.filter(call => call.method === 'POST')[1].body);
  assert.equal(request.revision, 2); assert.equal(request.config.interval_seconds, 90);
  assert.equal(request.config.on_five_hour_expiry, true); assert.equal(request.config.on_weekly_expiry, true);
});
test('pending home save disables both switches and editor saves without duplicate writes', async () => {
  let complete;
  const ui = panel((url, options) => {
    if (options.method === 'POST') return new Promise(resolve => { complete = () => resolve([200, querySettings(2, JSON.parse(options.body).config)]); });
    return url.endsWith('/overview') ? [200, configuredOverview()] : [200, accountPage()];
  });
  await ui.get('connect').onclick(); edit(ui, 'settings-interval', '90');
  ui.get('five-hour-query').checked = true; const pending = ui.get('five-hour-query').onchange(); await flush();
  assert.equal(ui.get('five-hour-query').checked, true); assert.equal(ui.get('five-hour-query').disabled, true);
  assert.equal(ui.get('weekly-query').disabled, true); assert.equal(ui.get('settings-save').disabled, true);
  assert.equal(ui.get('five-hour-query').attributes.get('aria-busy'), 'true');
  ui.get('weekly-query').checked = true; await ui.get('weekly-query').onchange(); await submitSettings(ui);
  assert.equal(ui.get('weekly-query').checked, false); assert.equal(ui.calls.filter(call => call.method === 'POST').length, 1);
  complete(); await pending;
  assert.equal(ui.get('five-hour-query').checked, true); assert.equal(ui.get('five-hour-query').disabled, false);
  assert.equal(ui.get('settings-interval').value, '90'); assert.equal(ui.get('settings-save').disabled, false);
});
test('rejected home saves roll back switches and display errors in place', async () => {
  for (const [status, error] of [[400, 'invalid_query_settings'], [503, 'storage_unavailable']]) {
    const ui = panel((url, options) => options.method === 'POST' ? [status, {error}] : url.endsWith('/overview') ? [200, configuredOverview()] : [200, accountPage()]);
    await ui.get('connect').onclick(); ui.get('weekly-query').checked = true; await ui.get('weekly-query').onchange();
    assert.equal(ui.get('weekly-query').checked, false); assert.equal(ui.get('weekly-query').disabled, false);
    assert.equal(ui.get('page-title').textContent, '运行总览'); assert.equal(ui.get('rules-message').hidden, false);
    assert.match(ui.get('rules-message').textContent, /开关未保存/); assert.equal(ui.get('rules-reload').hidden, true);
    assert.equal(ui.calls.filter(call => call.method === 'POST').length, 1);
  }
});
test('conflicting home save requires an explicit read and never overwrites unrelated server settings or drafts', async () => {
  let settings = querySettings(), conflict = true;
  const ui = panel((url, options) => {
    if (url.endsWith('/settings')) {
      if (options.method === 'POST') {
        if (conflict) { settings = querySettings(2, {...queryConfig(), interval_seconds: 180, on_weekly_expiry: true}); return [409, {error: 'settings_conflict'}]; }
        assert.equal(JSON.parse(options.body).revision, 2);
        settings = querySettings(3, JSON.parse(options.body).config);
      }
      return [200, settings];
    }
    return url.endsWith('/overview') ? [200, configuredOverview(settings)] : [200, accountPage(settings.view_revision)];
  });
  await ui.get('connect').onclick(); edit(ui, 'settings-interval', '90');
  ui.get('five-hour-query').checked = true; await ui.get('five-hour-query').onchange();
  assert.equal(ui.get('five-hour-query').checked, false); assert.equal(ui.get('five-hour-query').disabled, true);
  assert.equal(ui.get('rules-reload').hidden, false); assert.match(ui.get('rules-message').textContent, /设置已变化/);
  await ui.tick(5000); await ui.get('five-hour-query').onchange();
  assert.equal(ui.get('five-hour-query').disabled, true); assert.equal(ui.calls.filter(call => call.method === 'POST').length, 1);
  await ui.get('rules-reload').onclick();
  assert.equal(ui.get('page-title').textContent, '运行总览'); assert.equal(ui.get('weekly-query').checked, true);
  assert.equal(ui.get('settings-interval').value, '90'); assert.equal(ui.get('settings-conflict').hidden, false);
  assert.equal(ui.get('five-hour-query').disabled, false); conflict = false;
  ui.get('five-hour-query').checked = true; await ui.get('five-hour-query').onchange();
  assert.equal(settings.config.interval_seconds, 180); assert.equal(settings.config.on_weekly_expiry, true);
  assert.equal(settings.config.on_five_hour_expiry, true); assert.equal(ui.get('settings-interval').value, '90');
  assert.equal(ui.get('settings-conflict').hidden, false); assert.equal(ui.get('settings-save').disabled, true);
});
test('lost home save acknowledgement stays locked through polling until explicitly verified without retry', async () => {
  let settings = querySettings();
  const ui = panel((url, options) => {
    if (url.endsWith('/settings')) {
      if (options.method === 'POST') { settings = querySettings(2, JSON.parse(options.body).config); throw new Error('lost response'); }
      return [200, settings];
    }
    return url.endsWith('/overview') ? [200, configuredOverview(settings)] : [200, accountPage(settings.view_revision)];
  });
  await ui.get('connect').onclick(); ui.get('weekly-query').checked = true; await ui.get('weekly-query').onchange();
  assert.equal(ui.get('weekly-query').checked, false); assert.equal(ui.get('weekly-query').disabled, true);
  assert.match(ui.get('rules-message').textContent, /尚未确认/); assert.equal(ui.get('rules-reload').hidden, false);
  await ui.tick(5000);
  assert.equal(ui.get('weekly-query').checked, true); assert.equal(ui.get('weekly-query').disabled, true);
  edit(ui, 'settings-interval', '90'); await submitSettings(ui); await ui.get('weekly-query').onchange();
  assert.equal(ui.calls.filter(call => call.method === 'POST').length, 1);
  await ui.get('rules-reload').onclick();
  assert.equal(ui.get('weekly-query').disabled, false); assert.equal(ui.get('settings-interval').value, '90');
  assert.equal(ui.calls.filter(call => call.method === 'POST').length, 1);
});
test('late home save cannot replace newer settings observed while it was pending', async () => {
  let settings = querySettings(), complete;
  const ui = panel((url, options) => {
    if (options.method === 'POST') return new Promise(resolve => { complete = () => resolve([200, querySettings(2, JSON.parse(options.body).config)]); });
    return url.endsWith('/overview') ? [200, configuredOverview(settings)] : [200, accountPage(settings.view_revision)];
  });
  await ui.get('connect').onclick(); ui.get('weekly-query').checked = true;
  const pending = ui.get('weekly-query').onchange(); await flush();
  settings = querySettings(3, {...queryConfig(), interval_seconds: 180}); await ui.tick(5000);
  complete(); await pending;
  assert.equal(ui.get('weekly-query').checked, false); assert.equal(ui.get('weekly-query').disabled, true);
  assert.equal(ui.get('settings-interval').value, '180'); assert.equal(ui.calls.filter(call => call.method === 'POST').length, 1);
});
test('home switches can save browser-unsupported server timezone aliases without editing them', async () => {
  const config = {...queryConfig(), timezone: 'Factory'};
  const ui = panel((url, options) => options.method === 'POST' ? [200, querySettings(2, JSON.parse(options.body).config)] : url.endsWith('/overview') ? [200, configuredOverview(querySettings(1, config))] : [200, accountPage()]);
  await ui.get('connect').onclick(); ui.get('weekly-query').checked = true; await ui.get('weekly-query').onchange();
  assert.equal(ui.get('weekly-query').checked, true); assert.equal(ui.get('weekly-query').disabled, false);
  assert.deepEqual(JSON.parse(ui.calls.find(call => call.method === 'POST').body).config, {...config, on_weekly_expiry: true});
});
test('failed verification reads keep home switches locked and preserve the draft', async () => {
  const ui = panel((url, options) => {
    if (url.endsWith('/settings')) return options.method === 'POST' ? [409, {error: 'settings_conflict'}] : [503, {error: 'storage_unavailable'}];
    return url.endsWith('/overview') ? [200, configuredOverview()] : [200, accountPage()];
  });
  await ui.get('connect').onclick(); edit(ui, 'settings-interval', '90');
  ui.get('weekly-query').checked = true; await ui.get('weekly-query').onchange();
  await ui.get('rules-reload').onclick();
  assert.equal(ui.get('weekly-query').disabled, true); assert.equal(ui.get('rules-reload').hidden, false);
  assert.equal(ui.get('settings-interval').value, '90'); assert.match(ui.get('rules-message').textContent, /读取设置失败/);
  assert.equal(ui.calls.filter(call => call.method === 'POST').length, 1);
});
test('home save authentication failure disconnects instead of retrying writes', async () => {
  const ui = panel((url, options) => options.method === 'POST' ? [401, {error: 'unauthorized'}] : url.endsWith('/overview') ? [200, configuredOverview()] : [200, accountPage()]);
  await ui.get('connect').onclick(); ui.get('weekly-query').checked = true; await ui.get('weekly-query').onchange();
  assert.equal(ui.get('login-section').hidden, false); assert.equal(ui.get('weekly-query').disabled, true);
  assert.equal(ui.calls.filter(call => call.method === 'POST').length, 1);
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

const {modelDefaults, parseModelPhrases, exportModelPhrases, deduplicateModelPhrases, modelConfigDraft, validateModelDraft, modelPromptPreview} = require('../../../internal/v2/daemon/web/panel.js');
const modelConfig = overrides => {
  const {generation_prompt, ...legacyDefaults} = modelDefaults;
  return {...legacyDefaults, phrases: [], ...overrides};
};
const modelSettings = (revision = 1, config = modelConfig(), instance = 'a') => ({...view(instance, revision), revision, config});
const modelOverview = (instance = 'a', capable = true, canGenerate = true) => ({...overview(), ...view(instance, 20), capabilities: {quota_query: true, model_settings: capable, prompt_generation: canGenerate, model_execution: false}});
const submitModels = ui => ui.get('model-settings-form').onsubmit({preventDefault() {}});
const submitCandidates = ui => ui.get('generation-form').onsubmit({preventDefault() {}});
async function openModels(ui) {
  await ui.get('connect').onclick();
  ui.nav.find(node => node.dataset.page === 'models').listeners.get('click')(); await flush();
}
function configureCandidateRequest(ui) {
  ui.get('generation-account').value = 'codex-01'; ui.get('generation-model').value = 'actual-model';
  ui.get('generation-count').value = '3'; ui.get('generation-consent').checked = true;
  ui.get('generation-consent').onchange();
}
function modelResponse(url, settings = modelSettings(), instance = 'a') {
  if (url.endsWith('/model-settings')) return [200, settings];
  return url.endsWith('/overview') ? [200, modelOverview(instance)] : [200, {...accountPage(), ...view(instance)}];
}
function editablePromptResponse(url, settings = modelSettings(1, modelConfig({generation_prompt: modelDefaults.generation_prompt}))) {
  if (url.endsWith('/overview')) { const data = modelOverview(); data.capabilities.editable_generation_prompt = true; return [200, data]; }
  return modelResponse(url, settings);
}
test('background model reads preserve editable drafts while a slow poll is pending', async () => {
  let reads = 0, release;
  const ui = panel(url => url.endsWith('/model-settings') && ++reads > 1 ? new Promise(resolve => { release = () => resolve([200, modelSettings()]); }) : modelResponse(url));
  await openModels(ui); edit(ui, 'model-default', 'unsaved-model');
  const pending = ui.tick(5000); await flush();
  assert.equal(ui.get('model-fields').disabled, false);
  assert.equal(ui.get('model-default').value, 'unsaved-model');
  edit(ui, 'model-default-prompt', 'continue editing'); release(); await pending;
  assert.equal(ui.get('model-default-prompt').value, 'continue editing');
  assert.equal(ui.get('model-fields').disabled, false);
  assert.equal(ui.calls.some(call => call.method === 'POST'), false);
});
test('CPA model catalog uses exact account identity and never saves or sends a model request', async () => {
  const account = {id: 'codex/account-01', provider: 'codex', auth_index: 'bound-index', disabled: false};
  const ui = panel(url => url.endsWith('/auth-files') ? [200, {files: [account]}] : url.includes('/auth-files/models?') ? [200, {models: [{id: 'actual-model'}, {id: 'other-model'}, {id: 'actual-model'}]}] : modelResponse(url));
  await openModels(ui);
  assert.equal(ui.calls.some(call => call.url.includes('/auth-files')), false);
  ui.get('model-catalog-account').value = account.id; ui.get('model-catalog-account').oninput();
  await ui.get('model-catalog-refresh').onclick();
  assert.deepEqual(ui.get('wake-model-options').children.map(node => node.value), ['actual-model', 'other-model']);
  const reads = ui.calls.filter(call => call.url.includes('/auth-files'));
  assert.deepEqual(reads.map(call => call.url), ['/v0/management/auth-files', '/v0/management/auth-files/models?name=codex%2Faccount-01', '/v0/management/auth-files']);
  assert.equal(reads.every(call => call.headers.Authorization === 'Bearer fixture-key' && call.redirect === 'error' && call.credentials === 'omit'), true);
  assert.equal(ui.get('model-default').value, '');
  assert.equal(ui.calls.some(call => call.method === 'POST'), false);
});
test('CPA model catalog rejects changed identity and malformed model names without disabling manual inputs', async () => {
  for (const malformed of [false, true]) {
    let reads = 0;
    const ui = panel(url => url.endsWith('/auth-files') ? [200, {files: [{id: 'codex-01', provider: 'codex', auth_index: 'index-' + (malformed ? 1 : ++reads)}]}] : url.includes('/auth-files/models?') ? [200, {models: [{id: malformed ? 'bad model' : 'valid-model'}]}] : modelResponse(url));
    await openModels(ui); ui.get('model-catalog-account').value = 'codex-01';
    await ui.get('model-catalog-refresh').onclick();
    assert.equal(ui.get('wake-model-options').children.length, 0);
    assert.match(ui.get('model-catalog-status').textContent, /读取模型失败/);
    assert.equal(ui.get('model-fields').disabled, false);
    assert.equal(ui.calls.some(call => call.method === 'POST'), false);
  }
});
test('late model directory replies cannot populate a newly selected account', async () => {
  let release;
  const ui = panel(url => url.endsWith('/auth-files') ? [200, {files: [{id: 'old-account', provider: 'codex', auth_index: 'one'}]}] : url.includes('/auth-files/models?') ? new Promise(resolve => { release = () => resolve([200, {models: [{id: 'old-model'}]}]); }) : modelResponse(url));
  await openModels(ui); ui.get('model-catalog-account').value = 'old-account';
  const pending = ui.get('model-catalog-refresh').onclick(); await flush();
  ui.get('model-catalog-account').value = 'new-account'; ui.get('model-catalog-account').oninput();
  release(); await pending;
  assert.equal(ui.get('wake-model-options').children.length, 0);
  assert.equal(ui.get('model-catalog-refresh').disabled, false);
});
test('Codex style preset changes only prompt draft and never saves or generates automatically', async () => {
  const ui = panel(url => modelResponse(url)); await openModels(ui);
  assert.equal(ui.get('model-default-prompt').value, modelDefaults.default_prompt);
  ui.get('model-codex-preset').onclick();
  assert.match(ui.get('model-default-prompt').value, /Git.*commit.*push/);
  assert.equal(ui.get('model-save').disabled, false);
  assert.equal(ui.calls.some(call => call.method === 'POST'), false);
});
test('unchanged model polling does not dismiss a pending phrase clear confirmation', async () => {
  const ui = panel(url => modelResponse(url, modelSettings(1, modelConfig({phrases: ['保留的短句']}))));
  await openModels(ui); ui.get('model-clear').onclick();
  assert.equal(ui.get('model-confirmation').hidden, false);
  await ui.tick(5000);
  assert.equal(ui.get('model-confirmation').hidden, false);
  assert.equal(ui.get('model-phrases').value, '保留的短句');
  ui.get('model-cancel-action').onclick(); assert.equal(ui.get('model-confirmation').hidden, true);
  assert.equal(ui.calls.some(call => call.method === 'POST'), false);
});
test('generation prompt validation accepts legacy omissions and empty defaults but enforces bytes and controls', () => {
  const draft = modelConfigDraft(modelConfig());
  assert.equal(draft.generation_prompt, ''); assert.equal(validateModelDraft(draft).config.generation_prompt, '');
  for (const value of ['', ' \t\n\r', '你'.repeat(2730) + 'ab']) assert.equal(validateModelDraft({...draft, generation_prompt: value}).errors.generation_prompt, undefined);
  for (const value of ['你'.repeat(2731), 'x'.repeat(8193), 'bad\0', 'bad\u0085', null, 1]) assert.ok(validateModelDraft({...draft, generation_prompt: value}).errors.generation_prompt);
  const phrases = Array.from({length: 128}, (_, index) => String(index).padStart(3, '0') + 'x'.repeat(509)).join('\n');
  assert.ok(validateModelDraft({...draft, phrases, generation_prompt: 'x'.repeat(8192)}).errors.total);
});
test('editable generation prompt loads and persists only on explicit model configuration save, independently of wake prompts', async () => {
  let settings = modelSettings(1, modelConfig({generation_prompt: '已保存的生成提示词'}));
  const ui = panel((url, options) => {
    if (url.endsWith('/model-settings') && options.method === 'POST') { settings = modelSettings(2, JSON.parse(options.body).config); return [200, settings]; }
    return editablePromptResponse(url, settings);
  });
  await openModels(ui); assert.equal(ui.get('generation-prompt').value, '已保存的生成提示词');
  assert.equal(ui.get('generation-prompt').disabled, false);
  configureCandidateRequest(ui); edit(ui, 'generation-prompt', '  自定义候选\n\t精确保留空白。  ');
  assert.equal(ui.get('generation-consent').checked, false); assert.match(ui.get('model-diff').textContent, /候选生成提示词/);
  assert.equal(ui.calls.some(call => call.method === 'POST'), false);
  await ui.tick(5000); assert.equal(ui.get('generation-prompt').value, '  自定义候选\n\t精确保留空白。  ');
  ui.nav.find(node => node.dataset.page === 'overview').listeners.get('click')();
  ui.nav.find(node => node.dataset.page === 'models').listeners.get('click')(); await flush();
  assert.equal(ui.get('generation-prompt').value, '  自定义候选\n\t精确保留空白。  ');
  await submitModels(ui);
  const posts = ui.calls.filter(call => call.method === 'POST'); assert.equal(posts.length, 1); assert.ok(posts[0].url.endsWith('/model-settings'));
  assert.equal(settings.config.generation_prompt, '  自定义候选\n\t精确保留空白。  ');
  assert.equal(settings.config.default_prompt, modelDefaults.default_prompt); assert.equal(settings.config.five_hour_prompt, ''); assert.equal(settings.config.weekly_prompt, '');
  assert.equal(ui.get('model-save').disabled, true);
});
test('generation uses the exact unsaved prompt snapshot and verification never switches to later edits', async () => {
  let submissions = 0;
  const ui = panel((url, options) => {
    if (url.includes('/prompt-generations')) {
      if (++submissions === 1) throw new Error('lost acknowledgement');
      return [202, {instance_id: 'a', job_id: 'prompt-snapshot', phase: 'queued', candidates: []}];
    }
    return editablePromptResponse(url);
  });
  await openModels(ui); configureCandidateRequest(ui); edit(ui, 'generation-prompt', '  草稿候选\n\t不改唤醒。  ');
  assert.equal(ui.get('generation-consent').checked, false); await submitCandidates(ui); assert.equal(submissions, 0);
  ui.get('generation-consent').checked = true; ui.get('generation-consent').onchange(); await submitCandidates(ui);
  assert.equal(ui.get('generation-prompt').disabled, true); assert.equal(ui.get('generation-reset-prompt').disabled, true);
  ui.get('generation-prompt').value = '不应使用的新提示词'; ui.get('generation-reset-prompt').onclick();
  await ui.get('generation-verify').onclick();
  const posts = ui.calls.filter(call => call.method === 'POST'); assert.equal(posts.length, 2);
  assert.equal(posts[0].body, posts[1].body); assert.equal(posts[0].headers['Idempotency-Key'], posts[1].headers['Idempotency-Key']);
  assert.deepEqual(JSON.parse(posts[0].body), {account_id: 'codex-01', model: 'actual-model', count: 3, prompt: '  草稿候选\n\t不改唤醒。  '});
  assert.equal(ui.calls.some(call => call.url.endsWith('/model-settings') && call.method === 'POST'), false);
});
test('empty editable prompts omit the request field and legacy model configurations load as empty', async () => {
  const ui = panel((url, options) => url.includes('/prompt-generations') ? [202, {instance_id: 'a', job_id: 'empty-prompt', phase: 'queued', candidates: []}] : editablePromptResponse(url, modelSettings()));
  await openModels(ui); assert.equal(ui.get('generation-prompt').value, ''); assert.equal(ui.get('model-save').disabled, true);
  configureCandidateRequest(ui); await submitCandidates(ui);
  assert.deepEqual(JSON.parse(ui.calls.find(call => call.method === 'POST').body), {account_id: 'codex-01', model: 'actual-model', count: 3});
});
test('older backends disable generation prompt editing and never receive unsupported fields in either POST', async () => {
  let settings = modelSettings();
  const ui = panel((url, options) => {
    if (url.endsWith('/model-settings') && options.method === 'POST') { settings = modelSettings(2, JSON.parse(options.body).config); return [200, settings]; }
    if (url.includes('/prompt-generations')) return [202, {instance_id: 'a', job_id: 'legacy-prompt', phase: 'queued', candidates: []}];
    return modelResponse(url, settings);
  });
  await openModels(ui); assert.equal(ui.get('generation-prompt').disabled, true); assert.equal(ui.get('generation-reset-prompt').disabled, true);
  assert.match(ui.get('generation-prompt-help').textContent, /不支持/);
  edit(ui, 'generation-prompt', '不该发送'); ui.get('generation-reset-prompt').onclick();
  assert.equal(ui.get('model-save').disabled, true);
  edit(ui, 'model-default', 'legacy-model'); await submitModels(ui); configureCandidateRequest(ui); await submitCandidates(ui);
  const posts = ui.calls.filter(call => call.method === 'POST'); assert.equal(posts.length, 2);
  assert.equal(Object.hasOwn(JSON.parse(posts[0].body).config, 'generation_prompt'), false);
  assert.equal(Object.hasOwn(JSON.parse(posts[1].body), 'prompt'), false);
});
test('resetting generation prompt changes only a draft and cancels spend consent without any write', async () => {
  const ui = panel(url => editablePromptResponse(url, modelSettings(1, modelConfig({generation_prompt: '自定义'}))));
  await openModels(ui); configureCandidateRequest(ui); ui.get('generation-reset-prompt').onclick();
  assert.equal(ui.get('generation-prompt').value, modelDefaults.generation_prompt); assert.equal(ui.get('generation-consent').checked, false);
  assert.equal(ui.get('model-save').disabled, false); assert.equal(ui.calls.some(call => call.method === 'POST'), false);
  edit(ui, 'generation-prompt', ''); assert.match(ui.get('model-diff').textContent, /内置生成提示词/);
});
test('polling a changed saved generation prompt clears previous spend consent', async () => {
  let settings = modelSettings(1, modelConfig({generation_prompt: '第一版'}));
  const ui = panel(url => editablePromptResponse(url, settings)); await openModels(ui); configureCandidateRequest(ui);
  assert.equal(ui.get('generation-consent').checked, true);
  settings = modelSettings(2, modelConfig({generation_prompt: '第二版'})); await ui.tick(5000);
  assert.equal(ui.get('generation-prompt').value, '第二版'); assert.equal(ui.get('generation-consent').checked, false);
  assert.equal(ui.get('generation-submit').disabled, true); assert.equal(ui.calls.some(call => call.method === 'POST'), false);
});
test('invalid generation prompt blocks direct model and generation submission', async () => {
  const ui = panel(url => editablePromptResponse(url)); await openModels(ui); configureCandidateRequest(ui);
  for (const value of ['你'.repeat(2731), 'bad\0', 'bad\u0085']) {
    edit(ui, 'generation-prompt', value); ui.get('generation-consent').checked = true; ui.get('generation-consent').onchange();
    await submitModels(ui); await submitCandidates(ui);
    assert.equal(ui.calls.some(call => call.method === 'POST'), false); assert.match(ui.get('generation-message').textContent, /8192/);
  }
});
test('unknown generation locks prompt and reset without changing the snapshotted request or allowing resend', async () => {
  const ui = panel((url, options) => url.includes('/prompt-generations') ? [200, {instance_id: 'a', job_id: 'unknown-prompt', phase: 'unknown', candidates: []}] : editablePromptResponse(url));
  await openModels(ui); configureCandidateRequest(ui); await submitCandidates(ui);
  assert.equal(ui.get('generation-prompt').disabled, true); assert.equal(ui.get('generation-reset-prompt').disabled, true);
  ui.get('generation-reset-prompt').onclick(); await submitCandidates(ui); await ui.get('generation-verify').onclick();
  assert.equal(ui.calls.filter(call => call.method === 'POST').length, 1);
  assert.equal(ui.calls.filter(call => call.url.includes('/prompt-generations?job_id=')).length, 1);
});

test('model validation enforces UTF-8 bytes, controls, prompt strategies and phrase uniqueness', () => {
  const draft = modelConfigDraft(modelConfig());
  assert.deepEqual(validateModelDraft(draft).errors, {});
  for (const value of ['one two', 'model\n', 'x'.repeat(201), '你'.repeat(67), 'model\u0085']) assert.ok(validateModelDraft({...draft, default_model: value}).errors.default_model);
  for (const value of ['', ' \n ', '你'.repeat(2731), 'hello\x00', 'hello\u0085']) assert.ok(validateModelDraft({...draft, default_prompt: value}).errors.default_prompt);
  assert.deepEqual(validateModelDraft({...draft, default_prompt: '一行\n二行\t补充\r\n'}).errors, {});
  assert.ok(validateModelDraft({...draft, prompt_mode: 'other'}).errors.prompt_mode);
  assert.ok(validateModelDraft({...draft, prompt_mode: 'random'}).errors.phrases);
  for (const value of ['重复\n 重复 ', '你'.repeat(171), 'a\tb', 'a\u2028b', Array.from({length: 201}, (_, index) => '短句' + index).join('\n')]) assert.ok(validateModelDraft({...draft, phrases: value}).errors.phrases);
  assert.ok(validateModelDraft({...draft, phrases: Array.from({length: 200}, (_, index) => String(index) + 'x'.repeat(340)).join('\n')}).errors.total);
  assert.deepEqual(validateModelDraft({...draft, phrases: ' \n你好\r\n 再见 \n', prompt_mode: 'random'}).config.phrases, ['你好', '再见']);
});
test('model phrase import/export and deduplication are pure and preserve first occurrence order', () => {
  const phrases = parseModelPhrases(' 一句 \r\n\r另句\n一句\n');
  assert.deepEqual(phrases, ['一句', '另句', '一句']);
  assert.deepEqual(deduplicateModelPhrases(phrases), ['一句', '另句']);
  assert.deepEqual(phrases, ['一句', '另句', '一句']);
  assert.equal(exportModelPhrases(phrases), '一句\n另句\n一句\n');
  assert.equal(exportModelPhrases([]), '');
});
test('model preview resolves overrides and inheritance and random preview uses only local crypto', () => {
  const config = modelConfig({default_model: 'base', five_hour_prompt: '五小时专用', weekly_model: 'weekly-only'});
  assert.deepEqual(modelPromptPreview(config, 'five_hour'), {model: 'base', prompt: '五小时专用', modelInherited: true, promptInherited: false, mode: 'fixed'});
  const weekly = modelPromptPreview(config, 'weekly');
  assert.equal(weekly.model, 'weekly-only'); assert.equal(weekly.prompt, modelDefaults.default_prompt); assert.equal(weekly.promptInherited, true);
  const random = {...config, prompt_mode: 'random', phrases: ['第一句', '第二句']};
  assert.equal(modelPromptPreview(random, 'five_hour', {getRandomValues: bytes => bytes.fill(0)}).prompt, '第一句');
  assert.throws(() => modelPromptPreview(random, 'default', {}), /随机/);
  assert.throws(() => modelPromptPreview({...random, phrases: []}), /至少/);
});
test('model settings load only on explicit navigation and capabilities gate configuration and generation independently', async () => {
  const ui = panel(url => modelResponse(url)); await ui.get('connect').onclick(); await ui.tick(5000);
  assert.equal(ui.calls.some(call => call.url.includes('/model-settings')), false);
  ui.nav.find(node => node.dataset.page === 'models').listeners.get('click')(); await flush();
  assert.equal(ui.calls.filter(call => call.url.endsWith('/model-settings')).length, 1);
  assert.equal(ui.get('model-default-prompt').value, modelDefaults.default_prompt);
  assert.equal(ui.get('generation-fields').disabled, false); assert.equal(ui.get('auto-wake').disabled, true);
  assert.equal(ui.calls.some(call => call.method === 'POST'), false);
  const absent = panel(url => url.endsWith('/overview') ? [200, modelOverview('a', false, false)] : [200, accountPage()]); await openModels(absent);
  assert.equal(absent.calls.some(call => call.url.includes('/model-settings')), false);
  assert.equal(absent.get('model-fields').disabled, true); assert.equal(absent.get('generation-fields').disabled, true);
  await submitModels(absent); configureCandidateRequest(absent); await submitCandidates(absent);
  assert.equal(absent.calls.some(call => call.method === 'POST'), false);
});
test('model settings poll only while visible, update clean drafts and preserve dirty drafts on conflict', async () => {
  let settings = modelSettings();
  const ui = panel(url => modelResponse(url, settings)); await openModels(ui);
  settings = modelSettings(2, modelConfig({default_model: 'new'})); await ui.tick(5000);
  assert.equal(ui.get('model-default').value, 'new');
  edit(ui, 'model-default', 'local-draft'); settings = modelSettings(3, modelConfig({default_model: 'remote'})); await ui.tick(5000);
  assert.equal(ui.get('model-default').value, 'local-draft'); assert.equal(ui.get('model-conflict').hidden, false);
  assert.equal(ui.get('model-save').disabled, true); await submitModels(ui);
  assert.equal(ui.calls.some(call => call.method === 'POST'), false);
  ui.nav.find(node => node.dataset.page === 'overview').listeners.get('click')(); const reads = ui.calls.length; await ui.tick(5000);
  assert.equal(ui.calls.slice(reads).some(call => call.url.endsWith('/model-settings')), false);
});
test('model saves use independent revision/config and never submit query settings, jobs or generation', async () => {
  let settings = modelSettings();
  const ui = panel((url, options) => {
    if (url.endsWith('/model-settings') && options.method === 'POST') { settings = modelSettings(2, JSON.parse(options.body).config); return [200, settings]; }
    return modelResponse(url, settings);
  });
  await openModels(ui); edit(ui, 'model-default', 'actual-model'); edit(ui, 'model-five-hour-prompt', '早上好');
  edit(ui, 'model-phrases', ' 你好 \n再见\n'); edit(ui, 'model-prompt-mode', 'random');
  assert.match(ui.get('model-diff').textContent, /actual-model/); await submitModels(ui);
  const writes = ui.calls.filter(call => call.method === 'POST'); assert.equal(writes.length, 1);
  assert.ok(writes[0].url.endsWith('/model-settings'));
  assert.deepEqual(JSON.parse(writes[0].body), {revision: 1, config: modelConfig({default_model: 'actual-model', five_hour_prompt: '早上好', prompt_mode: 'random', phrases: ['你好', '再见']})});
  assert.equal(ui.get('model-save').disabled, true); assert.match(ui.get('model-message').textContent, /配置已保存/);
  assert.equal(ui.get('auto-wake').disabled, true);
});
test('model no-op normalized saves accept unchanged revisions without inventing a conflict', async () => {
  const settings = modelSettings(2, modelConfig({phrases: ['你好']}));
  const ui = panel(url => modelResponse(url, settings)); await openModels(ui);
  edit(ui, 'model-phrases', ' 你好 \n'); await submitModels(ui);
  assert.equal(ui.calls.filter(call => call.method === 'POST').length, 1);
  assert.equal(ui.get('model-conflict').hidden, true); assert.equal(ui.get('model-phrases').value, '你好');
});
test('model imports and local previews do not write, while destructive edits require explicit confirmation', async () => {
  const ui = panel(url => modelResponse(url), {crypto: {getRandomValues: bytes => bytes.fill(0)}}); await openModels(ui);
  ui.get('model-transfer').value = ' 一句\r\n另句\n一句'; ui.get('model-import').onclick();
  assert.equal(ui.get('model-phrases').value, '一句\n另句\n一句'); assert.equal(ui.get('model-validation').hidden, false);
  ui.get('model-deduplicate').onclick(); assert.equal(ui.get('model-phrases').value, '一句\n另句\n一句');
  assert.equal(ui.get('model-confirmation').hidden, false); ui.get('model-confirm-action').onclick();
  assert.equal(ui.get('model-phrases').value, '一句\n另句');
  ui.get('model-export').onclick(); assert.equal(ui.get('model-transfer').value, '一句\n另句\n');
  edit(ui, 'model-default', 'actual-model'); edit(ui, 'model-prompt-mode', 'random');
  ui.get('model-preview-rule').value = 'weekly'; ui.get('model-preview').onclick();
  assert.match(ui.get('model-preview-result').textContent, /继承默认模型/); assert.match(ui.get('model-preview-result').textContent, /一句/);
  ui.get('model-clear').onclick(); ui.get('model-cancel-action').onclick(); assert.equal(ui.get('model-phrases').value, '一句\n另句');
  ui.get('model-clear').onclick(); ui.get('model-confirm-action').onclick(); assert.equal(ui.get('model-phrases').value, '');
  assert.equal(ui.calls.some(call => call.method === 'POST'), false);
});
test('model form validation blocks direct invalid submission and unsupported random preview is text-only', async () => {
  const ui = panel(url => modelResponse(url)); await openModels(ui);
  edit(ui, 'model-default', 'bad model'); edit(ui, 'model-default-prompt', ''); await submitModels(ui);
  assert.equal(ui.get('model-default').attributes.get('aria-invalid'), 'true'); assert.equal(ui.get('model-save').disabled, true);
  edit(ui, 'model-default', 'good'); edit(ui, 'model-default-prompt', '<script>alert(1)</script>');
  edit(ui, 'model-prompt-mode', 'random'); edit(ui, 'model-phrases', '<img src=x onerror=alert(1)>'); ui.get('model-preview').onclick();
  assert.match(ui.get('model-preview-result').textContent, /随机/);
  assert.equal(ui.get('model-preview-result').children.length, 0); assert.equal(ui.calls.some(call => call.method === 'POST'), false);
});
test('model conflict and lost acknowledgement preserve drafts until explicit verification without automatic retry', async () => {
  for (const lost of [false, true]) {
    let settings = modelSettings();
    const ui = panel((url, options) => {
      if (url.endsWith('/model-settings') && options.method === 'POST') {
        settings = modelSettings(2, modelConfig({default_model: 'server'}));
        if (lost) throw new Error('lost acknowledgement');
        return [409, {error: 'model_settings_conflict'}];
      }
      return modelResponse(url, settings);
    });
    await openModels(ui); edit(ui, 'model-default', 'draft'); await submitModels(ui);
    assert.equal(ui.get('model-default').value, 'draft'); assert.equal(ui.get('model-save').disabled, true);
    await ui.tick(5000); await submitModels(ui); assert.equal(ui.calls.filter(call => call.method === 'POST').length, 1);
    await ui.get('model-reload').onclick(); assert.equal(ui.get('model-default').value, 'server');
    assert.equal(ui.get('model-conflict').hidden, true); assert.equal(ui.calls.filter(call => call.method === 'POST').length, 1);
  }
});
test('model storage failures remain editable and failed verification cannot unlock an ambiguous save', async () => {
  for (const code of ['invalid_model_settings', 'storage_unavailable']) {
    const ui = panel((url, options) => options.method === 'POST' ? [code === 'storage_unavailable' ? 503 : 400, {error: code}] : modelResponse(url));
    await openModels(ui); edit(ui, 'model-default', 'draft'); await submitModels(ui);
    assert.equal(ui.get('model-default').value, 'draft'); assert.equal(ui.get('model-save').disabled, false);
  }
  let failRead = false;
  const ui = panel((url, options) => {
    if (url.endsWith('/model-settings') && (failRead || options.method === 'POST')) { failRead = true; return [409, {error: 'model_settings_conflict'}]; }
    return modelResponse(url);
  });
  await openModels(ui); edit(ui, 'model-default', 'draft'); await submitModels(ui); await ui.get('model-reload').onclick();
  assert.equal(ui.get('model-save').disabled, true); assert.equal(ui.get('model-default').value, 'draft');
});
test('model restart and delayed responses cannot overwrite dirty drafts or unlock old-instance saves', async () => {
  let instance = 'a', settings = modelSettings(), release;
  const ui = panel((url, options) => {
    if (url.endsWith('/model-settings') && options.method === 'POST') return new Promise(resolve => { release = () => resolve([200, modelSettings(2, JSON.parse(options.body).config)]); });
    return modelResponse(url, settings, instance);
  });
  await openModels(ui); edit(ui, 'model-default', 'draft'); const saving = submitModels(ui); await flush();
  instance = 'b'; settings = modelSettings(0, modelConfig(), instance); await ui.tick(5000); release(); await saving;
  assert.equal(ui.get('model-default').value, 'draft'); assert.equal(ui.get('model-save').disabled, true);
  await submitModels(ui); assert.equal(ui.calls.filter(call => call.method === 'POST').length, 1);
  await ui.get('model-reload').onclick(); assert.equal(ui.get('model-default').value, '');
});
test('candidate generation requires capability, valid fields and explicit quota consent', async () => {
  const ui = panel(url => modelResponse(url)); await openModels(ui); await submitCandidates(ui);
  assert.equal(ui.calls.some(call => call.method === 'POST'), false);
  configureCandidateRequest(ui); ui.get('generation-count').value = '21'; await submitCandidates(ui);
  ui.get('generation-count').value = '2'; ui.get('generation-model').value = 'bad model'; await submitCandidates(ui);
  assert.equal(ui.calls.some(call => call.method === 'POST'), false); assert.match(ui.get('generation-message').textContent, /有效/);
  ui.get('model-default').value = 'real-default'; ui.get('generation-use-default').onclick(); assert.equal(ui.get('generation-model').value, 'real-default');
});
const generationDirectory = (instance = 'a', accounts = [
  {id: 'codex-01', provider: 'codex', status: 'available'},
  {id: 'antigravity-01', provider: 'antigravity', status: 'available'},
  {id: 'antigravity-disabled', provider: 'antigravity', status: 'disabled'}
]) => ({instance_id: instance, accounts});
const directoryOverview = (instance = 'a') => ({...modelOverview(instance), capabilities: {...modelOverview(instance).capabilities, generation_accounts: true, prompt_generation_providers: ['codex', 'antigravity']}});
function directoryResponse(url, directory = generationDirectory(), instance = 'a') {
  if (url.endsWith('/generation-accounts')) return [200, directory];
  if (url.endsWith('/overview')) return [200, directoryOverview(instance)];
  return modelResponse(url, modelSettings(1, modelConfig(), instance), instance);
}
test('generation account directory loads only on model navigation and explicit refresh without writes or quota changes', async () => {
  const ui = panel(url => directoryResponse(url));
  await ui.get('connect').onclick(); await ui.tick(5000);
  assert.equal(ui.calls.some(call => call.url.endsWith('/generation-accounts')), false);
  ui.nav.find(node => node.dataset.page === 'models').listeners.get('click')(); await flush();
  assert.equal(ui.calls.filter(call => call.url.endsWith('/generation-accounts')).length, 1);
  assert.equal(ui.get('generation-account').value, '');
  assert.equal(ui.get('metric-total').textContent, '24');
  assert.match(ui.get('generation-account-options').textContent, /Codex.*反重力.*已禁用/);
  await ui.tick(5000);
  ui.nav.find(node => node.dataset.page === 'overview').listeners.get('click')();
  ui.nav.find(node => node.dataset.page === 'models').listeners.get('click')(); await flush();
  assert.equal(ui.calls.filter(call => call.url.endsWith('/generation-accounts')).length, 1);
  await ui.get('generation-refresh-accounts').onclick();
  assert.equal(ui.calls.filter(call => call.url.endsWith('/generation-accounts')).length, 2);
  assert.equal(ui.calls.some(call => call.method === 'POST'), false);
});
test('Antigravity model suggestions use its own CPA account and never populate Codex wake choices', async () => {
  const ui = panel(url => url.endsWith('/auth-files') ? [200, {files: [{id: 'antigravity-01', provider: 'antigravity', auth_index: 'antig-index'}]}] : url.includes('/auth-files/models?') ? [200, {models: [{id: 'antigravity-only-model'}]}] : directoryResponse(url));
  await openModels(ui); ui.get('generation-account').value = 'antigravity-01'; ui.get('generation-account').oninput();
  await ui.get('generation-refresh-models').onclick();
  assert.deepEqual(ui.get('generation-model-options').children.map(node => node.value), ['antigravity-only-model']);
  assert.equal(ui.get('generation-model').value, '');
  assert.equal(ui.get('wake-model-options').children.length, 0);
  assert.equal(ui.calls.some(call => call.method === 'POST'), false);
  ui.get('generation-account').value = 'codex-01'; ui.get('generation-account').oninput();
  assert.equal(ui.get('generation-model-options').children.length, 0);
});
test('Antigravity generation uses its exact selected account and model without a client provider or automatic Codex default', async () => {
  const ui = panel((url, options) => url.includes('/prompt-generations') ? [202, {instance_id: 'a', job_id: 'antig-job', phase: 'queued', candidates: []}] : directoryResponse(url));
  await openModels(ui); configureCandidateRequest(ui); ui.get('generation-account').oninput();
  ui.get('generation-model').value = 'codex-default';
  ui.get('generation-account').value = 'antigravity-01'; ui.get('generation-account').oninput();
  assert.equal(ui.get('generation-model').value, ''); assert.equal(ui.get('generation-consent').checked, false);
  assert.match(ui.get('generation-account-help').textContent, /反重力/);
  ui.get('model-default').value = 'explicit-default'; ui.get('generation-use-default').onclick();
  assert.equal(ui.get('generation-model').value, 'explicit-default'); assert.match(ui.get('generation-message').textContent, /确认.*提供方/);
  ui.get('generation-model').value = 'antigravity-fixture-model'; ui.get('generation-consent').checked = true; ui.get('generation-consent').onchange();
  assert.match(ui.get('generation-consent-label').textContent, /反重力.*额度/);
  await submitCandidates(ui); await submitCandidates(ui);
  const posts = ui.calls.filter(call => call.method === 'POST'); assert.equal(posts.length, 1);
  assert.deepEqual(JSON.parse(posts[0].body), {account_id: 'antigravity-01', model: 'antigravity-fixture-model', count: 3});
});
test('unavailable, missing and replaced generation accounts never fall back or silently permit submission', async () => {
  let directory = generationDirectory();
  const ui = panel(url => directoryResponse(url, directory)); await openModels(ui); configureCandidateRequest(ui);
  ui.get('generation-account').value = 'antigravity-disabled'; ui.get('generation-account').oninput();
  ui.get('generation-model').value = 'model'; ui.get('generation-consent').checked = true; ui.get('generation-consent').onchange();
  assert.equal(ui.get('generation-submit').disabled, true); assert.match(ui.get('generation-account-help').textContent, /已禁用/); await submitCandidates(ui);
  ui.get('generation-account').value = 'antigravity-01'; ui.get('generation-account').oninput();
  ui.get('generation-model').value = 'antig-model';
  for (const status of ['unavailable', 'binding_unavailable']) {
    directory = generationDirectory('a', [{id: 'antigravity-01', provider: 'antigravity', status}]);
    await ui.get('generation-refresh-accounts').onclick();
    assert.equal(ui.get('generation-account').value, 'antigravity-01'); assert.equal(ui.get('generation-submit').disabled, true);
  }
  directory = generationDirectory('a', []); await ui.get('generation-refresh-accounts').onclick();
  assert.match(ui.get('generation-account-help').textContent, /已移除/);
  directory = generationDirectory('a', [{id: 'antigravity-01', provider: 'codex', status: 'available'}]);
  await ui.get('generation-refresh-accounts').onclick(); assert.match(ui.get('generation-account-help').textContent, /提供方已变化/);
  ui.get('generation-consent').checked = true; await submitCandidates(ui);
  assert.equal(ui.calls.some(call => call.method === 'POST'), false);
  ui.get('generation-account').oninput(); assert.equal(ui.get('generation-model').value, '');
  assert.match(ui.get('generation-account-help').textContent, /Codex/);
});
test('Antigravity catalog entries remain blocked when the backend has not advertised that generation provider', async () => {
  const ui = panel(url => {
    if (url.endsWith('/overview')) { const data = directoryOverview(); delete data.capabilities.prompt_generation_providers; return [200, data]; }
    return directoryResponse(url);
  });
  await openModels(ui); configureCandidateRequest(ui); ui.get('generation-account').value = 'antigravity-01'; ui.get('generation-account').oninput();
  ui.get('generation-model').value = 'model'; ui.get('generation-consent').checked = true; ui.get('generation-consent').onchange();
  assert.equal(ui.get('generation-submit').disabled, true); assert.match(ui.get('generation-account-help').textContent, /尚不支持/); await submitCandidates(ui);
  assert.equal(ui.calls.some(call => call.method === 'POST'), false);
});
test('explicit provider capabilities also gate Codex entries and legacy manual Codex input', async () => {
  for (const catalog of [true, false]) {
    const ui = panel(url => {
      if (url.endsWith('/overview')) { const data = directoryOverview(); data.capabilities.generation_accounts = catalog; data.capabilities.prompt_generation_providers = ['antigravity']; return [200, data]; }
      return directoryResponse(url);
    });
    await openModels(ui); configureCandidateRequest(ui);
    assert.equal(ui.get('generation-submit').disabled, true); assert.match(ui.get('generation-account-help').textContent, /未声明支持|尚不支持/);
    await submitCandidates(ui); assert.equal(ui.calls.some(call => call.method === 'POST'), false);
  }
});
test('generation directory unavailable errors have a readable message and never enable manual fallback', async () => {
  const ui = panel(url => url.endsWith('/generation-accounts') ? [503, {error: 'generation_accounts_unavailable'}] : directoryResponse(url));
  await openModels(ui); configureCandidateRequest(ui);
  assert.match(ui.get('generation-directory-status').textContent, /生成账号目录暂不可用/);
  assert.equal(ui.get('generation-submit').disabled, true); await submitCandidates(ui);
  assert.equal(ui.calls.some(call => call.method === 'POST'), false);
});
test('directory loading, read failure and stale instances block new generation without automatic retry', async () => {
  let resolveCatalog;
  const ui = panel(url => url.endsWith('/generation-accounts') ? new Promise(resolve => { resolveCatalog = resolve; }) : directoryResponse(url));
  await openModels(ui); configureCandidateRequest(ui);
  assert.equal(ui.get('generation-refresh-accounts').disabled, true); assert.equal(ui.get('generation-submit').disabled, true);
  await submitCandidates(ui); assert.equal(ui.calls.some(call => call.method === 'POST'), false);
  resolveCatalog([200, generationDirectory('stale-instance')]); await flush();
  assert.match(ui.get('generation-directory-status').textContent, /失败/); assert.equal(ui.get('generation-submit').disabled, true);
  await ui.tick(5000); assert.equal(ui.calls.filter(call => call.url.endsWith('/generation-accounts')).length, 1);
  const retry = ui.get('generation-refresh-accounts').onclick(); resolveCatalog([503, {error: 'storage_unavailable'}]); await retry;
  assert.equal(ui.get('generation-submit').disabled, true); assert.equal(ui.calls.some(call => call.method === 'POST'), false);
  const success = ui.get('generation-refresh-accounts').onclick(); resolveCatalog([200, generationDirectory()]); await success;
  assert.equal(ui.get('generation-submit').disabled, false);
});
test('late catalog responses after disconnect cannot expose options or enable generation', async () => {
  let resolveCatalog;
  const ui = panel(url => url.endsWith('/generation-accounts') ? new Promise(resolve => { resolveCatalog = resolve; }) : directoryResponse(url));
  await openModels(ui); ui.get('disconnect').onclick();
  resolveCatalog([200, generationDirectory()]); await flush();
  assert.equal(ui.get('generation-account-options').children.length, 0);
  assert.equal(ui.get('generation-submit').disabled, true); assert.equal(ui.get('generation-refresh-accounts').disabled, true);
});
test('invalid directory payloads are not rendered or accepted as an implicit manual-account fallback', async () => {
  const invalid = [
    {instance_id: 'a', accounts: null},
    generationDirectory('a', [{id: 'account', provider: 'other', status: 'available'}]),
    generationDirectory('a', [{id: 'account', provider: 'codex', status: 'other'}]),
    generationDirectory('a', [{id: 'account\n', provider: 'codex', status: 'available'}]),
    generationDirectory('a', [{id: 'same', provider: 'codex', status: 'available'}, {id: 'same', provider: 'antigravity', status: 'available'}])
  ];
  for (const directory of invalid) {
    const ui = panel(url => directoryResponse(url, directory)); await openModels(ui); configureCandidateRequest(ui);
    assert.equal(ui.get('generation-account-options').children.length, 0); assert.equal(ui.get('generation-submit').disabled, true);
    assert.match(ui.get('generation-directory-status').textContent, /失败/); await submitCandidates(ui);
    assert.equal(ui.calls.some(call => call.method === 'POST'), false);
  }
});
test('typing through partial account IDs still clears a model when changing the selected provider', async () => {
  const ui = panel(url => directoryResponse(url)); await openModels(ui);
  ui.get('generation-account').value = 'antigravity-01'; ui.get('generation-account').oninput();
  ui.get('generation-model').value = 'antig-model';
  ui.get('generation-account').value = 'codex-'; ui.get('generation-account').oninput();
  ui.get('generation-account').value = 'codex-01'; ui.get('generation-account').oninput();
  assert.equal(ui.get('generation-model').value, ''); assert.match(ui.get('generation-account-help').textContent, /Codex/);
});
test('catalog responses from an earlier backend cannot override a newly observed instance', async () => {
  let instance = 'a', resolveCatalog;
  const ui = panel(url => url.endsWith('/generation-accounts') ? new Promise(resolve => { resolveCatalog = resolve; }) : directoryResponse(url, generationDirectory(instance), instance));
  await openModels(ui); instance = 'b'; await ui.tick(5000);
  resolveCatalog([200, generationDirectory('a')]); await flush();
  assert.equal(ui.get('generation-account-options').children.length, 0); assert.equal(ui.get('generation-submit').disabled, true);
  await ui.tick(5000); resolveCatalog([200, generationDirectory('b')]); await flush();
  assert.equal(ui.get('generation-account-options').children.length, 3);
  assert.equal(ui.calls.some(call => call.method === 'POST'), false);
});
test('catalog refresh never clears unknown generation state, unlocks fields or creates another request key', async () => {
  const ui = panel((url, options) => url.includes('/prompt-generations') ? [200, {instance_id: 'a', job_id: 'unknown-antig', phase: 'unknown', candidates: []}] : directoryResponse(url));
  await openModels(ui); configureCandidateRequest(ui);
  await submitCandidates(ui); assert.equal(ui.get('generation-fields').disabled, true);
  await ui.get('generation-refresh-accounts').onclick();
  assert.equal(ui.get('generation-fields').disabled, true); assert.equal(ui.get('generation-submit').disabled, true);
  assert.match(ui.get('generation-message').textContent, /未知/);
  ui.get('generation-account').value = 'antigravity-01'; ui.get('generation-account').oninput();
  assert.equal(ui.get('generation-fields').disabled, true); await submitCandidates(ui); await ui.get('generation-verify').onclick();
  assert.equal(ui.calls.filter(call => call.method === 'POST').length, 1);
  assert.equal(ui.calls.filter(call => call.url.includes('/prompt-generations?job_id=')).length, 1);
});
test('catalog refresh after lost generation acknowledgement preserves the original verification key and body', async () => {
  let submissions = 0;
  const ui = panel((url, options) => {
    if (url.includes('/prompt-generations')) {
      if (++submissions === 1) throw new Error('lost acknowledgement');
      return [202, {instance_id: 'a', job_id: 'verified-original', phase: 'queued', candidates: []}];
    }
    return directoryResponse(url);
  });
  await openModels(ui); configureCandidateRequest(ui); await submitCandidates(ui);
  await ui.get('generation-refresh-accounts').onclick();
  ui.get('generation-account').value = 'antigravity-01'; ui.get('generation-model').value = 'changed';
  await ui.get('generation-verify').onclick();
  const posts = ui.calls.filter(call => call.method === 'POST'); assert.equal(posts.length, 2);
  assert.equal(posts[0].body, posts[1].body); assert.equal(posts[0].headers['Idempotency-Key'], posts[1].headers['Idempotency-Key']);
});
test('legacy generation keeps manual Codex entry and never fetches an unsupported directory', async () => {
  const ui = panel(url => modelResponse(url)); await openModels(ui); configureCandidateRequest(ui);
  assert.match(ui.get('generation-account-help').textContent, /旧版.*Codex/);
  assert.equal(ui.get('generation-refresh-accounts').disabled, true);
  await ui.get('generation-refresh-accounts').onclick(); assert.equal(ui.calls.some(call => call.url.endsWith('/generation-accounts')), false);
});
test('candidate generation submits exactly once, polls reads and stages only explicitly selected candidates', async () => {
  let phase = 'queued';
  const ui = panel((url, options) => {
    if (url.includes('/prompt-generations')) return [options.method === 'POST' ? 202 : 200, {...view(), job_id: 'gen-one', phase, candidates: phase === 'completed' ? ['你好', '<img src=x onerror=alert(1)>', '再见'] : []}];
    return modelResponse(url);
  });
  await openModels(ui); configureCandidateRequest(ui); await submitCandidates(ui);
  assert.equal(ui.get('generation-fields').disabled, true); await submitCandidates(ui);
  phase = 'running'; await ui.tick(5000); phase = 'completed'; await ui.tick(5000);
  const posts = ui.calls.filter(call => call.method === 'POST'); assert.equal(posts.length, 1);
  assert.deepEqual(JSON.parse(posts[0].body), {account_id: 'codex-01', model: 'actual-model', count: 3});
  assert.equal(posts[0].headers['Idempotency-Key'], 'fixture-idempotency-key');
  assert.equal(ui.get('generation-results').hidden, false); assert.equal(ui.get('model-phrases').value, '');
  const first = ui.get('generation-candidates').children[1].children[0]; first.checked = true; first.onchange(); ui.get('generation-apply').onclick();
  assert.equal(ui.get('model-phrases').value, '<img src=x onerror=alert(1)>'); assert.equal(ui.get('model-save').disabled, false);
  assert.match(ui.get('generation-message').textContent, /尚未保存/); assert.equal(ui.calls.filter(call => call.method === 'POST').length, 1);
  assert.equal(ui.get('generation-candidates').children[1].children[1].children.length, 0);
});
test('candidate all-select and navigation preserve results without implicitly applying or saving them', async () => {
  const ui = panel((url, options) => url.includes('/prompt-generations') ? [200, {...view(), job_id: 'gen-two', phase: 'completed', candidates: ['第一句', '第二句']}] : modelResponse(url));
  await openModels(ui); configureCandidateRequest(ui); await submitCandidates(ui);
  ui.nav.find(node => node.dataset.page === 'overview').listeners.get('click')(); ui.nav.find(node => node.dataset.page === 'models').listeners.get('click')(); await flush();
  assert.equal(ui.get('generation-results').hidden, false); assert.equal(ui.get('model-phrases').value, '');
  ui.get('generation-select-all').onclick(); ui.get('generation-apply').onclick();
  assert.equal(ui.get('model-phrases').value, '第一句\n第二句'); assert.equal(ui.calls.filter(call => call.method === 'POST').length, 1);
});
test('candidate checkboxes keep their nodes and keyboard-toggle handlers through repeated renders', async () => {
  const ui = panel(url => url.includes('/prompt-generations') ? [200, {...view(), job_id: 'stable-candidates', phase: 'completed', candidates: ['第一句', '第二句']}] : modelResponse(url));
  await openModels(ui); configureCandidateRequest(ui); await submitCandidates(ui);
  const label = ui.get('generation-candidates').children[0], checkbox = label.children[0];
  checkbox.checked = true; checkbox.onchange();
  assert.equal(ui.get('generation-candidates').children[0], label);
  assert.equal(label.children[0], checkbox);
  await ui.tick(5000);
  assert.equal(label.children[0], checkbox); assert.equal(checkbox.checked, true);
  checkbox.checked = false; checkbox.onchange();
  assert.equal(ui.get('generation-apply').disabled, true);
  checkbox.checked = true; checkbox.onchange(); ui.get('generation-apply').onclick();
  assert.equal(ui.get('model-phrases').value, '第一句');
  assert.equal(ui.calls.filter(call => call.method === 'POST').length, 1);
});
test('lost generation acknowledgement never automatically retries and explicit verification keeps the exact key and body', async () => {
  let posts = 0;
  const ui = panel((url, options) => {
    if (url.includes('/prompt-generations')) {
      if (++posts === 1) throw new Error('lost acknowledgement');
      return [202, {...view(), job_id: 'gen-original', phase: 'queued', candidates: []}];
    }
    return modelResponse(url);
  });
  await openModels(ui); configureCandidateRequest(ui); await submitCandidates(ui); await ui.tick(5000);
  assert.equal(posts, 1); assert.equal(ui.get('generation-verify').hidden, false); assert.equal(ui.get('generation-submit').disabled, true);
  ui.get('generation-account').value = 'different-account'; ui.get('generation-model').value = 'different-model'; await submitCandidates(ui);
  assert.equal(posts, 1); await ui.get('generation-verify').onclick(); assert.equal(posts, 2);
  const requests = ui.calls.filter(call => call.method === 'POST');
  assert.equal(requests[0].body, requests[1].body); assert.equal(requests[0].headers['Idempotency-Key'], requests[1].headers['Idempotency-Key']);
  assert.match(ui.get('generation-message').textContent, /任务已接受/);
});
test('unknown generation results can only be read and never resent or replaced by a new request', async () => {
  const ui = panel((url, options) => url.includes('/prompt-generations') ? [200, {...view(), job_id: 'gen-unknown', phase: 'unknown', candidates: []}] : modelResponse(url));
  await openModels(ui); configureCandidateRequest(ui); await submitCandidates(ui);
  assert.equal(ui.get('generation-submit').disabled, true); assert.match(ui.get('generation-message').textContent, /可能已消耗/);
  await submitCandidates(ui); await ui.get('generation-verify').onclick(); await ui.tick(5000);
  assert.equal(ui.calls.filter(call => call.method === 'POST').length, 1);
  assert.equal(ui.calls.filter(call => call.url.includes('/prompt-generations?job_id=')).length, 1);
});
test('generation task read failures retry only GET and unavailable or rejected backends do not fabricate candidates', async () => {
  let polls = 0;
  const ui = panel((url, options) => {
    if (url.includes('/prompt-generations')) {
      if (options.method === 'POST') return [202, {...view(), job_id: 'gen-retry', phase: 'queued', candidates: []}];
      if (++polls === 1) throw new Error('temporary read error');
      return [200, {...view(), job_id: 'gen-retry', phase: 'failed', error_code: 'quota_authentication_failed', candidates: []}];
    }
    return modelResponse(url);
  });
  await openModels(ui); configureCandidateRequest(ui); await submitCandidates(ui); await ui.tick(5000);
  assert.match(ui.get('generation-message').textContent, /仅读取|读取生成任务失败/); await ui.tick(5000);
  assert.equal(ui.get('generation-results').hidden, true); assert.equal(ui.get('generation-consent').checked, false);
  assert.equal(ui.calls.filter(call => call.method === 'POST').length, 1);
  const rejected = panel((url, options) => url.includes('/prompt-generations') ? [503, {error: 'prompt_generation_unavailable'}] : modelResponse(url));
  await openModels(rejected); configureCandidateRequest(rejected); await submitCandidates(rejected);
  assert.equal(rejected.get('generation-results').hidden, true); assert.match(rejected.get('generation-message').textContent, /未提供/);
  assert.equal(rejected.calls.filter(call => call.method === 'POST').length, 1);
});
test('a backend restart prevents uncertain generation intents from being posted to a new instance', async () => {
  let instance = 'a';
  const ui = panel((url, options) => {
    if (url.includes('/prompt-generations')) throw new Error('unknown acceptance');
    return modelResponse(url, modelSettings(1, modelConfig(), instance), instance);
  });
  await openModels(ui); configureCandidateRequest(ui); await submitCandidates(ui); instance = 'b'; await ui.tick(5000);
  assert.equal(ui.get('generation-verify').disabled, true); assert.match(ui.get('generation-message').textContent, /实例已更换/);
  await ui.get('generation-verify').onclick(); await submitCandidates(ui);
  assert.equal(ui.calls.filter(call => call.method === 'POST').length, 1);
});
test('failed verification and storage errors cannot release an uncertain generation for a fresh request', async () => {
  let attempts = 0;
  const ui = panel((url, options) => {
    if (url.includes('/prompt-generations')) {
      attempts++;
      return attempts === 1 ? [503, {error: 'storage_unavailable'}] : [409, {error: 'idempotency_conflict'}];
    }
    return modelResponse(url);
  });
  await openModels(ui); configureCandidateRequest(ui); await submitCandidates(ui);
  assert.equal(ui.get('generation-submit').disabled, true); await ui.get('generation-verify').onclick();
  assert.equal(ui.get('generation-submit').disabled, true); assert.equal(ui.get('generation-verify').hidden, false);
  configureCandidateRequest(ui); await submitCandidates(ui); assert.equal(attempts, 2);
  const writes = ui.calls.filter(call => call.method === 'POST'); assert.equal(writes[0].body, writes[1].body);
  assert.equal(writes[0].headers['Idempotency-Key'], writes[1].headers['Idempotency-Key']);
});
test('missing generation jobs become read-only unknown results and cannot be regenerated automatically', async () => {
  const ui = panel((url, options) => url.includes('/prompt-generations') ? options.method === 'POST' ? [202, {...view(), job_id: 'gone', phase: 'queued', candidates: []}] : [404, {error: 'not_found'}] : modelResponse(url));
  await openModels(ui); configureCandidateRequest(ui); await submitCandidates(ui); await ui.tick(5000);
  assert.match(ui.get('generation-message').textContent, /可能已消耗/); assert.equal(ui.get('generation-submit').disabled, true);
  const reads = ui.calls.filter(call => call.url.includes('/prompt-generations?')).length; await ui.tick(5000);
  assert.equal(ui.calls.filter(call => call.url.includes('/prompt-generations?')).length, reads);
  await ui.get('generation-verify').onclick(); assert.equal(ui.calls.filter(call => call.method === 'POST').length, 1);
});
test('disconnecting an in-flight model save requires explicit verification after reconnect', async () => {
  let release;
  const ui = panel((url, options) => {
    if (url.endsWith('/model-settings') && options.method === 'POST') return new Promise(resolve => { release = () => resolve([200, modelSettings(2, JSON.parse(options.body).config)]); });
    return modelResponse(url);
  });
  await openModels(ui); edit(ui, 'model-default', 'pending'); const pending = submitModels(ui); await flush();
  ui.get('disconnect').onclick(); ui.get('key').value = 'fixture-key'; await ui.get('connect').onclick(); release(); await pending;
  assert.equal(ui.get('model-default').value, 'pending'); assert.equal(ui.get('model-save').disabled, true);
  await submitModels(ui); assert.equal(ui.calls.filter(call => call.method === 'POST').length, 1);
  await ui.get('model-reload').onclick(); assert.equal(ui.get('model-default').value, '');
});
test('model and candidate writes never automatically retry after authentication expires', async () => {
  for (const candidate of [false, true]) {
    const ui = panel((url, options) => options.method === 'POST' ? [401, {error: 'unauthorized'}] : modelResponse(url));
    await openModels(ui);
    if (candidate) { configureCandidateRequest(ui); await submitCandidates(ui); }
    else { edit(ui, 'model-default', 'draft'); await submitModels(ui); }
    assert.equal(ui.get('login-section').hidden, false); assert.equal(ui.calls.filter(call => call.method === 'POST').length, 1);
    assert.equal(ui.get(candidate ? 'generation-submit' : 'model-save').disabled, true);
  }
});

const {wakeDefaults, wakeConfigDraft, validateWakeDraft} = require('../../../internal/v2/daemon/web/panel.js');
const wakeConfig = overrides => ({...wakeDefaults, random_delay: {...wakeDefaults.random_delay}, allowed_hours: [...wakeDefaults.allowed_hours], ...overrides});
const wakeSettings = (revision = 1, config = wakeConfig(), instance = 'a') => ({...view(instance, 20 + revision), revision, config});
const wakeOverview = (instance = 'a', capabilities = {}) => ({...overview(), ...view(instance, 20), capabilities: {quota_query: true, model_settings: true, model_execution: true, wake_settings: true, automatic_wake: true, ...capabilities}});
function wakeResponse(url, settings = wakeSettings(), instance = 'a') {
  if (url.endsWith('/overview')) return [200, wakeOverview(instance)];
  if (url.endsWith('/wake-settings')) return [200, settings];
  if (url.includes('/wake-jobs?')) return [200, {...view(instance, 20), jobs: []}];
  return modelResponse(url, modelSettings(1, modelConfig(), instance), instance);
}
const submitWakeSettings = ui => ui.get('wake-settings-form').onsubmit({preventDefault() {}});
const submitWake = ui => ui.get('wake-manual-form').onsubmit({preventDefault() {}});
async function openWake(ui) {
  await ui.get('connect').onclick(); ui.nav.find(node => node.dataset.page === 'wake').listeners.get('click')(); await flush();
}
test('background wake reads do not disable the editor or overwrite in-progress typing', async () => {
  let reads = 0, release;
  const ui = panel(url => url.endsWith('/wake-settings') && ++reads > 1 ? new Promise(resolve => { release = () => resolve([200, wakeSettings()]); }) : wakeResponse(url));
  await openWake(ui); edit(ui, 'wake-per-user', '0.5');
  const pending = ui.tick(5000); await flush();
  assert.equal(ui.get('wake-settings-fields').disabled, false);
  edit(ui, 'wake-per-user', '0.75'); release(); await pending;
  assert.equal(ui.get('wake-per-user').value, '0.75');
  assert.equal(ui.get('wake-settings-fields').disabled, false);
  assert.equal(ui.calls.some(call => call.method === 'POST'), false);
});
function configureWakeRequest(ui, account = 'codex-01') {
  ui.get('wake-account').value = account; ui.get('wake-manual-consent').checked = true; ui.get('wake-manual-consent').onchange();
}
test('wake policy validation mirrors float capacity, interval, timezone and opt-in safety limits', () => {
  const draft = wakeConfigDraft(wakeConfig()); assert.deepEqual(validateWakeDraft(draft).errors, {});
  for (const perUser of ['0', '0.5', '100']) assert.equal(validateWakeDraft({...draft, perUser}).errors.perUser, undefined);
  for (const perUser of ['', 'Infinity', 'NaN', '-1', '100.1']) assert.ok(validateWakeDraft({...draft, perUser}).errors.perUser);
  assert.deepEqual(validateWakeDraft({...draft, allowed: '', dailyTarget: '0'}).config.allowed_hours, []);
  assert.equal(validateWakeDraft({...draft, allowed: '23:00-04:00, 08:00-12:00', dailyWindow: '23:00-24:00'}).errors.allowed, undefined);
  assert.ok(validateWakeDraft({...draft, allowed: '23:00-04:00, 03:00-05:00'}).errors.allowed);
  for (const dailyWindow of ['23:00-01:00', '', '08:00-08:00', '08:00-09:00, 10:00-11:00']) assert.ok(validateWakeDraft({...draft, dailyWindow}).errors.dailyWindow);
  assert.ok(validateWakeDraft({...draft, dailyTarget: '3', maximum: '2'}).errors.dailyTarget);
  assert.ok(validateWakeDraft({...draft, maximum: '0'}).errors.maximum);
  assert.ok(validateWakeDraft({...draft, enabled: true}).errors.enabled);
  assert.deepEqual(validateWakeDraft({...draft, enabled: true, weekly: true}).errors, {});
  assert.ok(validateWakeDraft({...draft, timezone: 'Local'}).errors.timezone);
  assert.ok(validateWakeDraft({...draft, delayMin: '60', delayMax: '5'}).errors.delayMax);
});
test('wake reads are capability gated, defaults stay off and opening or refreshing never submits work', async () => {
  const legacy = panel(basic); await openWake(legacy);
  assert.equal(legacy.calls.some(call => call.url.includes('/wake-')), false);
  assert.equal(legacy.get('auto-wake').disabled, true); assert.equal(legacy.get('wake-manual-fields').disabled, true);
  const ui = panel(url => wakeResponse(url)); await ui.get('connect').onclick();
  assert.equal(ui.calls.filter(call => call.url.endsWith('/wake-settings')).length, 1);
  assert.equal(ui.calls.some(call => call.url.includes('/wake-jobs')), false);
  assert.equal(ui.get('auto-wake').checked, false); assert.equal(ui.get('auto-wake').disabled, false);
  ui.nav.find(node => node.dataset.page === 'wake').listeners.get('click')(); await flush(); await ui.get('wake-history-refresh').onclick();
  assert.equal(ui.get('wake-enabled').checked, false); assert.equal(ui.get('wake-manual-submit').disabled, true);
  assert.equal(ui.calls.some(call => call.method === 'POST'), false);
});
test('unchanged wake polling preserves consent but changed rules and explicit reload clear it', async () => {
  let settings = wakeSettings();
  const ui = panel(url => wakeResponse(url, settings)); await openWake(ui);
  ui.get('wake-settings-consent').checked = true; ui.get('wake-settings-consent').onchange();
  await ui.tick(5000); assert.equal(ui.get('wake-settings-consent').checked, true);
  settings = wakeSettings(2, wakeConfig({accounts_per_user: 0.5}));
  await ui.tick(5000); assert.equal(ui.get('wake-settings-consent').checked, false);
  ui.get('wake-settings-consent').checked = true;
  await ui.get('wake-settings-reload').onclick();
  assert.equal(ui.get('wake-settings-consent').checked, false);
  assert.equal(ui.calls.some(call => call.method === 'POST'), false);
});
test('reading historical wake results does not disable an unrelated manual account draft', async () => {
  let release;
  const job = {...view(), job_id: 'historic', account_id: 'historic-account', phase: 'completed'};
  const ui = panel(url => url.includes('/wake-jobs?job_id=') ? new Promise(resolve => { release = () => resolve([200, job]); }) : url.includes('/wake-jobs?limit=') ? [200, {...view(), jobs: [job]}] : wakeResponse(url));
  await openWake(ui); ui.get('wake-account').value = 'new-draft';
  const card = ui.get('wake-history').children[0], button = card.children.at(-1);
  const reading = button.onclick(); await flush();
  assert.equal(ui.get('wake-history').children[0], card);
  assert.equal(card.children.at(-1), button);
  assert.equal(ui.get('wake-manual-fields').disabled, false);
  assert.equal(ui.get('wake-manual-submit').disabled, true);
  release(); await reading;
  assert.equal(ui.get('wake-account').value, 'new-draft');
  assert.equal(card.children.at(-1), button);
  assert.equal(ui.calls.some(call => call.method === 'POST'), false);
});
test('wake opt-in requires a rule and separate spend consent; saving uses only wake revision and config', async () => {
  let settings = wakeSettings();
  const ui = panel((url, options) => {
    if (url.endsWith('/wake-settings') && options.method === 'POST') { settings = wakeSettings(2, JSON.parse(options.body).config); return [200, settings]; }
    return wakeResponse(url, settings);
  });
  await openWake(ui); edit(ui, 'wake-enabled', true); await submitWakeSettings(ui);
  assert.equal(ui.calls.some(call => call.method === 'POST'), false);
  edit(ui, 'wake-five-hour', true); await submitWakeSettings(ui); assert.equal(ui.get('wake-settings-save').disabled, true);
  assert.equal(ui.calls.some(call => call.method === 'POST'), false);
  ui.get('wake-settings-consent').checked = true; ui.get('wake-settings-consent').onchange();
  assert.equal(ui.get('wake-settings-save').disabled, false); await submitWakeSettings(ui);
  const posts = ui.calls.filter(call => call.method === 'POST'); assert.equal(posts.length, 1); assert.ok(posts[0].url.endsWith('/wake-settings'));
  assert.deepEqual(JSON.parse(posts[0].body), {revision: 1, config: wakeConfig({enabled: true, on_five_hour_reset: true})});
  assert.equal(ui.get('wake-settings-consent').checked, false); assert.equal(ui.get('auto-wake').checked, true);
  assert.equal(ui.calls.some(call => /prompt-generations|\/wake-jobs$|\/jobs$/.test(call.url)), false);
});
test('editing any enabled wake draft clears spend consent while disabled rules save without consent', async () => {
  const ui = panel((url, options) => url.endsWith('/wake-settings') && options.method === 'POST' ? [200, wakeSettings(2, JSON.parse(options.body).config)] : wakeResponse(url));
  await openWake(ui); ui.get('wake-settings-consent').checked = true; edit(ui, 'wake-per-user', '0.5');
  assert.equal(ui.get('wake-settings-consent').checked, false); assert.equal(ui.get('wake-settings-save').disabled, false);
  await submitWakeSettings(ui); assert.equal(ui.calls.filter(call => call.method === 'POST').length, 1);
});
test('overview auto wake directly saves only the saved enabled flag and preserves the independent editor draft', async () => {
  let settings = wakeSettings(1, wakeConfig({on_five_hour_reset: true}));
  const ui = panel((url, options) => {
    if (url.endsWith('/wake-settings') && options.method === 'POST') { settings = wakeSettings(settings.revision + 1, JSON.parse(options.body).config); return [200, settings]; }
    return wakeResponse(url, settings);
  });
  await openWake(ui); edit(ui, 'wake-per-user', '0.5');
  ui.nav.find(node => node.dataset.page === 'overview').listeners.get('click')();
  ui.get('auto-wake').checked = true; await ui.get('auto-wake').onchange();
  assert.equal(ui.get('auto-wake').checked, false); assert.match(ui.get('wake-overview-message').textContent, /确认/);
  assert.equal(ui.calls.some(call => call.method === 'POST'), false);
  ui.get('wake-overview-consent').checked = true; ui.get('auto-wake').checked = true; await ui.get('auto-wake').onchange();
  assert.equal(settings.config.enabled, true); assert.equal(settings.config.accounts_per_user, 1); assert.equal(ui.get('wake-per-user').value, '0.5');
  assert.equal(ui.panels.find(node => node.dataset.view === 'overview').hidden, false); assert.equal(ui.get('wake-overview-consent').checked, false);
  ui.get('auto-wake').checked = false; await ui.get('auto-wake').onchange();
  assert.equal(settings.config.enabled, false); assert.equal(ui.get('wake-per-user').value, '0.5');
  assert.equal(ui.calls.filter(call => call.method === 'POST').length, 2);
});
test('overview refuses enabling an empty rule set and settings capability alone cannot authorize execution', async () => {
  const ui = panel(url => url.endsWith('/overview') ? [200, wakeOverview('a', {model_execution: false})] : wakeResponse(url));
  await openWake(ui); assert.equal(ui.get('auto-wake').disabled, true); assert.equal(ui.get('wake-enabled').disabled, true);
  assert.equal(ui.get('wake-manual-fields').disabled, true);
  edit(ui, 'wake-enabled', true); edit(ui, 'wake-five-hour', true); ui.get('wake-settings-consent').checked = true; await submitWakeSettings(ui);
  assert.equal(ui.calls.some(call => call.method === 'POST'), false);
  const empty = panel(url => wakeResponse(url)); await openWake(empty);
  empty.get('wake-overview-consent').checked = true; empty.get('auto-wake').checked = true; await empty.get('auto-wake').onchange();
  assert.equal(empty.get('auto-wake').checked, false); assert.match(empty.get('wake-overview-message').textContent, /至少开启/);
  assert.equal(empty.calls.some(call => call.method === 'POST'), false);
});
test('pending wake settings saves lock controls and conflicts require explicit GET verification without automatic retry', async () => {
  let release, posts = 0;
  const ui = panel((url, options) => {
    if (url.endsWith('/wake-settings') && options.method === 'POST') { posts++; return new Promise(resolve => { release = resolve; }); }
    return wakeResponse(url);
  });
  await openWake(ui); edit(ui, 'wake-per-user', '0.5'); const pending = submitWakeSettings(ui); await flush();
  assert.equal(ui.get('wake-settings-fields').disabled, true); assert.equal(ui.get('auto-wake').disabled, true); await submitWakeSettings(ui);
  release([409, {error: 'wake_settings_conflict'}]); await pending;
  assert.equal(ui.get('wake-per-user').value, '0.5'); assert.equal(ui.get('wake-settings-save').disabled, true); assert.equal(ui.get('auto-wake').disabled, true);
  await ui.tick(5000); await submitWakeSettings(ui); assert.equal(posts, 1);
  await ui.get('wake-settings-reload').onclick(); assert.equal(ui.get('wake-per-user').value, '1'); assert.equal(ui.get('wake-settings-conflict').hidden, true);
});
test('lost wake settings acknowledgements never retry; overview GET verification preserves dirty rule drafts', async () => {
  let settings = wakeSettings(1, wakeConfig({on_weekly_reset: true}));
  const ui = panel((url, options) => {
    if (url.endsWith('/wake-settings') && options.method === 'POST') { settings = wakeSettings(2, JSON.parse(options.body).config); throw new Error('lost ack'); }
    return wakeResponse(url, settings);
  });
  await openWake(ui); edit(ui, 'wake-per-user', '0.25');
  ui.get('wake-overview-consent').checked = true; ui.get('auto-wake').checked = true; await ui.get('auto-wake').onchange();
  assert.equal(ui.get('auto-wake').checked, false); assert.equal(ui.get('wake-per-user').value, '0.25');
  await ui.tick(5000); assert.equal(ui.calls.filter(call => call.method === 'POST').length, 1);
  await ui.get('wake-overview-reload').onclick(); assert.equal(ui.get('auto-wake').checked, true); assert.equal(ui.get('wake-per-user').value, '0.25');
  assert.equal(ui.get('wake-settings-save').disabled, true); assert.equal(ui.calls.filter(call => call.method === 'POST').length, 1);
});
test('wake draft polling preserves dirty configuration and marks concurrent saved changes as conflicts', async () => {
  let settings = wakeSettings(); const ui = panel(url => wakeResponse(url, settings)); await openWake(ui);
  edit(ui, 'wake-per-user', '0.5'); await ui.tick(5000); assert.equal(ui.get('wake-per-user').value, '0.5');
  settings = wakeSettings(2, wakeConfig({accounts_per_user: 2})); await ui.tick(5000);
  assert.equal(ui.get('wake-per-user').value, '0.5'); assert.equal(ui.get('wake-settings-conflict').hidden, false);
  assert.equal(ui.get('wake-settings-save').disabled, true); assert.equal(ui.calls.some(call => call.method === 'POST'), false);
});
test('manual wake requires explicit spend consent, validates account and posts only its ID once', async () => {
  const ui = panel((url, options) => url.endsWith('/wake-jobs') && options.method === 'POST' ? [202, {...view(), job_id: 'manual-one', phase: 'checking'}] : wakeResponse(url));
  await openWake(ui); await submitWake(ui); assert.equal(ui.calls.some(call => call.method === 'POST'), false);
  configureWakeRequest(ui, ''); await submitWake(ui); configureWakeRequest(ui, 'bad\naccount'); await submitWake(ui);
  assert.equal(ui.calls.some(call => call.method === 'POST'), false);
  configureWakeRequest(ui); await submitWake(ui); await submitWake(ui);
  const posts = ui.calls.filter(call => call.method === 'POST'); assert.equal(posts.length, 1);
  assert.deepEqual(JSON.parse(posts[0].body), {account_id: 'codex-01'}); assert.ok(posts[0].headers['Idempotency-Key']);
  assert.equal(ui.get('wake-manual-fields').disabled, true); assert.match(ui.get('wake-manual-message').textContent, /查询并核实/);
});
test('manual wake reads each real execution phase and permits a new intent only after a known terminal result and new consent', async () => {
  let phase = 'queued';
  const ui = panel((url, options) => url.startsWith('/v0/management/codex-window-refresh/v2/wake-jobs') ? url.includes('?limit=') ? [200, {...view(), jobs: []}] : [200, {...view(), job_id: 'phase-job', account_id: 'codex-01', reason: 'manual', phase}] : wakeResponse(url));
  await openWake(ui); configureWakeRequest(ui); await submitWake(ui);
  for (phase of ['checking', 'prepared', 'sending', 'verifying']) {
    await ui.get('wake-history').children[0].children.at(-1).onclick(); assert.equal(ui.get('wake-manual-fields').disabled, true);
    assert.equal(ui.calls.filter(call => call.method === 'POST').length, 1);
  }
  phase = 'completed'; await ui.get('wake-history').children[0].children.at(-1).onclick();
  assert.equal(ui.get('wake-manual-fields').disabled, false); assert.equal(ui.get('wake-manual-consent').checked, false);
  await submitWake(ui); assert.equal(ui.calls.filter(call => call.method === 'POST').length, 1);
});
test('manual wake lost acknowledgement verifies the exact original key and account without automatic POST retry', async () => {
  let posts = 0;
  const ui = panel((url, options) => {
    if (url.endsWith('/wake-jobs') && options.method === 'POST') { if (++posts === 1) throw new Error('lost acknowledgement'); return [202, {...view(), job_id: 'verified-wake', phase: 'prepared'}]; }
    return wakeResponse(url);
  });
  await openWake(ui); configureWakeRequest(ui); await submitWake(ui); await ui.tick(5000);
  assert.equal(posts, 1); assert.equal(ui.get('wake-manual-fields').disabled, true); assert.equal(ui.get('wake-manual-verify').hidden, false);
  ui.get('wake-account').value = 'replacement'; await submitWake(ui); assert.equal(posts, 1);
  await ui.get('wake-manual-verify').onclick(); const writes = ui.calls.filter(call => call.method === 'POST'); assert.equal(writes.length, 2);
  assert.equal(writes[0].body, writes[1].body); assert.equal(writes[0].headers['Idempotency-Key'], writes[1].headers['Idempotency-Key']);
});
test('unknown manual wake never resends or releases its account fields after refresh or settings reads', async () => {
  const job = {...view(), job_id: 'unknown-wake', account_id: 'codex-01', reason: 'manual', phase: 'unknown', hold_reason: 'verification_pending'};
  const ui = panel((url, options) => url.includes('/wake-jobs') ? url.includes('?limit=') ? [200, {...view(), jobs: [job]}] : [200, job] : wakeResponse(url));
  await openWake(ui); configureWakeRequest(ui); await submitWake(ui); await ui.get('wake-history-refresh').onclick(); await ui.get('wake-settings-reload').onclick();
  assert.equal(ui.get('wake-manual-fields').disabled, true); assert.match(ui.get('wake-manual-message').textContent, /禁止|不会重发/);
  await submitWake(ui); await ui.get('wake-manual-verify').onclick();
  assert.equal(ui.calls.filter(call => call.method === 'POST').length, 1); assert.match(ui.get('wake-history').textContent, /等待结果核实|禁止重发/);
});
test('missing wake jobs become read-only unknown results and never create replacement work', async () => {
  const ui = panel((url, options) => url.endsWith('/wake-jobs') && options.method === 'POST' ? [202, {...view(), job_id: 'gone-wake', phase: 'queued'}] : url.includes('/wake-jobs?job_id=') ? [404, {error: 'not_found'}] : wakeResponse(url));
  await openWake(ui); configureWakeRequest(ui); await submitWake(ui); await ui.tick(5000);
  assert.equal(ui.get('wake-manual-fields').disabled, true); assert.match(ui.get('wake-manual-message').textContent, /无法核实/);
  await submitWake(ui); await ui.get('wake-manual-verify').onclick(); assert.equal(ui.calls.filter(call => call.method === 'POST').length, 1);
});
test('wake skipped and failed results explain holds without inventing success or retrying', async () => {
  for (const phase of ['skipped', 'failed', 'cancelled']) {
    const ui = panel((url, options) => url.endsWith('/wake-jobs') && options.method === 'POST' ? [202, {...view(), job_id: 'terminal-wake', phase, hold_reason: 'active_window', error_code: phase === 'failed' ? 'wake_model_missing' : ''}] : wakeResponse(url));
    await openWake(ui); configureWakeRequest(ui); await submitWake(ui);
    assert.equal(ui.get('wake-manual-fields').disabled, false); assert.equal(ui.get('wake-manual-consent').checked, false);
    assert.match(ui.get('wake-history').textContent, /已有活动窗口/); await submitWake(ui); assert.equal(ui.calls.filter(call => call.method === 'POST').length, 1);
  }
});
test('manual wake preflight rejections clear spend consent without fabricating jobs', async () => {
  for (const [status, error] of [[409, 'wake_account_unavailable'], [409, 'wake_model_missing'], [400, 'invalid_wake_request'], [429, 'queue_full'], [503, 'wake_unavailable']]) {
    const ui = panel((url, options) => url.endsWith('/wake-jobs') && options.method === 'POST' ? [status, {error}] : wakeResponse(url));
    await openWake(ui); configureWakeRequest(ui); await submitWake(ui);
    assert.equal(ui.get('wake-manual-fields').disabled, false); assert.equal(ui.get('wake-manual-consent').checked, false);
    assert.match(ui.get('wake-manual-message').textContent, /未接受/); await submitWake(ui); assert.equal(ui.calls.filter(call => call.method === 'POST').length, 1);
  }
});
test('manual wake uncertain storage failures and verification failures stay locked with the same intent', async () => {
  const ui = panel((url, options) => url.endsWith('/wake-jobs') && options.method === 'POST' ? [503, {error: 'storage_unavailable'}] : wakeResponse(url));
  await openWake(ui); configureWakeRequest(ui); await submitWake(ui); await ui.get('wake-manual-verify').onclick(); await submitWake(ui);
  const posts = ui.calls.filter(call => call.method === 'POST'); assert.equal(posts.length, 2); assert.equal(posts[0].body, posts[1].body);
  assert.equal(ui.get('wake-manual-fields').disabled, true);
});
test('backend restart prevents uncertain manual wake POST verification against a new instance', async () => {
  let instance = 'a';
  const ui = panel((url, options) => url.endsWith('/wake-jobs') && options.method === 'POST' ? Promise.reject(new Error('lost ack')) : wakeResponse(url, wakeSettings(1, wakeConfig(), instance), instance));
  await openWake(ui); configureWakeRequest(ui); await submitWake(ui); instance = 'b'; await ui.tick(5000);
  assert.match(ui.get('wake-manual-message').textContent, /实例已更换/); assert.equal(ui.get('wake-manual-verify').disabled, true);
  await ui.get('wake-manual-verify').onclick(); await submitWake(ui); assert.equal(ui.calls.filter(call => call.method === 'POST').length, 1);
});
test('expired authentication during wake writes stops polling and never retries', async () => {
  for (const manual of [true, false]) {
    const ui = panel((url, options) => options.method === 'POST' ? [401, {error: 'unauthorized'}] : wakeResponse(url)); await openWake(ui);
    if (manual) { configureWakeRequest(ui); await submitWake(ui); }
    else { edit(ui, 'wake-per-user', '0.5'); await submitWakeSettings(ui); }
    assert.equal(ui.get('login-section').hidden, false); assert.equal(ui.calls.filter(call => call.method === 'POST').length, 1);
    assert.equal(ui.get('wake-manual-submit').disabled, true); assert.equal(ui.get('auto-wake').disabled, true);
  }
});
test('disconnecting an in-flight wake settings save preserves its draft and demands explicit read verification after reconnect', async () => {
  let release;
  const ui = panel((url, options) => url.endsWith('/wake-settings') && options.method === 'POST' ? new Promise(resolve => { release = () => resolve([200, wakeSettings(2, JSON.parse(options.body).config)]); }) : wakeResponse(url));
  await openWake(ui); edit(ui, 'wake-per-user', '0.5'); const pending = submitWakeSettings(ui); await flush();
  ui.get('disconnect').onclick(); ui.get('key').value = 'fixture-key'; await ui.get('connect').onclick(); release(); await pending;
  assert.equal(ui.get('wake-per-user').value, '0.5'); assert.equal(ui.get('wake-settings-save').disabled, true); assert.equal(ui.get('auto-wake').disabled, true);
  await submitWakeSettings(ui); assert.equal(ui.calls.filter(call => call.method === 'POST').length, 1);
  await ui.get('wake-settings-reload').onclick(); assert.equal(ui.get('wake-per-user').value, '1');
});
test('disconnecting a pending manual wake prevents late acknowledgements from enabling a new request', async () => {
  let release;
  const ui = panel((url, options) => url.endsWith('/wake-jobs') && options.method === 'POST' ? new Promise(resolve => { release = () => resolve([202, {...view(), job_id: 'late-wake', phase: 'prepared'}]); }) : wakeResponse(url));
  await openWake(ui); configureWakeRequest(ui); const pending = submitWake(ui); await flush(); ui.get('disconnect').onclick();
  ui.get('key').value = 'fixture-key'; await ui.get('connect').onclick(); release(); await pending;
  assert.equal(ui.get('wake-manual-fields').disabled, true); assert.equal(ui.get('wake-manual-verify').hidden, false);
  await submitWake(ui); assert.equal(ui.calls.filter(call => call.method === 'POST').length, 1);
});
test('backend wake settings changes invalidate an earlier overview spend confirmation', async () => {
  let settings = wakeSettings(1, wakeConfig({on_five_hour_reset: true}));
  const ui = panel(url => wakeResponse(url, settings)); await openWake(ui); ui.get('wake-overview-consent').checked = true;
  settings = wakeSettings(2, wakeConfig({on_five_hour_reset: true, accounts_per_user: 2})); await ui.tick(5000);
  assert.equal(ui.get('wake-overview-consent').checked, false);
  ui.get('auto-wake').checked = true; await ui.get('auto-wake').onchange(); assert.equal(ui.calls.some(call => call.method === 'POST'), false);
});
test('wake histories safely render all backend hold reasons and unknown public errors without exposing arbitrary HTML', async () => {
  const holds = ['quota_pending', 'quota_unknown', 'quota_blocked', 'active_window', 'weekly_reset_conflict', 'activity_unknown', 'account_busy', 'capacity_satisfied', 'outside_allowed_hours', 'model_busy', 'model_missing', 'verification_pending'];
  const jobs = holds.map((hold_reason, index) => ({job_id: 'hold-' + index, account_id: '<img src=x onerror=alert(1)>', phase: 'queued', reason: 'weekly_reset', hold_reason, error_code: index ? '' : 'internal_secret_text'}));
  const ui = panel(url => url.includes('/wake-jobs?limit=') ? [200, {...view(), jobs}] : wakeResponse(url)); await openWake(ui);
  assert.equal(ui.get('wake-history').children.length, 12); assert.match(ui.get('wake-history').textContent, /额度证据未知|目标容量已满足|等待结果核实/);
  assert.doesNotMatch(ui.get('wake-history').textContent, /internal_secret_text/);
  assert.equal(ui.get('wake-history').children[0].children[0].children[0].children.length, 0);
  assert.equal(ui.calls.some(call => call.method === 'POST'), false);
});
test('malformed wake save acknowledgements remain uncertain and cannot be blindly resubmitted', async () => {
  for (const config of [undefined, {}, {...wakeConfig(), allowed_hours: null}]) {
    const ui = panel((url, options) => url.endsWith('/wake-settings') && options.method === 'POST' ? [200, {...view(), revision: 2, config}] : wakeResponse(url));
    await openWake(ui); edit(ui, 'wake-per-user', '0.5'); await submitWakeSettings(ui);
    assert.equal(ui.get('wake-settings-save').disabled, true); assert.equal(ui.get('auto-wake').disabled, true);
    await submitWakeSettings(ui); assert.equal(ui.calls.filter(call => call.method === 'POST').length, 1);
    assert.match(ui.get('wake-settings-message').textContent, /尚未确认/);
  }
});
test('older wake history snapshots cannot roll a known unknown outcome back to an active state', async () => {
  let jobRevision = 30, phase = 'unknown';
  const ui = panel((url, options) => {
    if (url.includes('/wake-jobs')) {
      const job = {job_id: 'monotonic-wake', account_id: 'codex-01', phase};
      return url.includes('?limit=') ? [200, {...view('a', jobRevision), jobs: [job]}] : [200, {...view('a', jobRevision), ...job}];
    }
    return wakeResponse(url);
  });
  await openWake(ui); configureWakeRequest(ui); await submitWake(ui);
  jobRevision = 29; phase = 'queued'; await ui.get('wake-history-refresh').onclick();
  assert.equal(ui.get('wake-manual-fields').disabled, true); assert.equal(ui.get('wake-manual-status').textContent, '结果未知');
  await submitWake(ui); assert.equal(ui.calls.filter(call => call.method === 'POST').length, 1);
});
