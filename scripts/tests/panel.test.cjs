'use strict';
const {test} = require('node:test');
const assert = require('node:assert/strict');
const {readFileSync} = require('node:fs');
const {runInNewContext} = require('node:vm');
const {createSession, createEditor, resetTime, countdown, windowCountdown, manualWakeReason, apiBase, readPanelKey, quotaFailure, dailyTimeRange} = require('../../internal/plugin/web/panel.js');
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
  assert.match(manualWakeReason({...account, FiveHour: {State: 'active', ResetAt: new Date(Date.now() + 60000).toISOString()}}, config, summary, capabilities), /无需重复/);
  const calls = [], session = createSession(async (url, options) => {calls.push({url, options}); return response({status: 'queued'}, 202);});
  session.setKey('test-key');
  await session.request('/manual-wake', 'POST', {account_id: 'account-a'});
  assert.equal(calls.length, 1);
  assert.equal(calls[0].options.body, JSON.stringify({account_id: 'account-a'}));
  assert.ok(calls[0].url.endsWith('/manual-wake'));
  session.disconnect();
});
function createPanelFixture(fetcher, confirm = () => true, fields = []) {
  const elements = new Map(), timers = new Map(), visibility = new Map(), events = new Map(), requests = [];
  const navigator = {onLine: true};
  let nextTimer = 0;
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
  const data = {summary: {running: true, enabled: false, scheduler: {}}, accounts: {},
    capabilities: {QuotaAdapterReady: true}, config, 'config-editor': {config, revision: 'r1'}};
  runInNewContext(readFileSync(require.resolve('../../internal/plugin/web/panel.js'), 'utf8'), {
    document, navigator, URL, URLSearchParams, TextEncoder, AbortController, structuredClone, confirm,
    location: {origin: 'https://cpa.test', pathname: '/v0/resource/plugins/codex-window-refresh/status'},
    localStorage: {getItem: () => null}, addEventListener: (event, handler) => events.set(event, handler), setInterval() {},
    setTimeout: (callback, duration) => {const timer = ++nextTimer; timers.set(timer, {callback, duration}); return timer;},
    clearTimeout: timer => timers.delete(timer),
    fetch: async (url, options) => {requests.push({url, options}); return fetcher ? fetcher(url, options, data) : response(data[url.split('/').pop()] || {status: 'queued'});}
  });
  const settle = () => new Promise(resolve => setImmediate(resolve));
  async function poll() {
    const [timerID, timer] = [...timers].find(([, value]) => [5000, 10000, 20000, 40000, 60000].includes(value.duration));
    timers.delete(timerID);
    timer.callback();
    await settle();
  }
  return {lookup, timers, visibility, events, navigator, document, requests, settle, poll};
}
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
test('unavailable account still allows exact quota query while manual wake stays blocked', async () => {
  const {lookup, requests, settle} = createPanelFixture(async (url, options, data) => {
    data.capabilities.StableAccountIdentity = data.capabilities.ExactAccountExecution = true;
    data.accounts = {'unavailable-account': {ID: 'unavailable-account', Enabled: false, Permission: 'blocked', FiveHour: {State: 'expired', Remaining: {Value: 0, Unit: 'percent'}}}};
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
  assert.equal(wake.disabled, true);
  assert.ok(items.some(item => item.textContent === 'CPA 账号不可用'));
  query.listeners.get('click')();
  await settle();
  const writes = requests.filter(request => request.options.method === 'POST');
  assert.equal(writes.length, 1);
  assert.ok(writes[0].url.endsWith('/quota-query'));
  assert.deepEqual(JSON.parse(writes[0].options.body), {account_id: 'unavailable-account'});
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
test('timeline sorts events and groups days in configured timezone without writes', async () => {
  const {lookup, requests, settle} = createPanelFixture(async (url, options, data) => {
    data.config.timezone = 'Asia/Shanghai';
    data.summary.scheduler.plan = {enabled: true, horizon_days: 3, entries: [
      {kind: 'wake', trigger: 'weekly_reset', earliest_at: '2026-10-14T16:00:00Z', latest_at: '2026-10-14T16:00:00Z', status: 'blocked', detail: 'blocked'},
      {kind: 'query', trigger: 'periodic', window: 'five_hour', earliest_at: '2026-10-13T16:00:00Z', latest_at: '2026-10-13T16:00:00Z', repeat_every: '30s', status: 'estimated', detail: 'query'}
    ]};
    return response(data[url.split('/').pop()]);
  });
  lookup('management-key').value = 'test-key'; lookup('login-form').listeners.get('submit')({preventDefault() {}});
  await settle();
  const rows = lookup('plan-list').children;
  assert.equal(rows.length, 4);
  assert.match(rows[0].textContent, /2026\/10\/14/);
  assert.match(rows[1].children[0].textContent, /5 小时/);
  assert.match(rows[1].children[2].textContent, /每 30s 循环/);
  assert.match(rows[2].textContent, /2026\/10\/15/);
  assert.equal(rows[3].className, 'card plan-item plan-wake plan-blocked');
  assert.equal(requests.some(request => request.options.method === 'POST'), false);
  lookup('disconnect').listeners.get('click')();
});
test('future plan renders server state safely without dispatching operations', async () => {
  const now = new Date().toISOString();
  const {lookup, requests, settle} = createPanelFixture(async (url, options, data) => {
    data.summary.scheduler.plan = {enabled: true, horizon_days: 3, generated_at: now, until: now, entries: [{kind: 'query', trigger: 'weekly_reset', account_id: '<img src=x onerror=alert(1)>', earliest_at: now, latest_at: now, status: 'estimated', detail: 'Only quota'}]};
    return response(data[url.split('/').pop()]);
  });
  lookup('management-key').value = 'test-key'; lookup('login-form').listeners.get('submit')({preventDefault() {}});
  await settle();
  const row = lookup('plan-list').children[1];
  assert.equal(lookup('plan-list').children[0].className, 'timeline-day');
  assert.equal(row.className, 'card plan-item plan-query');
  assert.ok(row.children.some(item => item.textContent === '<img src=x onerror=alert(1)>'));
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
test('long-running polling backs off failures and returns to five seconds without accumulating requests', async () => {
  let failing = false;
  const {lookup, timers, settle, poll, requests} = createPanelFixture(async (url, options, data) => {
    if (failing && url.endsWith('/accounts')) throw new Error('temporary network failure');
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
    if (pending && url.endsWith('/accounts')) {
      return new Promise(resolve => {lateRead = () => resolve(response({late: {ID: 'late', Enabled: true}}));});
    }
    return response(data[url.split('/').pop()]);
  });
  lookup('management-key').value = 'test-key'; lookup('login-form').listeners.get('submit')({preventDefault() {}});
  await settle();
  lookup('auto-refresh').checked = true; lookup('auto-refresh').listeners.get('change')();
  pending = true; await poll();
  const outstanding = requests.at(-2), count = requests.length;
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
      if (failing && url.endsWith('/accounts')) {
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
    if (url.endsWith('/accounts')) {
      if (mode === 'failed') throw new Error('private network failure');
      if (mode === 'pending') return new Promise(resolve => {pendingRead = () => resolve(response(data.accounts));});
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
