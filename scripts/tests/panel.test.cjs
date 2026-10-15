'use strict';
const {test} = require('node:test');
const assert = require('node:assert/strict');
const {readFileSync} = require('node:fs');
const {runInNewContext} = require('node:vm');
const {createSession, createEditor, createPanelPreferences, createAccountPreferences, configChanges, changeValue, accountComparator, observationAge, resetTime, countdown, windowCountdown, manualWakeReason, apiBase, readPanelKey, quotaFailure, dailyTimeRange} = require('../../internal/plugin/web/panel.js');
const response = (value, status = 200, type = 'application/json') => ({ok: status >= 200 && status < 300, status, headers: {get: () => type}, json: async () => value});
test('history endpoints allow only local read filters', async () => {
  const calls=[], session=createSession(async (url,options)=>{calls.push({url,options});return response({entries:[]});});session.setKey('key');
  await session.request('/history?account_id=%3Cimg%3E&limit=50');
  assert.equal(calls.length,1);
  for(const path of ['/history?url=https://evil.test','/history?limit=2&limit=3','/summary?cursor=1']) await assert.rejects(session.request(path),{code:'invalid_endpoint'});
  await assert.rejects(session.request('/history?limit=1','POST'),{code:'invalid_endpoint'});
  session.disconnect();
});
test('random prompt settings default for legacy drafts without preserving old messages', () => {
  const editor=createEditor();editor.load({config:policy(),revision:'random'});
  editor.update('execution.prompt_mode','random');editor.update('execution.prompt_pool',['Hello.','Ready.']);editor.update('reset_rules.weekly.prompt_pool',[]);
  editor.update('quota_query.five_hour_recent_usage.enabled',true);editor.update('quota_query.five_hour_recent_usage.lookback','30m');
  assert.deepEqual(editor.value().execution.prompt_pool,['Hello.','Ready.']);
  assert.equal(editor.value().quota_query.automatic,false);
  editor.replace(editor.value());
  assert.throws(()=>editor.replace({...editor.value(),execution:{...editor.value().execution,prompt_pool:[null]}}),{code:'invalid_config'});
});
test('history timeline renders actual prompt and source as text without writes', async () => {
  const now=new Date().toISOString();
  const {lookup,requests,settle}=createPanelFixture(async(url,options,data)=>{
    if(url.includes('/history?'))return response({entries:[{kind:'wake',account_id:'<img>',status:'verified',started_at:now,prompt:'<script>alert(1)</script>',sources:[{trigger:'daily_schedule',rule_id:'morning',evidence_source:'cpa_wham_usage',evidence_at:now}]}],next_cursor:12,retention_days:30,maximum_entries:10000});
    return response(data[url.split('/').pop()]);
  });
  lookup('management-key').value='key';lookup('login-form').listeners.get('submit')({preventDefault(){}});await settle();
  lookup('history-kind').value='wake';lookup('history-refresh').listeners.get('click')();await settle();
  const row=lookup('history-list').children[0];
  assert.ok(row.children.some(item=>item.textContent==='实际发送：<script>alert(1)</script>'));
  assert.ok(row.children.some(item=>item.textContent.includes('每日规则')&&item.textContent.includes('morning')));
  assert.equal(lookup('history-next').disabled,false);
  lookup('history-next').listeners.get('click')();await settle();
  assert.ok(requests.some(request=>request.url.includes('cursor=12')));
  assert.equal(requests.some(request=>request.options.method==='POST'),false);
  lookup('disconnect').listeners.get('click')();assert.equal(lookup('history-list').children.length,0);
});
test('model generated candidates require confirmation and explicit draft application', async () => {
  let confirmed=false;const confirmations=[];
  const {lookup,requests,settle}=createPanelFixture(async(url,options,data)=>{
    data.summary.host_enabled=true;data.capabilities.ExactAccountExecution=true;data.accounts={a:{ID:'a',Enabled:true}};
    if(url.endsWith('/prompt-generation')&&options.method==='POST'){
      const request=JSON.parse(options.body);data.summary.scheduler.prompt_generation={request,phase:'completed',candidates:['Hello.','Hi.','Ready.','Yes.','OK.']};return response({status:'queued'},202);
    }
    return response(data[url.split('/').pop()]);
  },message=>{confirmations.push(message);return confirmed;});
  lookup('management-key').value='key';lookup('login-form').listeners.get('submit')({preventDefault(){}});await settle();
  lookup('generation-account').value='a';lookup('generation-model').value='model';lookup('generation-count').value='5';lookup('generation-target').value='default';
  lookup('generation-start').listeners.get('click')();await settle();assert.equal(requests.some(request=>request.options.method==='POST'),false);
  confirmed=true;lookup('generation-start').listeners.get('click')();await settle();
  const writes=requests.filter(request=>request.options.method==='POST');assert.equal(writes.length,1);assert.ok(writes[0].url.endsWith('/prompt-generation'));
  assert.match(confirmations[0],/消耗额度/);assert.equal(lookup('generation-preview').value,'Hello.\nHi.\nReady.\nYes.\nOK.');
  lookup('generation-apply').listeners.get('click')();
  assert.deepEqual(JSON.parse(lookup('config-json').value).execution.prompt_pool,['Hello.','Hi.','Ready.','Yes.','OK.']);
  assert.equal(requests.filter(request=>request.options.method==='POST').length,1);
  lookup('disconnect').listeners.get('click')();
});
test('Antigravity candidates use an explicit model and preserve the preview/apply workflow', async () => {
  const confirmations = [];
  const fixture = createPanelFixture(async (url, options, data) => {
    data.capabilities.ExactAccountExecution = true;
    data.capabilities.QuotaAdapterReady = false;
    data.generation_accounts = {
      ag: {id: 'ag', provider: 'antigravity', enabled: true},
      disabled: {id: 'disabled', provider: 'antigravity', enabled: false}
    };
    if (url.endsWith('/prompt-generation') && options.method === 'POST') {
      const request = JSON.parse(options.body);
      data.summary.scheduler.prompt_generation = {request, phase: 'completed', candidates: ['Hello.', 'Hi.', 'Ready.', 'Yes.', 'OK.']};
      return response({status: 'queued'}, 202);
    }
    return response(data[url.split('/').pop()]);
  }, message => { confirmations.push(message); return true; });
  await loginFixture(fixture);
  const select = fixture.lookup('generation-account');
  assert.ok(select.children.some(item => item.value === 'ag' && item.textContent === '反重力 · ag'));
  assert.equal(select.children.some(item => item.value === 'disabled'), false);
  assert.equal(descendants(fixture.lookup('account-list')).some(item => item.textContent === 'ag'), false);
  select.value = 'ag'; select.listeners.get('change')();
  assert.equal(fixture.lookup('generation-start').disabled, false, 'Antigravity generation does not require the Codex quota adapter');
  assert.match(fixture.lookup('generation-model').placeholder, /反重力/);
  fixture.lookup('generation-count').value = '5'; fixture.lookup('generation-target').value = 'weekly';
  fixture.lookup('generation-start').listeners.get('click')(); await fixture.settle();
  assert.equal(confirmations.length, 0); assert.match(fixture.lookup('notice').textContent, /请输入.*反重力.*模型名/);
  fixture.lookup('generation-model').value = 'registered-ag-model';
  fixture.lookup('generation-start').listeners.get('click')(); await fixture.settle();
  const writes = fixture.requests.filter(request => request.options.method === 'POST');
  assert.equal(writes.length, 1);
  const request = JSON.parse(writes[0].options.body);
  assert.equal(request.provider, 'antigravity'); assert.equal(request.account_id, 'ag'); assert.equal(request.model, 'registered-ag-model');
  assert.match(confirmations[0], /反重力账号 ag.*registered-ag-model/);
  assert.doesNotMatch(confirmations[0], /五小时窗口/);
  assert.equal(fixture.lookup('generation-preview').value, 'Hello.\nHi.\nReady.\nYes.\nOK.');
  fixture.lookup('generation-apply').listeners.get('click')();
  assert.deepEqual(JSON.parse(fixture.lookup('config-json').value).reset_rules.weekly.prompt_pool, ['Hello.', 'Hi.', 'Ready.', 'Yes.', 'OK.']);
  assert.equal(fixture.requests.filter(request => request.options.method === 'POST').length, 1, 'Applying candidates must only change the local draft');
  fixture.lookup('disconnect').listeners.get('click')();
});
test('saving settings immediately restores the generation button without requiring polling', async () => {
  const {lookup,settle}=createPanelFixture(async(url,options,data)=>{
    data.summary.host_enabled=true;data.capabilities.ExactAccountExecution=true;data.accounts={a:{ID:'a',Enabled:true}};
    if(url.endsWith('/config')&&options.method==='POST'){
      const config=JSON.parse(options.body).config;data.config=config;return response({config,revision:'r2'});
    }
    return response(data[url.split('/').pop()]);
  });
  lookup('management-key').value='key';lookup('login-form').listeners.get('submit')({preventDefault(){}});await settle();
  assert.equal(lookup('generation-start').disabled,false);
  const config=JSON.parse(lookup('config-json').value);config.execution.prompt='saved fixed text';
  lookup('config-json').value=JSON.stringify(config);lookup('config-json').listeners.get('input')();lookup('json-apply').listeners.get('click')();
  lookup('config-save').listeners.get('click')();await settle();
  assert.equal(lookup('generation-start').disabled,false);
  lookup('disconnect').listeners.get('click')();
});
test('only confirmed active windows display a running countdown', () => {
  const now = Date.parse('2026-10-13T00:00:00Z');
  const fullWindow = {Kind: 'five_hour', State: 'unknown', Source: 'cpa_wham_usage', ResetAt: '2026-10-13T05:00:00Z', Remaining: {Value: 100, Unit: 'percent'}};
  assert.match(windowCountdown(fullWindow, now), /尚未确认窗口启动/);
  assert.match(windowCountdown({...fullWindow, State: 'inactive'}, now), /未启动/);
  assert.match(windowCountdown({...fullWindow, State: 'expired'}, now), /已到期/);
  assert.equal(windowCountdown({...fullWindow, State: 'active'}, now), '05:00:00');
  assert.match(windowCountdown({}, now), /不倒计时/);
});
test('manual wake remains independent of automatic settings but requires safety capabilities', async () => {
  const account = {ID: 'account-a', Enabled: true, FiveHour: {State: 'unknown'}};
  const config = {...policy(), execution: {...policy().execution, model: 'model'}};
  const summary = {running: true, host_enabled: true, enabled: false};
  const capabilities = {StableAccountIdentity: true, ExactAccountExecution: true, QuotaAdapterReady: true};
  assert.equal(manualWakeReason(account, config, summary, capabilities), '');
  assert.match(manualWakeReason(account, {...config, execution: {model: ''}}, summary, capabilities), /填写并保存/);
  assert.match(manualWakeReason(account, config, {...summary, host_enabled: false}, capabilities), /停用/);
  assert.match(manualWakeReason(account, config, summary, {}), /未就绪/);
  assert.match(manualWakeReason({...account, Task: {OperationID: 'pending'}}, config, summary, capabilities), /待验证/);
  for (const cached of [
    {Enabled: false},
    {Enabled: false, Availability: 'disabled'},
    {FiveHour: {State: 'active', ResetAt: new Date(Date.now() + 60000).toISOString()}},
    {FiveHourBaseline: {StableResetAt: new Date(Date.now() + 60000).toISOString()}},
    {BackoffUntil: new Date(Date.now() + 60000).toISOString()}
  ]) assert.equal(manualWakeReason({...account, ...cached}, config, summary, capabilities), '');
  assert.match(manualWakeReason(account, {...config, account_overrides: {'account-a': {paused: true}}}, summary, capabilities), /已暂停/);
  const calls = [], session = createSession(async (url, options) => {calls.push({url, options}); return response({status: 'queued'}, 202);});
  session.setKey('test-key');
  await session.request('/manual-wake', 'POST', {account_id: 'account-a'});
  assert.equal(calls.length, 1);
  assert.equal(calls[0].options.body, JSON.stringify({account_id: 'account-a'}));
  assert.ok(calls[0].url.endsWith('/manual-wake'));
  session.disconnect();
});
function createPanelFixture(fetcher, confirm = () => true, fields = [], options = {}) {
  const elements = new Map(), timers = new Map(), visibility = new Map(), events = new Map(), requests = [];
  const navigator = {onLine: true};
  let nextTimer = 0, clock = Date.now();
  class FixtureDate extends Date { static now() { return clock; } }
  function element() {
    return {value: '', textContent: '', checked: false, disabled: false, hidden: true, dataset: {}, listeners: new Map(), children: [],
      classList: {toggle() {}}, append(...items) {this.children.push(...items);}, replaceChildren(...items) {this.children = items;}, setAttribute() {},
      addEventListener(event, handler) {this.listeners.set(event, handler);}};
  }
  function lookup(id) {
    if (!elements.has(id)) elements.set(id, element());
    return elements.get(id);
  }
  const fieldElements = fields.map(({id, field, type}) => { const input = lookup(id); input.dataset.field = field; input.type = type || 'text'; return input; });
  const document = {hidden: false, getElementById: lookup, createElement: element, querySelectorAll: selector => selector === '[data-field]' || selector.startsWith('#workspace [data-field]') ? fieldElements : [],
    createTextNode: text => ({textContent: text, children: [], dataset: {}}),
    addEventListener: (event, handler) => visibility.set(event, handler)};
  const config = {...policy(), quota_query: {automatic: false, manual: false, interval: '30s'}};
  const data = {instance_id: 'fixture-instance', snapshot_seq: 0, summary: {running: true, enabled: false, scheduler: {}}, accounts: {},
    capabilities: {QuotaAdapterReady: true}, config, 'config-editor': {config, revision: 'r1'}};
  const records = new Map();
  Object.defineProperty(data, 'snapshot', {get() {
    const snapshot = structuredClone({instance_id: data.instance_id, snapshot_seq: ++data.snapshot_seq, summary: data.summary, accounts: data.accounts, generation_accounts: data.generation_accounts, capabilities: data.capabilities, config: data.config});
    if (!Array.isArray(snapshot.summary.scheduler.jobs)) snapshot.summary.scheduler.jobs = [...records.values()].map(record => {
      const current = data.summary.scheduler[record.kind];
      const detail = current && JSON.stringify(current) !== record.previous ? current : {phase: 'queued', account_id: record.target};
      return {job_id: record.id, kind: record.kind, phase: detail.phase, account_id: detail.account_id || record.target || '', [record.kind]: detail};
    });
    return snapshot;
  }});
  runInNewContext(readFileSync(require.resolve('../../internal/plugin/web/panel.js'), 'utf8'), {
    document, navigator, URL, URLSearchParams, TextEncoder, AbortController, structuredClone, confirm, Date: FixtureDate,
    location: {origin: 'https://cpa.test', pathname: '/v0/resource/plugins/codex-window-refresh/status'},
    localStorage: options.localStorage || {getItem: () => null}, addEventListener: (event, handler) => events.set(event, handler), setInterval() {},
    setTimeout: (callback, duration) => {const timer = ++nextTimer; timers.set(timer, {callback, duration}); return timer;},
    clearTimeout: timer => timers.delete(timer),
    fetch: async (url, options) => {
      requests.push({url, options});
      const kind = {'quota-query': 'quota_query', 'manual-wake': 'manual_wake', 'prompt-generation': 'prompt_generation'}[url.split('/').pop()];
      const previous = kind ? JSON.stringify(data.summary.scheduler[kind]) : '';
      const result = await (fetcher ? fetcher(url, options, data) : response(data[url.split('/').pop()] || {status: 'queued'}));
      if (kind && options.method === 'POST' && result.ok) {
        const body = options.body ? JSON.parse(options.body) : {}, value = await result.json();
        if (!Object.hasOwn(value, 'job_id')) value.job_id = body.request_id || 'fixture-job-' + requests.length;
        records.set(value.job_id, {id: value.job_id, kind, target: body.account_id || '', previous});
        return response(value, result.status);
      }
      return result;
    }
  });
  const settle = () => new Promise(resolve => setImmediate(resolve));
  async function poll() {
    const entry = [...timers].find(([, value]) => [500, 1000, 2000, 4000, 5000, 8000, 10000, 16000, 20000, 32000, 40000, 60000].includes(value.duration));
    assert.ok(entry, 'Expected a scheduled snapshot poll');
    const [timerID, timer] = entry;
    timers.delete(timerID);
    clock += timer.duration;
    timer.callback();
    await settle();
  }
  return {lookup, timers, visibility, events, navigator, document, requests, settle, poll};
}
test('configuration changes preserve baseline values and include nested, removed and account settings', () => {
  const editor = createEditor(), initial = policy();
  editor.load({config: initial, revision: 'r1'});
  const baseline = editor.baseline(); baseline.enabled = !initial.enabled;
  assert.equal(editor.baseline().enabled, initial.enabled, 'A caller cannot mutate the baseline');
  const draft = editor.value(); draft.enabled = !initial.enabled;
  draft.execution.model = 'different-model'; draft.execution.prompt_pool = ['One', 'Two'];
  draft.account_overrides = JSON.parse('{"account.with.dots":{"paused":true}}');
  delete draft.timezone;
  const changes = configChanges(initial, draft);
  assert.ok(changes.some(change => change.label === '自动模型唤醒' && change.before === initial.enabled && change.after === draft.enabled));
  assert.ok(changes.some(change => change.label === '规则时区' && change.after === undefined));
  assert.ok(changes.some(change => change.label.includes('账号「account.with.dots」') && change.after === true));
  assert.ok(changes.some(change => change.label === '模型执行 / 模型' && change.after === 'different-model'));
  assert.equal(changeValue(true), '开启'); assert.equal(changeValue(false), '关闭');
  assert.match(changeValue(undefined), /继承默认/);
  assert.equal(changeValue(['One', 'Two']), 'One；Two');
  assert.deepEqual(configChanges(initial, initial), []);
});
test('history subview paginates both ways and keeps the previous page after read failures', async () => {
  let fail = false;
  const panel = createPanelFixture((url, options, data) => {
    if (url.includes('/history?')) {
      if (fail) return response({}, 503);
      const cursor = Number(new URL(url, 'https://cpa.test').searchParams.get('cursor') || 0);
      return response({entries: [{id: cursor + 1, kind: 'query', status: 'completed', account_id: 'page-' + cursor, started_at: '2026-10-14T01:00:00Z'}], next_cursor: cursor === 0 ? 50 : 0, retention_days: 30, maximum_entries: 10000});
    }
    return response(data[url.split('/').pop()]);
  });
  panel.lookup('management-key').value = 'key'; panel.lookup('login-form').listeners.get('submit')({preventDefault() {}}); await panel.settle();
  assert.equal(panel.requests.some(request => request.url.includes('/history?')), false, 'Initial viewing must not load history');
  panel.lookup('plan-view-history').listeners.get('click')(); await panel.settle();
  assert.match(panel.lookup('history-status').textContent, /第 1 页/);
  assert.equal(panel.lookup('history-prev').disabled, true);
  assert.equal(panel.lookup('history-next').disabled, false);
  const first = panel.lookup('history-list').children[0];
  fail = true; panel.lookup('history-next').listeners.get('click')(); await panel.settle();
  assert.equal(panel.lookup('history-list').children[0], first, 'A failed read must retain the prior page');
  assert.match(panel.lookup('history-status').textContent, /保留上次结果/);
  fail = false; panel.lookup('history-next').listeners.get('click')(); await panel.settle();
  assert.match(panel.lookup('history-status').textContent, /第 2 页/);
  assert.equal(panel.lookup('history-prev').disabled, false);
  panel.lookup('history-prev').listeners.get('click')(); await panel.settle();
  assert.match(panel.lookup('history-status').textContent, /第 1 页/);
  const reads = panel.requests.length;
  panel.lookup('history-from').value = 'invalid-time'; panel.lookup('history-refresh').listeners.get('click')(); await panel.settle();
  assert.equal(panel.requests.length, reads, 'Invalid time filters must fail locally');
  assert.match(panel.lookup('history-status').textContent, /时间需包含/);
  assert.equal(panel.requests.filter(request => request.options.method === 'POST').length, 0);
});
test('unconfirmed model streams expose the diagnostic without suggesting a duplicate send', async () => {
  const panel = createPanelFixture((url, options, data) => {
    data.accounts = {a: {ID: 'a', Enabled: true}};
    data.summary.scheduler.manual_wake = {account_id: 'a', phase: 'unknown', last_error: 'model_stream_missing_completion'};
    return response(data[url.split('/').pop()]);
  });
  panel.lookup('management-key').value = 'key'; panel.lookup('login-form').listeners.get('submit')({preventDefault() {}}); await panel.settle();
  const text = item => item.textContent + item.children.map(text).join('');
  assert.match(text(panel.lookup('account-list')), /模型流未识别到完成事件/);
  assert.match(text(panel.lookup('account-list')), /不自动重发/);
  assert.doesNotMatch(text(panel.lookup('account-list')), /可手动重试/);
  assert.equal(panel.requests.filter(request => request.options.method === 'POST').length, 0);
});
test('save review shows cross-page changes and cancellation sends no write', async () => {
  let accepted = false;
  const confirmations = [], fixture = createPanelFixture((url, options, data) => {
    if (url.endsWith('/config') && options.method === 'POST') {
      data.config = JSON.parse(options.body).config;
      data['config-editor'] = {config: data.config, revision: 'r2'};
      return response(data['config-editor']);
    }
    return response(data[url.split('/').pop()]);
  }, message => {confirmations.push(message); return accepted;});
  const {lookup, settle, requests} = fixture;
  lookup('management-key').value = 'key'; lookup('login-form').listeners.get('submit')({preventDefault() {}}); await settle();
  const config = JSON.parse(lookup('config-json').value);
  config.quota_query.automatic = true; config.quota_query.interval = '5m'; config.execution.model = '<img src=x>';
  lookup('config-json').value = JSON.stringify(config); lookup('config-json').listeners.get('input')(); lookup('json-apply').listeners.get('click')();
  const text = item => item.textContent + item.children.map(text).join('');
  assert.match(text(lookup('config-changes')), /定期查询总开关关闭开启/);
  assert.match(text(lookup('config-changes')), /查询间隔30s5m/);
  assert.match(text(lookup('config-changes')), /<img src=x>/);
  assert.equal(lookup('changes-preview').hidden, false);
  lookup('config-save').listeners.get('click')(); await settle();
  assert.equal(requests.filter(request => request.options.method === 'POST').length, 0);
  assert.match(confirmations[0], /定期查询总开关：关闭 → 开启/);
  assert.match(confirmations[0], /查询间隔：30s → 5m/);
  accepted = true; lookup('config-save').listeners.get('click')(); await settle();
  assert.equal(requests.filter(request => request.options.method === 'POST').length, 1);
  assert.equal(lookup('save-bar').hidden, true);
  assert.equal(lookup('config-changes').children.length, 0);
});
test('observation age remains independent of page reads and failed quota queries keep old values', async () => {
  const observed = new Date(Date.now() - 12 * 60000).toISOString();
  const panel = createPanelFixture((url, options, data) => {
    data.accounts = {a: {ID: 'a', Enabled: true, FiveHour: {State: 'active', ConfirmedAt: observed, Remaining: {Value: 21, Unit: 'percent'}}}};
    data.summary.scheduler.quota_query = {phase: 'completed', account_id: 'a', failed: 1, last_error: 'quota_credential_expired', finished_at: observed};
    return response(data[url.split('/').pop()]);
  });
  panel.lookup('management-key').value = 'key'; panel.lookup('login-form').listeners.get('submit')({preventDefault() {}}); await panel.settle();
  assert.match(panel.lookup('updated-at').textContent, /页面更新于/);
  assert.match(panel.lookup('quota-freshness').textContent, /12 分钟前/);
  assert.equal(panel.lookup('quota-freshness-warning').hidden, false);
  const text = item => item.textContent + item.children.map(text).join('');
  assert.match(text(panel.lookup('account-list')), /21%/);
  assert.match(text(panel.lookup('account-list')), /保留上次额度/);
  panel.lookup('refresh').listeners.get('click')(); await panel.settle();
  assert.match(panel.lookup('quota-freshness').textContent, /12 分钟前/);
  assert.equal(panel.requests.filter(request => request.options.method === 'POST').length, 0);
  assert.match(observationAge('0001-01-01T00:00:00Z'), /尚无/);
  assert.match(observationAge(new Date(Date.now() + 60000).toISOString()), /晚于本机/);
});
test('account sorting keeps unknown values last and compares only matching percentage units', () => {
  const rows = [['unknown', {}], ['tokens', {FiveHour: {Remaining: {Value: 1, Unit: 'tokens'}}}], ['full', {FiveHour: {Remaining: {Value: 90, Unit: 'percent'}}}], ['low', {FiveHour: {Remaining: {Value: 10, Unit: 'percent'}}}]];
  assert.deepEqual([...rows].sort(accountComparator('five-remaining')).map(([id]) => id), ['low', 'full', 'tokens', 'unknown']);
  const resets = [['inactive', {FiveHour: {State: 'inactive', ResetAt: '2026-01-01T00:00:00Z'}}], ['later', {FiveHour: {State: 'active', ResetAt: '2026-10-14T12:00:00Z'}}], ['early', {FiveHour: {State: 'active', ResetAt: '2026-10-14T11:00:00Z'}}]];
  assert.deepEqual(resets.sort(accountComparator('reset')).map(([id]) => id), ['early', 'later', 'inactive']);
  assert.deepEqual([['healthy', {Enabled: true, FiveHour: {State: 'active'}}], ['blocked', {Enabled: false}]].sort(accountComparator('attention')).map(([id]) => id), ['blocked', 'healthy']);
});
test('account display preferences persist independently of auth, refresh and server rules', async () => {
  const entries = new Map(), localStorage = {getItem: name => entries.get(name) ?? null, setItem: (name, value) => entries.set(name, value)};
  const base = '/v0/management/codex-window-refresh';
  const preferences = createAccountPreferences({localStorage}, base);
  preferences.save('five-remaining', 'compact');
  assert.deepEqual(createAccountPreferences({localStorage}, base).value(), {sort: 'five-remaining', layout: 'compact'});
  assert.deepEqual(createAccountPreferences({localStorage}, '/other' + base).value(), {sort: 'id', layout: 'cards'});
  assert.deepEqual(createAccountPreferences({localStorage: {getItem() {throw new Error('blocked');}}}, base).value(), {sort: 'id', layout: 'cards'});
  const panel = createPanelFixture((url, options, data) => {
    data.accounts = {full: {ID: 'full', FiveHour: {Remaining: {Value: 90, Unit: 'percent'}}}, low: {ID: 'low', FiveHour: {Remaining: {Value: 10, Unit: 'percent'}}}};
    return response(data[url.split('/').pop()]);
  }, undefined, [], {localStorage});
  panel.lookup('management-key').value = 'key'; panel.lookup('login-form').listeners.get('submit')({preventDefault() {}}); await panel.settle();
  assert.equal(panel.lookup('account-sort').value, 'five-remaining');
  assert.equal(panel.lookup('account-layout').value, 'compact');
  assert.equal(panel.lookup('account-list').children[0].dataset.accountId, 'low');
  panel.lookup('account-sort').value = 'id'; panel.lookup('account-sort').listeners.get('change')();
  assert.equal(panel.lookup('account-list').children[0].dataset.accountId, 'full');
  assert.equal(panel.requests.filter(request => request.options.method === 'POST').length, 0);
  assert.equal(panel.lookup('auto-refresh').checked, false);
});
test('refresh preferences survive reopening and reconnecting without storing secrets or sending operations', async () => {
  const entries = new Map([['unrelated-host-setting', 'unchanged']]);
  const localStorage = {getItem: name => entries.get(name) ?? null, setItem: (name, value) => entries.set(name, value)};
  const fixture = () => createPanelFixture(undefined, undefined, [], {localStorage});
  async function login(panel) {
    panel.lookup('management-key').value = 'private-test-key';
    panel.lookup('login-form').listeners.get('submit')({preventDefault() {}});
    await panel.settle();
  }
  const first = fixture(); await login(first);
  assert.equal(first.timers.size, 0);
  for (const id of ['auto-refresh', 'background-refresh']) {
    first.lookup(id).checked = true; first.lookup(id).listeners.get('change')();
  }
  await first.settle();
  const saved = [...entries].find(([key]) => key.startsWith('codex-window-refresh:panel-preferences:'));
  assert.deepEqual(JSON.parse(saved[1]), {version: 1, autoRefresh: true, backgroundRefresh: true});
  assert.equal(entries.size, 2);
  assert.equal(entries.get('unrelated-host-setting'), 'unchanged');
  assert.ok(!JSON.stringify([...entries]).includes('private-test-key'));
  first.lookup('disconnect').listeners.get('click')();
  assert.equal(first.timers.size, 0);
  assert.equal(first.lookup('auto-refresh').checked, false);
  assert.equal(entries.get(saved[0]), saved[1]);
  await login(first);
  assert.equal(first.lookup('auto-refresh').checked, true);
  assert.equal(first.lookup('background-refresh').checked, true);
  assert.equal(first.timers.size, 1);
  first.events.get('pagehide')();
  const reopened = fixture();
  assert.equal(reopened.timers.size, 0);
  assert.equal(reopened.requests.length, 0, 'Preferences must wait for authentication');
  await login(reopened);
  assert.equal(reopened.lookup('auto-refresh').checked, true);
  assert.equal(reopened.lookup('background-refresh').checked, true);
  assert.equal([...reopened.timers.values()][0].duration, 5000);
  assert.ok(reopened.requests.every(request => request.options.method === 'GET'));
  reopened.lookup('auto-refresh').checked = false; reopened.lookup('auto-refresh').listeners.get('change')();
  assert.equal(reopened.timers.size, 0);
  assert.deepEqual(JSON.parse(entries.get(saved[0])), {version: 1, autoRefresh: false, backgroundRefresh: false});
  const disabled = fixture(); await login(disabled);
  assert.equal(disabled.lookup('auto-refresh').checked, false);
  assert.equal(disabled.lookup('background-refresh').checked, false);
  assert.equal(disabled.timers.size, 0);
});
test('damaged, blocked and unavailable preference storage falls back safely and proxy mounts are isolated', () => {
  const base = '/v0/management/codex-window-refresh';
  const defaults = {autoRefresh: false, backgroundRefresh: false};
  for (const raw of [null, '{broken', 'null', '[]', '{"version":2,"autoRefresh":true,"backgroundRefresh":true}', '{"version":1,"autoRefresh":"true","backgroundRefresh":true}']) {
    assert.deepEqual(createPanelPreferences({localStorage: {getItem: () => raw}}, base).value(), defaults);
  }
  const blocked = {};
  Object.defineProperty(blocked, 'localStorage', {get() {throw new Error('storage denied');}});
  for (const context of [{}, blocked, {localStorage: {getItem() {throw new Error('blocked');}, setItem() {throw new Error('full');}}}]) {
    const preferences = createPanelPreferences(context, base);
    assert.deepEqual(preferences.value(), defaults);
    assert.doesNotThrow(() => preferences.save(true, true));
    assert.deepEqual(preferences.value(), {autoRefresh: true, backgroundRefresh: true});
  }
  const entries = new Map(), context = {localStorage: {getItem: name => entries.get(name) ?? null, setItem: (name, value) => entries.set(name, value)}};
  createPanelPreferences(context, base).save(true, true);
  assert.deepEqual(createPanelPreferences(context, '/other' + base).value(), defaults);
  assert.deepEqual(createPanelPreferences(context, base).value(), {autoRefresh: true, backgroundRefresh: true});
});
test('authentication failure stops restored polling without erasing browser preferences', async () => {
  const saved = JSON.stringify({version: 1, autoRefresh: true, backgroundRefresh: true});
  let rejected = false;
  const panel = createPanelFixture((url, options, data) => response(data[url.split('/').pop()], rejected ? 401 : 200), undefined, [], {
    localStorage: {getItem: name => name.startsWith('codex-window-refresh:panel-preferences:') ? saved : null, setItem() {assert.fail('Authentication must not write preferences');}}
  });
  panel.lookup('management-key').value = 'key'; panel.lookup('login-form').listeners.get('submit')({preventDefault() {}});
  await panel.settle();
  assert.equal(panel.lookup('auto-refresh').checked, true);
  rejected = true; await panel.poll();
  assert.equal(panel.lookup('workspace').hidden, true);
  assert.equal(panel.timers.size, 0);
  assert.equal(panel.lookup('auto-refresh').checked, false);
  const count = panel.requests.length;
  panel.events.get('online')(); panel.events.get('focus')(); await panel.settle();
  assert.equal(panel.requests.length, count);
});
test('account cards require confirmation for exact manual wake and never animate an unstarted window', async () => {
  let confirmed = false;
  const confirmations = [], now = new Date().toISOString();
  const {lookup, requests, settle} = createPanelFixture(async (url, options, data) => {
    data.capabilities.StableAccountIdentity = data.capabilities.ExactAccountExecution = true;
    data.accounts = {'account-a': {ID: 'account-a', Enabled: true, FiveHour: {Kind: 'five_hour', State: 'unknown', ResetAt: new Date(Date.now() + 5*3600000).toISOString(), Remaining: {Value: 100, Unit: 'percent'}, ConfirmedAt: now, Source: 'cpa_wham_usage'}}};
    return response(data[url.split('/').pop()] || {status: 'queued'}, options.method === 'POST' ? 202 : 200);
  }, message => {confirmations.push(message); return confirmed;});
  function descendants(item) {return [item, ...item.children.flatMap(descendants)];}
  lookup('management-key').value = 'test-key';
  lookup('login-form').listeners.get('submit')({preventDefault() {}});
  await settle();
  assert.equal(lookup('workspace').hidden, false);
  const items = descendants(lookup('account-list'));
  const wakeButton = items.find(item => item.dataset.wakeAccount === 'account-a');
  assert.ok(wakeButton);
  assert.equal(wakeButton.disabled, false);
  assert.equal(items.some(item => Object.hasOwn(item.dataset, 'reset')), false);
  assert.ok(items.some(item => item.textContent.includes('不倒计时')));
  wakeButton.listeners.get('click')();
  await settle();
  assert.equal(requests.some(request => request.options.method === 'POST'), false);
  confirmed = true;
  wakeButton.listeners.get('click')();
  await settle();
  const writes = requests.filter(request => request.options.method === 'POST');
  assert.equal(writes.length, 1);
  assert.ok(writes[0].url.endsWith('/manual-wake'));
  assert.equal(writes[0].options.body, JSON.stringify({account_id: 'account-a'}));
  assert.match(confirmations[0], /真实模型请求并消耗额度/);
  lookup('disconnect').listeners.get('click')();
});
test('unavailable account allows quota query and manual wake recheck', async () => {
  const {lookup, requests, settle} = createPanelFixture(async (url, options, data) => {
    data.capabilities.StableAccountIdentity = data.capabilities.ExactAccountExecution = true;
    data.accounts = {'unavailable-account': {ID: 'unavailable-account', Enabled: false, Availability: 'unavailable', Permission: 'blocked', FiveHour: {State: 'expired', Remaining: {Value: 0, Unit: 'percent'}}}};
    if (options.method === 'POST' && url.endsWith('/quota-query')) data.summary.scheduler.quota_query = {account_id: 'unavailable-account', phase: 'completed', completed: 1};
    if (options.method === 'POST' && url.endsWith('/manual-wake')) data.summary.scheduler.manual_wake = {account_id: 'unavailable-account', phase: 'blocked', last_error: 'account_unavailable'};
    return response(data[url.split('/').pop()] || {status: 'queued'}, options.method === 'POST' ? 202 : 200);
  });
  lookup('management-key').value = 'test-key'; lookup('login-form').listeners.get('submit')({preventDefault() {}});
  await settle();
  const descendants = item => [item, ...item.children.flatMap(descendants)];
  const items = descendants(lookup('account-list'));
  const query = items.find(item => item.dataset.quotaAccount === 'unavailable-account');
  const wake = items.find(item => item.dataset.wakeAccount === 'unavailable-account');
  assert.ok(query); assert.ok(wake);
  assert.equal(query.disabled, false);
  assert.equal(wake.disabled, false);
  assert.ok(items.some(item => item.textContent === 'CPA 账号暂时不可用'));
  query.listeners.get('click')();
  await settle();
  const writes = requests.filter(request => request.options.method === 'POST');
  assert.equal(writes.length, 1);
  assert.ok(writes[0].url.endsWith('/quota-query'));
  assert.deepEqual(JSON.parse(writes[0].options.body), {account_id: 'unavailable-account'});
  const refreshedWake = descendants(lookup('account-list')).find(item => item.dataset.wakeAccount === 'unavailable-account');
  assert.equal(refreshedWake.disabled, false, 'successful quota query must leave wake recheck available');
  refreshedWake.listeners.get('click')();
  await settle();
  const wakeWrites = requests.filter(request => request.options.method === 'POST' && request.url.endsWith('/manual-wake'));
  assert.equal(wakeWrites.length, 1);
  assert.deepEqual(JSON.parse(wakeWrites[0].options.body), {account_id: 'unavailable-account'});
  assert.ok(descendants(lookup('account-list')).some(item => item.textContent.includes('CPA 仍报告账号暂时不可用')));
  lookup('disconnect').listeners.get('click')();
});
test('granular settings can be added to legacy drafts without enabling other features', () => {
  const editor = createEditor(), original = policy();
  editor.load({config: original, revision: 'r1'}, true);
  editor.update('quota_query.on_five_hour_expiry', true);
  editor.update('quota_query.on_weekly_expiry', false);
  editor.update('planning.enabled', true); editor.update('planning.horizon_days', 3);
  editor.update('reset_rules.five_hour.model', 'five-model'); editor.update('reset_rules.weekly.prompt', 'Weekly content');
  editor.update('ordinary_rules.daily_enabled', false);
  const settings = editor.value();
  assert.equal(settings.enabled, false);
  assert.equal(settings.quota_query.automatic, false);
  assert.equal(settings.quota_query.manual, true);
  assert.equal(settings.quota_query.on_five_hour_expiry, true);
  assert.deepEqual(settings.planning, {enabled: true, horizon_days: 3});
  assert.deepEqual(settings.execution, original.execution);
  assert.equal(settings.reset_rules.five_hour.model, 'five-model');
  assert.equal(settings.ordinary_rules.daily_enabled, false);
  assert.throws(() => editor.update('planning.arbitrary', true), {code: 'invalid_config'});
  assert.throws(() => editor.replace({...settings, planning: {enabled: true, horizon_days: 31}}), {code: 'invalid_config'});
  assert.throws(() => editor.replace({...settings, quota_query: {...settings.quota_query, on_weekly_expiry: 'true'}}), {code: 'invalid_config'});
});
test('daily point and range controls serialize valid wall-clock rules', () => {
  assert.equal(dailyTimeRange('point', '09:00', 'invalid'), '09:00-09:00');
  assert.equal(dailyTimeRange('range', '09:00', '09:10'), '09:00-09:10');
  assert.equal(dailyTimeRange('range', '22:00', '24:00'), '22:00-24:00');
  for (const [mode, start, end] of [['range', '22:00', '02:00'], ['point', '24:00', '24:00'], ['range', '09:00', '09:00'], ['range', '9:00', '09:10'], ['point', '09:60', '09:60']]) {
    assert.throws(() => dailyTimeRange(mode, start, end), {code: 'invalid_config'});
  }
});
test('independent query policy fields scaffold old configurations without enabling requests', () => {
  const editor = createEditor();
  editor.load({config: policy(), revision: 'query-policy'});
  assert.equal(editor.dirty(), false);
  editor.update('quota_query.five_hour.expiry.excluded_hours', ['22:00-08:00']);
  editor.update('quota_query.five_hour.expiry.random_delay.min', '1m');
  editor.update('quota_query.five_hour.expiry.random_delay.max', '5m');
  editor.update('quota_query.weekly.periodic_enabled', false);
  editor.update('quota_query.weekly.periodic.random_delay.max', '2m');
  const draft = editor.value();
  assert.equal(draft.quota_query.automatic, false);
  assert.equal(draft.quota_query.on_five_hour_expiry, false);
  assert.equal(draft.quota_query.weekly.periodic_enabled, false);
  assert.deepEqual(draft.quota_query.five_hour.expiry.excluded_hours, ['22:00-08:00']);
  assert.deepEqual(draft.quota_query.five_hour.expiry.random_delay, {min: '1m', max: '5m'});
  editor.replace(draft);
  for (const invalid of [null, '22:00-08:00', [false]]) {
    const config = structuredClone(draft);
    config.quota_query.five_hour.expiry.excluded_hours = invalid;
    assert.throws(() => editor.replace(config), {code: 'invalid_config'});
  }
  assert.throws(() => editor.update('quota_query.weekly.unrecognized.value', true), {code: 'invalid_config'});
});
test('query policy form parses excluded hours and saves separate windows as one draft', async () => {
  const fields = [{id: 'five-hours', field: 'quota_query.five_hour.expiry.excluded_hours'}, {id: 'week-min', field: 'quota_query.weekly.periodic.random_delay.min'}];
  const {lookup, requests, settle} = createPanelFixture(async (url, options, data) => {
    if (url.endsWith('/config') && options.method === 'POST') {
      const saved = JSON.parse(options.body);
      data.config = saved.config;
      return response({config: saved.config, revision: 'r2'});
    }
    return response(data[url.split('/').pop()]);
  }, () => true, fields);
  lookup('management-key').value = 'test-key'; lookup('login-form').listeners.get('submit')({preventDefault() {}});
  await settle();
  assert.equal(lookup('five-hours').value, '');
  assert.equal(lookup('week-min').value, '0s');
  lookup('five-hours').value = '22:00-08:00，12:00-13:00\n';
  lookup('five-hours').listeners.get('change')();
  lookup('week-min').value = '0s'; lookup('week-min').listeners.get('change')();
  lookup('config-save').listeners.get('click')();
  await settle();
  const writes = requests.filter(request => request.options.method === 'POST');
  assert.equal(writes.length, 1);
  const saved = JSON.parse(writes[0].options.body).config;
  assert.deepEqual(saved.quota_query.five_hour.expiry.excluded_hours, ['22:00-08:00', '12:00-13:00']);
  assert.equal(saved.quota_query.weekly.periodic.random_delay.min, '0s');
  assert.equal(saved.quota_query.automatic, false);
  lookup('disconnect').listeners.get('click')();
});
test('next wake table shows one row per account and excludes queries and pool events', async () => {
  const {lookup, requests, settle} = createPanelFixture(async (url, options, data) => {
    data.config.timezone = 'Asia/Shanghai'; data.config.enabled = true;
    data.accounts = Object.fromEntries(['a', 'b', 'unknown'].map(id => [id, {ID: id, Enabled: true}]));
    data.summary.scheduler.plan = {enabled: true, horizon_days: 3, entries: [
      {kind: 'wake', account_id: 'a', earliest_at: '2026-10-15T16:00:00Z', status: 'scheduled'},
      {kind: 'wake', account_id: 'a', earliest_at: '2026-10-14T16:00:00Z', latest_at: '2026-10-14T16:10:00Z', status: 'estimated'},
      {kind: 'wake', account_id: 'b', earliest_at: '2026-10-13T16:00:00Z', status: 'scheduled'},
      {kind: 'wake', account_id: 'unknown', earliest_at: '2026-10-12T16:00:00Z', status: 'blocked'},
      {kind: 'wake', trigger: 'daily_schedule', earliest_at: '2026-10-12T16:00:00Z', status: 'scheduled'},
      {kind: 'query', account_id: 'a', earliest_at: '2026-10-12T16:00:00Z', status: 'scheduled'}
    ]};
    return response(data[url.split('/').pop()]);
  });
  lookup('management-key').value = 'test-key'; lookup('login-form').listeners.get('submit')({preventDefault() {}});
  await settle();
  const rows = lookup('plan-list').children;
  assert.equal(rows.length, 3);
  assert.deepEqual(rows.map(row => row.children[0].textContent), ['b', 'a', 'unknown']);
  assert.match(rows[0].children[1].textContent, /2026\/10\/14/);
  assert.match(rows[1].children[1].textContent, /预计.*2026\/10\/15.*—/);
  assert.equal(rows[2].children[1].textContent, '暂无可确定时间');
  assert.ok(rows.every(row => row.children.length === 2));
  assert.equal(requests.some(request => request.options.method === 'POST'), false);
  lookup('disconnect').listeners.get('click')();
});
test('next wake summary works without full planning and renders account names safely', async () => {
  const id = '<img src=x onerror=alert(1)>', now = new Date().toISOString();
  const {lookup, requests, settle} = createPanelFixture(async (url, options, data) => {
    data.config.enabled = true;
    data.accounts = {[id]: {ID: id, Enabled: true}, disabled: {ID: 'disabled', Enabled: false}, pending: {ID: 'pending', Enabled: true, Task: {OperationID: 'op'}}, paused: {ID: 'paused', Enabled: true}};
    data.config.account_overrides = {paused: {paused: true}};
    data.summary.scheduler.plan = {enabled: false, horizon_days: 3, entries: [], next_wakes: {[id]: {kind: 'wake', account_id: id, earliest_at: now, latest_at: now, status: 'scheduled'}}};
    return response(data[url.split('/').pop()]);
  });
  lookup('management-key').value = 'test-key'; lookup('login-form').listeners.get('submit')({preventDefault() {}});
  await settle();
  const rows = lookup('plan-list').children;
  assert.equal(rows.length, 4);
  assert.equal(rows[0].children[0].textContent, id);
  assert.equal(rows[0].children[0].children.length, 0);
  const values = Object.fromEntries(rows.map(row => row.children.map(cell => cell.textContent)));
  assert.equal(values.disabled, 'CPA 账号不可用');
  assert.equal(values.pending, '等待上次唤醒验证');
  assert.equal(values.paused, '已暂停唤醒');
  assert.match(lookup('plan-status').textContent, /未来 3 天/);
  assert.equal(requests.some(request => request.options.method === 'POST'), false);
  lookup('disconnect').listeners.get('click')();
});
test('daily UI switches between a point and a random range without removing the rule', async () => {
  const {lookup, requests, settle} = createPanelFixture(async (url, options, data) => {
    data.config.daily_rules = [{id: 'point', enabled: true, random_time_range: '09:00-09:00', minimum_ready_accounts: 1, catch_up: true}];
    return response(data[url.split('/').pop()]);
  });
  lookup('management-key').value = 'test-key'; lookup('login-form').listeners.get('submit')({preventDefault() {}});
  await settle();
  assert.equal(lookup('workspace').hidden, false);
  const row = lookup('daily-rules').children[0];
  const times = row.children.find(item => item.className === 'form-grid daily-times');
  const mode = times.children[0].children[0], start = times.children[1].children[0], end = times.children[2].children[0];
  assert.equal(mode.value, 'point'); assert.equal(end.disabled, true);
  mode.value = 'range'; mode.listeners.get('change')();
  assert.equal(end.disabled, false);
  assert.equal(JSON.parse(lookup('config-json').value).daily_rules[0].random_time_range, '09:00-09:10');
  mode.value = 'point'; mode.listeners.get('change')();
  start.value = '10:30'; start.listeners.get('change')();
  assert.equal(JSON.parse(lookup('config-json').value).daily_rules[0].random_time_range, '10:30-10:30');
  assert.equal(requests.some(request => request.options.method === 'POST'), false);
  lookup('disconnect').listeners.get('click')();
});
test('page polling requires opt-in and legacy manual permission does not block queries', async () => {
  const {lookup, timers, visibility, requests, settle} = createPanelFixture();
  lookup('management-key').value = 'test-key';
  lookup('login-form').listeners.get('submit')({preventDefault() {}});
  await settle();
  assert.equal(lookup('workspace').hidden, false);
  assert.equal(lookup('quota-query').disabled, false);
  assert.equal(timers.size, 0);
  lookup('auto-refresh').checked = true;
  lookup('auto-refresh').listeners.get('change')();
  assert.equal(timers.size, 1);
  assert.equal([...timers.values()][0].duration, 5000);
  const poll = [...timers.values()][0].callback;
  lookup('auto-refresh').checked = false;
  lookup('auto-refresh').listeners.get('change')();
  assert.equal(timers.size, 0);
  const before = requests.length;
  await poll();
  visibility.get('visibilitychange')();
  await settle();
  assert.equal(requests.length, before);
  lookup('refresh').listeners.get('click')();
  await settle();
  assert.ok(requests.length > before);
  assert.equal(timers.size, 0);
  lookup('quota-query').listeners.get('click')();
  await settle();
  assert.equal(requests.filter(request => request.options.method === 'POST').length, 1);
  assert.ok(requests.some(request => request.url.endsWith('/quota-query') && request.options.method === 'POST'));
  lookup('auto-refresh').checked = true;
  lookup('auto-refresh').listeners.get('change')();
  lookup('disconnect').listeners.get('click')();
  assert.equal(lookup('auto-refresh').checked, false);
  assert.equal(timers.size, 0);
});
test('manual queries track delayed snapshots through completion with page polling off', async () => {
  for (const target of ['', 'account-a']) {
    for (const failed of [false, true]) {
      let phase = 'previous';
      const previousStart = '2026-10-13T00:00:00Z';
      const {lookup, timers, requests, settle, poll} = createPanelFixture(async (url, options, data) => {
        data.accounts = {'account-a': {ID: 'account-a', Enabled: true, FiveHour: {Remaining: {Value: phase === 'completed' ? 73 : 80, Unit: 'percent'}}}};
        data.summary.scheduler.quota_query = {account_id: target, phase: phase === 'previous' ? 'completed' : phase, started_at: phase === 'previous' ? previousStart : '2026-10-14T00:00:00Z', total: 1, completed: phase === 'completed' && !failed ? 1 : 0, failed: phase === 'completed' && failed ? 1 : 0, last_error: failed ? 'quota_credential_expired' : ''};
        if (options.method === 'POST') return response({status: 'queued'}, 202);
        return response(data[url.split('/').pop()]);
      });
      lookup('management-key').value = 'test-key'; lookup('login-form').listeners.get('submit')({preventDefault() {}});
      await settle();
      assert.equal(timers.size, 0);
      lookup('config-json').value = '{unsaved-query-draft'; lookup('config-json').listeners.get('input')();
      const descendants = item => [item, ...item.children.flatMap(descendants)];
      if (target) descendants(lookup('account-list')).find(item => item.dataset.quotaAccount === target).listeners.get('click')();
      else lookup('quota-query').listeners.get('click')();
      await settle();
      assert.equal(lookup('auto-refresh').checked, false);
      assert.equal(lookup('quota-query').disabled, true);
      assert.match(lookup('quota-query-status').textContent, /等待查询/);
      assert.match(lookup('refresh-status').textContent, /手动任务进行中/);
      assert.equal(timers.size, 1);
      assert.equal([...timers.values()][0].duration, 500, 'Manual queries must not wait for the idle five-second refresh');
      await poll(); // The runtime has not published this request yet.
      assert.match(lookup('quota-query-status').textContent, /等待查询/);
      phase = 'running'; await poll();
      assert.match(lookup('quota-query-status').textContent, /正在查询/);
      phase = 'completed'; await poll();
      assert.match(lookup('quota-query-status').textContent, failed ? /失败 1/ : /成功 1/);
      assert.match(lookup('quota-query-status').textContent, /查询结束/);
      assert.equal(lookup('quota-query').disabled, false);
      const cards = descendants(lookup('account-list'));
      assert.ok(cards.some(item => item.textContent.includes('73%')));
      if (target) assert.ok(cards.some(item => item.textContent.includes(failed ? '查询失败，账号凭据已过期' : '查询完成')));
      assert.equal(lookup('config-json').value, '{unsaved-query-draft');
      assert.equal(lookup('auto-refresh').checked, false);
      assert.equal(timers.size, 0, 'query tracking must stop at completion');
      assert.equal(requests.filter(request => request.options.method === 'POST').length, 1);
      lookup('disconnect').listeners.get('click')();
    }
  }
});
test('accepted manual queries recover from snapshot failures without resubmitting', async () => {
  let accepted = false, failing = false, completed = false;
  const {lookup, timers, requests, settle, poll} = createPanelFixture(async (url, options, data) => {
    if (options.method === 'POST') { accepted = failing = true; return response({status: 'queued'}, 202); }
    if (accepted && failing && url.endsWith('/snapshot')) throw new Error('temporary read failure');
    if (completed) data.summary.scheduler.quota_query = {phase: 'completed', started_at: '2026-10-14T00:00:00Z', total: 1, completed: 1};
    return response(data[url.split('/').pop()]);
  });
  lookup('management-key').value = 'test-key'; lookup('login-form').listeners.get('submit')({preventDefault() {}});
  await settle();
  lookup('quota-query').listeners.get('click')(); await settle();
  assert.match(lookup('notice').textContent, /无法读取 CPA/);
  assert.equal(lookup('quota-query').disabled, true);
  await poll();
  assert.equal([...timers.values()][0].duration, 1000);
  failing = false; completed = true; await poll();
  assert.match(lookup('quota-query-status').textContent, /查询结束/);
  assert.equal(lookup('notice').hidden, true);
  assert.equal(timers.size, 0);
  assert.equal(requests.filter(request => request.options.method === 'POST').length, 1);
  lookup('disconnect').listeners.get('click')();
});
test('manual query tracking stops on terminal states or a stopped scheduler', async () => {
  for (const phase of ['cancelled', 'unavailable', 'stopped']) {
    let finish = false;
    const {lookup, timers, settle, poll} = createPanelFixture(async (url, options, data) => {
      if (finish) {
        data.summary.running = phase !== 'stopped';
        data.summary.scheduler.quota_query = {phase: phase === 'stopped' ? 'queued' : phase, started_at: '2026-10-14T00:00:00Z'};
      }
      return response(data[url.split('/').pop()] || {status: 'queued'}, options.method === 'POST' ? 202 : 200);
    });
    lookup('management-key').value = 'test-key'; lookup('login-form').listeners.get('submit')({preventDefault() {}});
    await settle(); lookup('quota-query').listeners.get('click')(); await settle();
    assert.equal(timers.size, 1);
    finish = true; await poll();
    assert.equal(timers.size, 0);
    assert.match(lookup('quota-query-status').textContent, phase === 'stopped' ? /后台未运行/ : phase === 'cancelled' ? /已取消/ : /额度查询接口未就绪/);
    lookup('disconnect').listeners.get('click')();
  }
});
test('manual query tracking pauses while hidden or offline and resumes without page polling', async () => {
  const {lookup, timers, requests, settle, document, navigator, visibility, events} = createPanelFixture();
  lookup('management-key').value = 'test-key'; lookup('login-form').listeners.get('submit')({preventDefault() {}});
  await settle();
  lookup('quota-query').listeners.get('click')(); await settle();
  document.hidden = true; visibility.get('visibilitychange')();
  assert.equal(timers.size, 0);
  const hidden = requests.length;
  events.get('focus')(); await settle(); assert.equal(requests.length, hidden);
  document.hidden = false; visibility.get('visibilitychange')(); await settle();
  assert.ok(requests.length > hidden);
  assert.equal(timers.size, 1);
  navigator.onLine = false; events.get('offline')();
  assert.equal(timers.size, 0);
  const offline = requests.length;
  events.get('focus')(); await settle(); assert.equal(requests.length, offline);
  navigator.onLine = true; events.get('online')(); await settle();
  assert.ok(requests.length > offline);
  assert.equal(lookup('auto-refresh').checked, false);
  assert.equal(requests.filter(request => request.options.method === 'POST').length, 1);
  lookup('disconnect').listeners.get('click')();
  assert.equal(timers.size, 0);
  events.get('focus')(); await settle(); assert.equal(timers.size, 0);
});
test('rejected manual queries and authentication failures stop query tracking', async () => {
  for (const rejected of [true, false]) {
    let accepted = false;
    const {lookup, timers, requests, settle} = createPanelFixture(async (url, options, data) => {
      if (options.method === 'POST') { accepted = !rejected; return response({}, rejected ? 409 : 202); }
      return response(data[url.split('/').pop()], accepted ? 401 : 200);
    });
    lookup('management-key').value = 'test-key'; lookup('login-form').listeners.get('submit')({preventDefault() {}});
    await settle();
    lookup('quota-query').listeners.get('click')(); await settle();
    assert.equal(timers.size, 0);
    assert.match(lookup('notice').textContent, rejected ? /未能安排额度查询/ : /认证失败/);
    assert.equal(requests.filter(request => request.options.method === 'POST').length, 1);
    lookup('disconnect').listeners.get('click')();
  }
});
test('long-running polling backs off failures and returns to five seconds without accumulating requests', async () => {
  let failing = false;
  const {lookup, timers, settle, poll, requests} = createPanelFixture(async (url, options, data) => {
    if (failing && url.endsWith('/snapshot')) throw new Error('temporary network failure');
    return response(data[url.split('/').pop()]);
  });
  lookup('management-key').value = 'test-key'; lookup('login-form').listeners.get('submit')({preventDefault() {}});
  await settle();
  lookup('auto-refresh').checked = true; lookup('auto-refresh').listeners.get('change')();
  failing = true;
  for (const expected of [10000, 20000, 40000, 60000, 60000]) {
    await poll();
    assert.equal(timers.size, 1);
    assert.equal([...timers.values()][0].duration, expected);
    assert.match(lookup('refresh-status').textContent, /自动重试/);
  }
  failing = false;
  for (let cycle = 0; cycle < 100; cycle++) {
    await poll();
    assert.equal(timers.size, 1);
    assert.equal([...timers.values()][0].duration, 5000);
    assert.equal(lookup('notice').hidden, true);
  }
  assert.equal(requests.filter(request => request.options.method === 'POST').length, 0);
  lookup('disconnect').listeners.get('click')();
  assert.equal(timers.size, 0);
});
test('active task polling slows after ten seconds and returns to idle cadence after completion', async () => {
  let completed = false;
  const fixture = createPanelFixture(async (url, options, data) => {
    data.summary.scheduler.jobs = [trackedEnvelope('active-at-connect', 'quota_query', completed ? 'completed' : 'running')];
    return response(data[url.split('/').pop()]);
  });
  await loginFixture(fixture);
  for (let cycle = 0; cycle < 20; cycle++) {
    assert.equal(fixture.timers.size, 1);
    assert.equal([...fixture.timers.values()][0].duration, 500);
    await fixture.poll();
  }
  for (let cycle = 0; cycle < 5; cycle++) {
    assert.equal([...fixture.timers.values()][0].duration, 1000);
    await fixture.poll();
  }
  fixture.lookup('auto-refresh').checked = true; fixture.lookup('auto-refresh').listeners.get('change')();
  assert.equal([...fixture.timers.values()][0].duration, 1000, 'Enabling idle refresh must not restart a long task burst');
  completed = true; await fixture.poll();
  assert.equal([...fixture.timers.values()][0].duration, 5000);
  assert.equal(fixture.requests.filter(request => request.options.method === 'POST').length, 0);
  fixture.lookup('disconnect').listeners.get('click')();
});
test('active task read failures back off to sixty seconds without resubmitting and reset after recovery', async () => {
  let failing = false;
  const fixture = createPanelFixture(async (url, options, data) => {
    if (failing && url.endsWith('/snapshot')) throw new Error('temporary network failure');
    data.summary.scheduler.jobs = [trackedEnvelope('slow-task', 'quota_query', 'running')];
    return response(data[url.split('/').pop()]);
  });
  await loginFixture(fixture); failing = true;
  for (const expected of [1000, 2000, 4000, 8000, 16000, 32000, 60000, 60000]) {
    await fixture.poll();
    assert.equal(fixture.timers.size, 1);
    assert.equal([...fixture.timers.values()][0].duration, expected);
  }
  failing = false; await fixture.poll();
  assert.equal([...fixture.timers.values()][0].duration, 1000);
  assert.equal(fixture.lookup('notice').hidden, true);
  assert.equal(fixture.requests.filter(request => request.options.method === 'POST').length, 0);
  fixture.lookup('disconnect').listeners.get('click')();
});
test('completed quota jobs show optional total duration and support older timestamp-only jobs', async () => {
  for (const timing of [{total_ms: 1234}, undefined, {total_ms: -10}]) {
    const fixture = createPanelFixture(async (url, options, data) => {
      data.summary.scheduler.jobs = [trackedEnvelope('timed', 'quota_query', 'completed', {timing, started_at: '2026-10-14T00:00:00Z', finished_at: '2026-10-14T00:00:02Z'})];
      return response(data[url.split('/').pop()]);
    });
    await loginFixture(fixture);
    assert.match(fixture.lookup('quota-query-status').textContent, timing?.total_ms === 1234 ? /耗时 1\.2 秒/ : /耗时 2 秒/);
    assert.equal(fixture.timers.size, 0);
    fixture.lookup('disconnect').listeners.get('click')();
  }
});
test('background refresh is explicit and resumes after tab changes and offline periods', async () => {
  const {lookup, timers, settle, poll, requests, document, navigator, visibility, events} = createPanelFixture();
  lookup('management-key').value = 'test-key'; lookup('login-form').listeners.get('submit')({preventDefault() {}});
  await settle();
  assert.equal(lookup('background-refresh').checked, false);
  assert.equal(lookup('background-refresh').disabled, true);
  lookup('auto-refresh').checked = true; lookup('auto-refresh').listeners.get('change')();
  document.hidden = true; visibility.get('visibilitychange')();
  assert.equal(timers.size, 0);
  assert.match(lookup('refresh-status').textContent, /标签页隐藏/);
  const paused = requests.length;
  events.get('focus')(); await settle();
  assert.equal(requests.length, paused);
  lookup('background-refresh').checked = true; lookup('background-refresh').listeners.get('change')();
  await settle(); await poll();
  assert.ok(requests.length > paused);
  assert.equal(timers.size, 1);
  assert.match(lookup('refresh-status').textContent, /含后台标签页/);
  navigator.onLine = false; events.get('offline')();
  assert.equal(timers.size, 0);
  assert.match(lookup('refresh-status').textContent, /网络离线/);
  const offline = requests.length;
  events.get('focus')(); await settle();
  assert.equal(requests.length, offline);
  navigator.onLine = true; events.get('online')(); await settle();
  assert.ok(requests.length > offline);
  assert.equal(timers.size, 1);
  lookup('auto-refresh').checked = false; lookup('auto-refresh').listeners.get('change')();
  assert.equal(lookup('background-refresh').checked, false);
  assert.equal(lookup('background-refresh').disabled, true);
  assert.equal(timers.size, 0);
  const disabled = requests.length;
  document.hidden = false; visibility.get('visibilitychange')(); events.get('online')(); events.get('focus')();
  await settle(); assert.equal(requests.length, disabled);
  lookup('disconnect').listeners.get('click')();
});
test('pausing an in-flight poll cancels only its reads and does not render late data', async () => {
  let pending = false, lateRead;
  const {lookup, timers, settle, poll, requests, document, visibility, events} = createPanelFixture(async (url, options, data) => {
    if (pending && url.endsWith('/snapshot')) {
      return new Promise(resolve => {lateRead = () => resolve(response({...data.snapshot, accounts: {late: {ID: 'late', Enabled: true}}}));});
    }
    return response(data[url.split('/').pop()]);
  });
  lookup('management-key').value = 'test-key'; lookup('login-form').listeners.get('submit')({preventDefault() {}});
  await settle();
  lookup('auto-refresh').checked = true; lookup('auto-refresh').listeners.get('change')();
  pending = true; await poll();
  const outstanding = requests.at(-1), count = requests.length;
  events.get('focus')(); events.get('online')();
  assert.equal(requests.length, count, 'parallel wake-up events duplicated an in-flight refresh');
  document.hidden = true; visibility.get('visibilitychange')();
  assert.equal(outstanding.options.signal.aborted, true);
  assert.equal(timers.size, 1, 'the pending transport retains only its timeout until it settles');
  lateRead(); await settle();
  assert.equal(timers.size, 0);
  assert.equal(lookup('notice').hidden, true);
  assert.equal(lookup('account-count').textContent, '显示 0 / 0 个账号');
  pending = false; document.hidden = false; visibility.get('visibilitychange')(); await settle();
  assert.equal(timers.size, 1);
  assert.equal(lookup('refresh').disabled, false);
  lookup('disconnect').listeners.get('click')();
});
test('polling updates applied configuration while preserving unsaved JSON and rule drafts', async () => {
  let newConfig = false;
  const {lookup, settle, poll} = createPanelFixture(async (url, options, data) => {
    if (newConfig) data.config.quota_query.on_weekly_expiry = true;
    return response(data[url.split('/').pop()]);
  });
  lookup('management-key').value = 'test-key'; lookup('login-form').listeners.get('submit')({preventDefault() {}});
  await settle();
  lookup('config-json').value = '{unsaved-json'; lookup('config-json').listeners.get('input')();
  lookup('auto-refresh').checked = true; lookup('auto-refresh').listeners.get('change')();
  newConfig = true; await poll();
  assert.equal(lookup('config-json').value, '{unsaved-json');
  assert.match(lookup('quota-query-status').textContent, /周到期查询：开启/);
  assert.equal(lookup('save-bar').hidden, false);
});
test('authenticated polling stops on rejected credentials rather than repeatedly retrying', async () => {
  let rejected = false;
  const {lookup, timers, settle, poll, requests, events} = createPanelFixture(async (url, options, data) => response(data[url.split('/').pop()], rejected ? 401 : 200));
  lookup('management-key').value = 'test-key'; lookup('login-form').listeners.get('submit')({preventDefault() {}});
  await settle();
  lookup('auto-refresh').checked = true; lookup('auto-refresh').listeners.get('change')();
  rejected = true; await poll();
  assert.equal(lookup('workspace').hidden, true);
  assert.match(lookup('notice').textContent, /认证失败/);
  assert.equal(timers.size, 0);
  const stopped = requests.length;
  events.get('online')(); events.get('focus')(); await settle();
  assert.equal(requests.length, stopped);
});
test('successful snapshot reads hide recovered timeout and network errors', async () => {
  for (const failure of ['timeout', 'network']) {
    let failing = false;
    const {lookup, timers, settle, poll} = createPanelFixture(async (url, options, data) => {
      if (failing && url.endsWith('/snapshot')) {
        if (failure === 'network') throw new Error('private network failure');
        return new Promise((_, reject) => options.signal.addEventListener('abort', () => reject(new Error('aborted'))));
      }
      return response(data[url.split('/').pop()]);
    });
    lookup('management-key').value = 'test-key';
    lookup('login-form').listeners.get('submit')({preventDefault() {}});
    await settle();
    lookup('auto-refresh').checked = true;
    lookup('auto-refresh').listeners.get('change')();
    failing = true;
    await poll();
    if (failure === 'timeout') {
      [...timers.values()].find(timer => timer.duration === 15000).callback();
      await settle();
    }
    assert.equal(lookup('notice').hidden, false);
    assert.equal(lookup('notice').className, 'notice error');
    assert.match(lookup('notice').textContent, failure === 'timeout' ? /读取状态超时/ : /无法读取 CPA/);
    failing = false;
    await poll();
    assert.equal(lookup('notice').hidden, true);
    assert.equal(lookup('notice').textContent, '');
    lookup('disconnect').listeners.get('click')();
  }
});
test('snapshot recovery preserves a newer unrelated error', async () => {
  let mode = '', pendingRead;
  const {lookup, settle, poll} = createPanelFixture(async (url, options, data) => {
    if (url.endsWith('/snapshot')) {
      if (mode === 'failed') throw new Error('private network failure');
      if (mode === 'pending') return new Promise(resolve => {pendingRead = () => resolve(response(data.snapshot));});
    }
    return response(data[url.split('/').pop()]);
  });
  lookup('management-key').value = 'test-key';
  lookup('login-form').listeners.get('submit')({preventDefault() {}});
  await settle();
  lookup('auto-refresh').checked = true;
  lookup('auto-refresh').listeners.get('change')();
  mode = 'failed';
  await poll();
  assert.match(lookup('notice').textContent, /无法读取 CPA/);
  mode = 'pending';
  await poll();
  assert.equal(lookup('notice').hidden, false);
  assert.match(lookup('notice').textContent, /无法读取 CPA/);
  lookup('config-json').value = '{invalid-json';
  lookup('json-format').listeners.get('click')();
  const validationError = lookup('notice').textContent;
  assert.match(validationError, /JSON 无法解析/);
  pendingRead();
  await settle();
  assert.equal(lookup('notice').hidden, false);
  assert.equal(lookup('notice').textContent, validationError);
  mode = '';
  await poll();
  assert.equal(lookup('notice').textContent, validationError);
  lookup('disconnect').listeners.get('click')();
});
test('successful snapshot reads do not hide host connection check timeouts', async () => {
  const {lookup, timers, settle, poll} = createPanelFixture(async (url, options, data) => {
    if (url.endsWith('/quota-connection-check')) {
      return new Promise((_, reject) => options.signal.addEventListener('abort', () => reject(new Error('aborted'))));
    }
    return response(data[url.split('/').pop()]);
  });
  lookup('management-key').value = 'test-key';
  lookup('login-form').listeners.get('submit')({preventDefault() {}});
  await settle();
  lookup('auto-refresh').checked = true;
  lookup('auto-refresh').listeners.get('change')();
  lookup('quota-connection-check').listeners.get('click')();
  await settle();
  [...timers.values()].find(timer => timer.duration === 15000).callback();
  await settle();
  assert.match(lookup('notice').textContent, /读取状态超时/);
  await poll();
  assert.equal(lookup('notice').hidden, false);
  assert.match(lookup('notice').textContent, /读取状态超时/);
  lookup('disconnect').listeners.get('click')();
});
test('quota errors show safe stages and distinguish management authentication from upstream rejection', () => {
  const management = quotaFailure({last_error: 'quota_management_auth_failed', diagnostic: {stage: 'management_http', http_status: 403, elapsed_ms: 120}});
  const upstream = quotaFailure({last_error: 'quota_upstream_forbidden', diagnostic: {stage: 'upstream_http', http_status: 403, elapsed_ms: 80}});
  assert.match(management, /后台管理密钥认证失败/); assert.match(management, /管理接口 · HTTP 403 · 120 毫秒/);
  assert.match(upstream, /上游拒绝访问/); assert.match(upstream, /上游响应 · HTTP 403/);
  assert.match(quotaFailure({last_error: 'quota_host_operation_failed'}), /接口兼容性/);
  assert.match(quotaFailure({last_error: 'quota_query_timeout'}), /查询超时/);
  assert.match(quotaFailure({last_error: 'durable_commit_failed'}), /后台状态未能保存/);
  assert.match(quotaFailure({last_error: 'account_inventory_unavailable'}), /账号清单暂不可用/);
  assert.match(quotaFailure({last_error: 'account_read_failed'}), /账号清单读取失败/);
  assert.match(quotaFailure({last_error: 'capability_read_failed'}), /宿主能力读取失败/);
  assert.equal(quotaFailure({last_error: 'private-token', diagnostic: {stage: 'private-proxy', http_status: 999, elapsed_ms: -1}}), '可手动重试');
  assert.equal(quotaFailure({last_error: '__proto__', diagnostic: {stage: 'constructor'}}), '可手动重试');
});
test('cancelling a poll does not cancel or retry a concurrent write', async () => {
  const signals = new Map(), cancellation = new AbortController();
  const session = createSession(async (url, options) => {
    signals.set(url.split('/').pop(), options.signal);
    return new Promise((_, reject) => options.signal.addEventListener('abort', () => reject(new Error('aborted'))));
  }, null, {setTimeout: () => 1, clearTimeout() {}});
  session.setKey('test-key');
  const read = session.request('/accounts', 'GET', undefined, cancellation.signal);
  const write = session.request('/manual-wake', 'POST', {account_id: 'a'});
  cancellation.abort();
  await assert.rejects(read, {code: 'cancelled'});
  assert.equal(signals.get('manual-wake').aborted, false);
  assert.equal(session.connected(), true);
  session.disconnect();
  await assert.rejects(write, {code: 'stale'});
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

const descendants = item => [item, ...item.children.flatMap(descendants)];
async function loginFixture(fixture) {
  fixture.lookup('management-key').value = 'key';
  fixture.lookup('login-form').listeners.get('submit')({preventDefault() {}});
  await fixture.settle();
  assert.equal(fixture.lookup('workspace').hidden, false);
}
function trackedEnvelope(id, kind, phase, detail = {}) {
  return {job_id: id, kind, phase, account_id: detail.account_id || detail.request?.account_id || '', [kind]: {...detail, phase}};
}
test('runtime refresh uses one complete snapshot and leaves the editor draft separate', async () => {
  const fixture = createPanelFixture(); await loginFixture(fixture);
  assert.deepEqual(fixture.requests.map(request => request.url.split('/').pop()), ['snapshot', 'config-editor']);
  fixture.lookup('refresh').listeners.get('click')(); await fixture.settle();
  assert.equal(fixture.requests.at(-1).url.split('/').pop(), 'snapshot');
  assert.equal(fixture.requests.length, 3);
});
test('CPA refresh synchronizes disabled state once and preserves an unfinished draft', async () => {
  let release; const confirmations = [];
  const fixture = createPanelFixture(async (url, options, data) => {
    data.accounts.a ||= {ID: 'a', Enabled: true, Availability: 'available'};
    if (url.endsWith('/refresh-accounts')) {
      assert.equal(options.method, 'POST');
      assert.equal(options.body, undefined);
      return new Promise(resolve => { release = () => {
        data.accounts.a.Enabled = false; data.accounts.a.Availability = 'disabled';
        resolve(response(data.snapshot));
      }; });
    }
    return response(data[url.split('/').pop()]);
  }, message => {confirmations.push(message); return true;});
  await loginFixture(fixture);
  const button = fixture.lookup('refresh-cpa');
  assert.equal(button.hidden, false); assert.equal(button.disabled, false);
  fixture.lookup('config-json').value = '{unfinished draft'; fixture.lookup('config-json').listeners.get('input')();
  button.listeners.get('click')(); button.listeners.get('click')();
  assert.equal(button.disabled, true); assert.equal(button.textContent, '正在同步…');
  assert.equal(fixture.requests.filter(request => request.options.method === 'POST').length, 1);
  release(); await fixture.settle();
  assert.ok(descendants(fixture.lookup('account-list')).some(item => item.textContent === 'CPA 账号已禁用'));
  assert.equal(fixture.lookup('config-json').value, '{unfinished draft');
  assert.match(fixture.lookup('notice').textContent, /已从 CPA 同步.*未连接上游/);
  assert.equal(button.disabled, false); assert.equal(button.textContent, '从 CPA 刷新');
  assert.equal(fixture.requests.length, 3, 'Refresh must not reload policy or submit quota/wake work');
  assert.equal(confirmations.length, 0, 'Local CPA refresh must not require confirmation');
  assert.equal(fixture.lookup('auto-refresh').checked, false);
  fixture.lookup('disconnect').listeners.get('click')();
  assert.equal(button.hidden, true); assert.equal(button.disabled, true);
});
test('failed CPA refresh keeps displayed accounts and restores its button without retries', async () => {
  const fixture = createPanelFixture(async (url, options, data) => {
    data.accounts.a ||= {ID: 'a', Enabled: true};
    if (url.endsWith('/refresh-accounts')) return response({}, 503);
    return response(data[url.split('/').pop()]);
  });
  await loginFixture(fixture);
  const content = () => descendants(fixture.lookup('account-list')).map(item => item.textContent).join('|');
  const before = content();
  fixture.lookup('refresh-cpa').listeners.get('click')(); await fixture.settle();
  assert.equal(content(), before);
  assert.equal(fixture.lookup('refresh-cpa').disabled, false);
  assert.match(fixture.lookup('notice').textContent, /从 CPA 刷新失败，已保留页面数据/);
  assert.equal(fixture.requests.filter(request => request.options.method === 'POST').length, 1);
  assert.equal(fixture.timers.size, 0);
});
test('a pre-refresh poll cannot overwrite the synchronized CPA status', async () => {
  let delayPoll = false, release;
  const fixture = createPanelFixture(async (url, options, data) => {
    data.accounts.a ||= {ID: 'a', Enabled: true, Availability: 'available'};
    if (url.endsWith('/snapshot') && delayPoll) {
      delayPoll = false; const old = data.snapshot;
      return new Promise(resolve => {release = () => resolve(response(old));});
    }
    if (url.endsWith('/refresh-accounts')) {
      data.accounts.a.Enabled = false; data.accounts.a.Availability = 'disabled';
      return response(data.snapshot);
    }
    return response(data[url.split('/').pop()]);
  });
  await loginFixture(fixture);
  fixture.lookup('auto-refresh').checked = true; fixture.lookup('auto-refresh').listeners.get('change')();
  delayPoll = true; await fixture.poll();
  fixture.lookup('refresh-cpa').listeners.get('click')(); await fixture.settle();
  release(); await fixture.settle();
  assert.ok(descendants(fixture.lookup('account-list')).some(item => item.textContent === 'CPA 账号已禁用'));
  assert.match(fixture.lookup('notice').textContent, /已从 CPA 同步/);
  fixture.lookup('disconnect').listeners.get('click')();
});
test('late polling results and errors cannot overwrite a newer accepted query snapshot', async () => {
  for (const oldFails of [false, true]) {
    let defer = false, release;
    const fixture = createPanelFixture(async (url, options, data) => {
      data.accounts.a ||= {ID: 'a', Enabled: true, FiveHour: {Remaining: {Value: 80, Unit: 'percent'}}};
      if (url.endsWith('/snapshot') && defer) {
        defer = false; const old = data.snapshot;
        return new Promise((resolve, reject) => {release = () => oldFails ? reject(new Error('old failed read')) : resolve(response(old));});
      }
      if (options.method === 'POST') {
        data.accounts.a.FiveHour.Remaining.Value = 73;
        data.summary.scheduler.jobs = [trackedEnvelope('my-query', 'quota_query', 'completed', {total: 1, completed: 1})];
        return response({status: 'queued', job_id: 'my-query'}, 202);
      }
      return response(data[url.split('/').pop()]);
    });
    await loginFixture(fixture);
    fixture.lookup('auto-refresh').checked = true; fixture.lookup('auto-refresh').listeners.get('change')();
    defer = true; await fixture.poll();
    fixture.lookup('quota-query').listeners.get('click')(); await fixture.settle();
    const notice = fixture.lookup('notice').textContent;
    assert.match(fixture.lookup('quota-query-status').textContent, /查询结束/);
    release(); await fixture.settle();
    assert.ok(descendants(fixture.lookup('account-list')).some(item => item.textContent === '73%'));
    assert.equal(fixture.lookup('notice').textContent, notice);
    fixture.lookup('disconnect').listeners.get('click')();
  }
});
test('a pre-save poll cannot revert an accepted config even when the followup snapshot fails', async () => {
  for (const followupFails of [false, true]) {
    let defer = false, accepted = false, release;
    const fixture = createPanelFixture(async (url, options, data) => {
      if (url.endsWith('/snapshot') && defer) {
        defer = false; const old = data.snapshot;
        return new Promise(resolve => {release = () => resolve(response(old));});
      }
      if (url.endsWith('/config') && options.method === 'POST') {
        accepted = true; data.config = JSON.parse(options.body).config; data.summary.enabled = data.config.enabled;
        return response({config: data.config, revision: 'saved'});
      }
      if (url.endsWith('/snapshot') && accepted && followupFails) throw new Error('followup read failed');
      return response(data[url.split('/').pop()]);
    });
    await loginFixture(fixture);
    fixture.lookup('auto-refresh').checked = true; fixture.lookup('auto-refresh').listeners.get('change')();
    defer = true; await fixture.poll();
    const config = JSON.parse(fixture.lookup('config-json').value); config.enabled = true;
    fixture.lookup('config-json').value = JSON.stringify(config); fixture.lookup('config-json').listeners.get('input')(); fixture.lookup('json-apply').listeners.get('click')();
    fixture.lookup('config-save').listeners.get('click')(); await fixture.settle();
    assert.match(fixture.lookup('mode-title').textContent, /模型唤醒已开启/);
    if (followupFails) assert.match(fixture.lookup('notice').textContent, /规则已保存并应用；状态暂未读取成功/);
    release(); await fixture.settle();
    assert.match(fixture.lookup('mode-title').textContent, /模型唤醒已开启/);
    assert.equal(JSON.parse(fixture.lookup('config-json').value).enabled, true);
    assert.equal(fixture.requests.filter(request => request.options.method === 'POST').length, 1);
    fixture.lookup('disconnect').listeners.get('click')();
  }
});
test('a retained task ID resolves its own result after another tab overwrites the latest job', async () => {
  let completed = false;
  const fixture = createPanelFixture(async (url, options, data) => {
    if (options.method === 'POST') return response({status: 'queued', job_id: 'mine'}, 202);
    if (url.endsWith('/snapshot')) {
      data.summary.scheduler.quota_query = {account_id: 'other-tab', phase: 'completed', completed: 8};
      data.summary.scheduler.jobs = [trackedEnvelope('mine', 'quota_query', completed ? 'completed' : 'queued', {account_id: 'mine-account', total: 1, completed: completed ? 1 : 0}), trackedEnvelope('other', 'quota_query', 'completed', {account_id: 'other-tab', completed: 8})];
    }
    return response(data[url.split('/').pop()]);
  });
  // Initial state has a queued task from another tab: opt out of it before submitting ours.
  completed = true; await loginFixture(fixture); completed = false;
  fixture.lookup('quota-query').listeners.get('click')(); await fixture.settle();
  assert.match(fixture.lookup('quota-query-status').textContent, /等待查询/);
  completed = true; await fixture.poll();
  assert.match(fixture.lookup('quota-query-status').textContent, /成功 1/);
  assert.equal(fixture.timers.size, 0);
  assert.equal(fixture.requests.filter(request => request.options.method === 'POST').length, 1);
});
test('restart and evicted active task records end optimistic waiting explicitly', async () => {
  for (const mode of ['restart', 'evicted']) {
    let finish = false, submitted = false;
    const fixture = createPanelFixture(async (url, options, data) => {
      if (options.method === 'POST') {submitted = true; return response({status: 'queued', job_id: 'mine'}, 202);}
      if (submitted) {
        if (finish && mode === 'restart') data.instance_id = 'new-runtime';
        data.summary.scheduler.jobs = finish ? [] : [trackedEnvelope('mine', 'quota_query', 'queued')];
      }
      return response(data[url.split('/').pop()]);
    });
    await loginFixture(fixture); fixture.lookup('quota-query').listeners.get('click')(); await fixture.settle();
    assert.equal(fixture.timers.size, 1);
    finish = true; await fixture.poll();
    assert.match(fixture.lookup('quota-query-status').textContent, mode === 'restart' ? /后台已停止或重启/ : /任务记录已不可用/);
    assert.equal(fixture.timers.size, 0);
    assert.equal(fixture.lookup('quota-query').disabled, false);
  }
});
test('wake tasks recover from accepted read failures and poll through verification with page refresh off', async () => {
  let phase = '', failRead = false;
  const fixture = createPanelFixture(async (url, options, data) => {
    data.accounts = {a: {ID: 'a', Enabled: true}};
    data.capabilities.StableAccountIdentity = data.capabilities.ExactAccountExecution = true;
    if (options.method === 'POST') {phase = 'queued'; failRead = true; return response({status: 'queued', job_id: 'wake'}, 202);}
    if (url.endsWith('/snapshot') && failRead) throw new Error('read failure');
    data.summary.scheduler.jobs = phase ? [trackedEnvelope('wake', 'manual_wake', phase, {account_id: 'a'})] : [];
    return response(data[url.split('/').pop()]);
  });
  await loginFixture(fixture);
  descendants(fixture.lookup('account-list')).find(item => item.dataset.wakeAccount === 'a').listeners.get('click')(); await fixture.settle();
  assert.equal(fixture.timers.size, 1); assert.match(fixture.lookup('notice').textContent, /无法读取 CPA/);
  failRead = false;
  for (const next of ['running', 'verifying']) {phase = next; await fixture.poll(); assert.equal(fixture.timers.size, 1);}
  assert.ok(descendants(fixture.lookup('account-list')).some(item => item.textContent.includes('等待服务端窗口证据')));
  phase = 'completed'; await fixture.poll();
  assert.equal(fixture.timers.size, 0); assert.equal(fixture.lookup('auto-refresh').checked, false);
  assert.ok(descendants(fixture.lookup('account-list')).some(item => item.textContent.includes('已确认窗口启动')));
  assert.equal(fixture.requests.filter(request => request.options.method === 'POST').length, 1);
});
test('generation tasks recover from accepted read failures and retain their preview after another generation', async () => {
  let phase = '', failRead = false, request;
  const fixture = createPanelFixture(async (url, options, data) => {
    data.accounts = {a: {ID: 'a', Enabled: true}}; data.capabilities.ExactAccountExecution = true;
    if (options.method === 'POST') {request = JSON.parse(options.body); phase = 'queued'; failRead = true; return response({status: 'queued', job_id: request.request_id}, 202);}
    if (url.endsWith('/snapshot') && failRead) throw new Error('read failure');
    data.summary.scheduler.jobs = phase ? [trackedEnvelope(request.request_id, 'prompt_generation', phase, {request, candidates: ['Hello.', 'Ready.']})] : [];
    data.summary.scheduler.prompt_generation = {phase: 'completed', request: {request_id: 'another-tab'}, candidates: ['Wrong.']};
    return response(data[url.split('/').pop()]);
  });
  await loginFixture(fixture);
  fixture.lookup('generation-account').value = 'a'; fixture.lookup('generation-model').value = 'model'; fixture.lookup('generation-count').value = '5'; fixture.lookup('generation-target').value = 'default';
  fixture.lookup('generation-start').listeners.get('click')(); await fixture.settle();
  assert.equal(fixture.timers.size, 1);
  failRead = false; phase = 'dispatched'; await fixture.poll(); assert.match(fixture.lookup('generation-status').textContent, /模型正在生成/);
  phase = 'completed'; await fixture.poll();
  assert.equal(fixture.lookup('generation-preview').value, 'Hello.\nReady.');
  assert.equal(fixture.timers.size, 0); assert.equal(fixture.lookup('auto-refresh').checked, false);
  assert.equal(fixture.requests.filter(request => request.options.method === 'POST').length, 1);
  fixture.lookup('generation-apply').listeners.get('click')();
  assert.deepEqual(JSON.parse(fixture.lookup('config-json').value).execution.prompt_pool, ['Hello.', 'Ready.']);
});
test('invalid snapshots never partially replace applied config, accounts or drafts', async () => {
  let invalid = false;
  const fixture = createPanelFixture(async (url, options, data) => {
    if (url.endsWith('/snapshot') && invalid) return response({...data.snapshot, accounts: null, config: {...data.config, enabled: true}, summary: {...data.summary, enabled: true}});
    return response(data[url.split('/').pop()]);
  });
  await loginFixture(fixture);
  fixture.lookup('config-json').value = '{unfinished draft'; fixture.lookup('config-json').listeners.get('input')();
  invalid = true; fixture.lookup('refresh').listeners.get('click')(); await fixture.settle();
  assert.match(fixture.lookup('mode-title').textContent, /仅查看与查询额度/);
  assert.equal(fixture.lookup('config-json').value, '{unfinished draft');
  assert.match(fixture.lookup('notice').textContent, /无法识别的数据/);
});
test('snapshot versions reject regressions and accept a new instance starting at a lower sequence', async () => {
  let version = 10, instance = 'first', enabled = false;
  const fixture = createPanelFixture(async (url, options, data) => {
    if (url.endsWith('/snapshot')) { const snapshot = data.snapshot; return response({...snapshot, instance_id: instance, snapshot_seq: version, summary: {...snapshot.summary, enabled}, config: {...snapshot.config, enabled}}); }
    return response(data[url.split('/').pop()]);
  });
  await loginFixture(fixture);
  enabled = true; version = 9; fixture.lookup('refresh').listeners.get('click')(); await fixture.settle();
  assert.match(fixture.lookup('mode-title').textContent, /仅查看与查询额度/);
  instance = 'second'; version = 1; fixture.lookup('refresh').listeners.get('click')(); await fixture.settle();
  assert.match(fixture.lookup('mode-title').textContent, /模型唤醒已开启/);
  instance = 'first'; version = 99; enabled = false; fixture.lookup('refresh').listeners.get('click')(); await fixture.settle();
  assert.match(fixture.lookup('mode-title').textContent, /模型唤醒已开启/);
});
test('accepted reload, recheck and connection checks remain accepted if their snapshot read fails', async () => {
  for (const action of ['reload', 'recheck', 'quota-connection-check']) {
    let accepted = false;
    const fixture = createPanelFixture(async (url, options, data) => {
      data.capabilities.quota_bridge = {mode: 'host'};
      if (options.method === 'POST') {accepted = true; return response({status: 'scheduled'});}
      if (accepted && url.endsWith('/snapshot')) throw new Error('followup snapshot unavailable');
      return response(data[url.split('/').pop()]);
    });
    await loginFixture(fixture);
    fixture.lookup(action).listeners.get('click')(); await fixture.settle();
    assert.match(fixture.lookup('notice').textContent, action === 'quota-connection-check' ? /连通性检查已完成；状态暂未读取成功/ : /操作已接受；状态暂未读取成功/);
    assert.equal(fixture.requests.filter(request => request.options.method === 'POST').length, 1);
    assert.equal(fixture.lookup('workspace').hidden, false);
  }
});
test('blocked tasks expose known retry reasons without reflecting arbitrary error codes', async () => {
  let submitted = false;
  const fixture = createPanelFixture(async (url, options, data) => {
    if (options.method === 'POST') {submitted = true; return response({job_id: 'blocked'}, 202);}
    if (submitted) data.summary.scheduler.jobs = [{...trackedEnvelope('blocked', 'quota_query', 'queued'), blocked_reason: 'account_inventory_unavailable', retry_at: new Date(Date.now() + 5000).toISOString()}];
    return response(data[url.split('/').pop()]);
  });
  await loginFixture(fixture); fixture.lookup('quota-query').listeners.get('click')(); await fixture.settle();
  assert.match(fixture.lookup('quota-query-status').textContent, /暂时阻塞：账号清单暂不可用.*下次检查/);
  assert.equal(fixture.timers.size, 1);
  fixture.lookup('disconnect').listeners.get('click')();
});
test('a task accepted by a restarted server resolves by ID and keeps its confirmed result across a later restart', async () => {
  let submitted = false, completed = false, restartAgain = false;
  const fixture = createPanelFixture(async (url, options, data) => {
    if (options.method === 'POST') {submitted = true; data.instance_id = 'new-after-post'; return response({job_id: 'accepted-new-instance'}, 202);}
    if (submitted) data.summary.scheduler.jobs = restartAgain ? [] : [trackedEnvelope('accepted-new-instance', 'quota_query', completed ? 'completed' : 'queued', {completed: completed ? 1 : 0, total: 1})];
    if (restartAgain) data.instance_id = 'next-instance';
    return response(data[url.split('/').pop()]);
  });
  await loginFixture(fixture); fixture.lookup('quota-query').listeners.get('click')(); await fixture.settle();
  assert.match(fixture.lookup('quota-query-status').textContent, /等待查询/);
  assert.equal(fixture.timers.size, 1);
  completed = true; await fixture.poll();
  assert.match(fixture.lookup('quota-query-status').textContent, /查询结束.*成功 1/);
  restartAgain = true; fixture.lookup('refresh').listeners.get('click')(); await fixture.settle();
  assert.match(fixture.lookup('quota-query-status').textContent, /查询结束.*成功 1/);
  assert.equal(fixture.timers.size, 0);
});
test('reconnecting prefers retained terminal wake jobs over a legacy verifying status', async () => {
  for (const phase of ['unknown', 'skipped', 'blocked']) {
    let submitted = false, terminal = false;
    const fixture = createPanelFixture(async (url, options, data) => {
      data.accounts = {a: {ID: 'a', Enabled: true}}; data.capabilities.StableAccountIdentity = data.capabilities.ExactAccountExecution = true;
      if (options.method === 'POST') {submitted = true; return response({job_id: 'wake-reconnect'}, 202);}
      data.summary.scheduler.manual_wake = {phase: 'verifying', account_id: 'a'};
      data.summary.scheduler.jobs = submitted ? [trackedEnvelope('wake-reconnect', 'manual_wake', terminal ? phase : 'verifying', {account_id: 'a', finished_at: terminal ? new Date().toISOString() : '', last_error: terminal ? 'send_not_sent' : ''})] : [];
      if (!submitted) data.summary.scheduler.manual_wake = {};
      return response(data[url.split('/').pop()]);
    });
    await loginFixture(fixture);
    descendants(fixture.lookup('account-list')).find(item => item.dataset.wakeAccount === 'a').listeners.get('click')(); await fixture.settle();
    assert.equal(fixture.timers.size, 1);
    terminal = true; await fixture.poll();
    assert.equal(fixture.timers.size, 0);
    fixture.lookup('disconnect').listeners.get('click')(); await loginFixture(fixture);
    assert.equal(fixture.timers.size, 0, 'legacy verifying state must not resume endless polling after reconnect');
    const text = descendants(fixture.lookup('account-list')).map(item => item.textContent).join(' ');
    assert.match(text, phase === 'unknown' ? /送达或窗口启动未确认，不自动重试/ : /未发送，安全条件不满足/);
    assert.doesNotMatch(text, /请求已发送，等待服务端窗口证据/);
    fixture.lookup('disconnect').listeners.get('click')();
  }
});
test('newly accepted registry jobs are visible on connection even when legacy latest fields lag', async () => {
  let finished = false;
  const fixture = createPanelFixture(async (url, options, data) => {
    data.summary.scheduler.quota_query = {phase: 'completed', completed: 99, total: 99};
    data.summary.scheduler.jobs = [trackedEnvelope('previous', 'quota_query', 'completed', {completed: 99, total: 99}), trackedEnvelope('newest', 'quota_query', finished ? 'completed' : 'queued', {completed: finished ? 1 : 0, total: 1})];
    return response(data[url.split('/').pop()]);
  });
  await loginFixture(fixture);
  assert.match(fixture.lookup('quota-query-status').textContent, /等待查询/);
  assert.equal(fixture.timers.size, 1);
  finished = true; await fixture.poll();
  assert.match(fixture.lookup('quota-query-status').textContent, /查询结束.*成功 1/);
  assert.equal(fixture.timers.size, 0);
});
test('terminal failures cannot be rendered as temporary blocks with a future retry', async () => {
  const fixture = createPanelFixture(async (url, options, data) => {
    data.summary.scheduler.jobs = [{...trackedEnvelope('failed-job', 'quota_query', 'failed', {last_error: 'quota_query_timeout', finished_at: new Date().toISOString()}), blocked_reason: 'account_inventory_unavailable', retry_at: new Date(Date.now() + 10000).toISOString()}];
    return response(data[url.split('/').pop()]);
  });
  await loginFixture(fixture);
  assert.match(fixture.lookup('quota-query-status').textContent, /任务失败，不自动重试.*额度查询超时/);
  assert.doesNotMatch(fixture.lookup('quota-query-status').textContent, /暂时阻塞|下次检查/);
  assert.equal(fixture.timers.size, 0);
});
