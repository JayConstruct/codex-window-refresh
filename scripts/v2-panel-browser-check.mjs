import assert from 'node:assert/strict';
import {createServer} from 'node:http';
import {spawn} from 'node:child_process';
import {readFile, writeFile, mkdir, mkdtemp, rm, access} from 'node:fs/promises';
import {tmpdir, homedir} from 'node:os';
import {join, resolve, dirname} from 'node:path';
import {fileURLToPath} from 'node:url';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const base = '/cpa/v0/resource/plugins/codex-window-refresh/';
const api = '/cpa/v0/management/codex-window-refresh/v2/';
const fixtureKey = 'local-v2-browser-fixture';
const output = process.env.CWR_SCREENSHOT_DIR || join(tmpdir(), 'cwr-v2-panel-preview');
const temporary = await mkdtemp(join(tmpdir(), 'cwr-v2-browser-'));
const posts = [], reads = [], exceptions = [], pageErrors = [];
const jobs = new Map();
let accountReadGate, settingsPostGate, settingsReadGate;
let revision = 1, accountsRevision = 1, settingsRevision = 1, settingsConflictOnce = false, unauthorized = false, browser, server, socket;
let modelRevision = 0, modelConflictOnce = false;
let modelConfig = {default_model: '', default_prompt: '你好，请用一句简短的中文问候回复。', five_hour_model: '', five_hour_prompt: '', weekly_model: '', weekly_prompt: '', generation_prompt: '生成简短、自然、适合日常问候的中文短句，避免重复。', prompt_mode: 'fixed', phrases: []};
const generations = new Map();
let wakeCapable = false, wakeRevision = 0, wakeConflictOnce = false;
let wakeConfig = {enabled: false, on_five_hour_reset: false, on_weekly_reset: false, timezone: 'Asia/Shanghai', random_delay: {min_seconds: 5, max_seconds: 60}, allowed_hours: ['08:00-23:00'], accounts_per_user: 1, max_accounts: 10000, daily_enabled: false, daily_window: '09:00-09:10', daily_target: 1};
const wakeJobs = new Map();
const generationAccounts = [
  {id: 'codex-01', provider: 'codex', status: 'available'},
  {id: 'antigravity-fixture', provider: 'antigravity', status: 'available'},
  {id: 'antigravity-disabled', provider: 'antigravity', status: 'disabled'}
];
let queryConfig = {timezone: 'Asia/Shanghai', automatic: false, interval_seconds: 300, on_five_hour_expiry: false,
  on_weekly_expiry: false, random_delay: {min_seconds: 5, max_seconds: 60}, excluded_hours: []};
const observed = () => new Date().toISOString();
const later = seconds => new Date(Date.now() + seconds * 1000).toISOString();
const accounts = Array.from({length: 24}, (_, index) => {
  const percent = [82, 100, 46, 65, 0, 0][index] ?? 72;
  return {id: 'codex-' + String(index + 1).padStart(2, '0'), status: [4, 22].includes(index) ? 'disabled' : [5, 23].includes(index) ? 'unavailable' : 'available', observed_at: observed(),
    quota: index === 4 ? undefined : {source: 'cpa_wham_usage', observed_at: observed(), permission: index === 5 ? 'blocked' : 'allowed',
      five_hour: {State: 'active', Remaining: {Value: percent, Unit: 'percent'}, ResetAt: later(4320), ConfirmedAt: observed()},
      weekly: {State: 'active', Remaining: {Value: [67, 91, 58, 34, 0, 12][index] ?? 67, Unit: 'percent'}, ResetAt: later(201600), ConfirmedAt: observed()}}};
});
const fixtureJobs = [
  {id: 'fixture-running', target: 'codex-07', phase: 'running', trigger: 'manual_single', querying: 1, waiting: 0, completed: 0, failed: 0, total: 1, created_at: observed(), updated_at: observed()},
  {id: 'fixture-waiting', target: 'codex-02', phase: 'running', trigger: 'periodic', scheduled_at: later(-60), hold_reason: 'excluded_hours', querying: 0, waiting: 1, completed: 0, failed: 0, total: 1, created_at: observed(), updated_at: observed()},
  {id: 'fixture-bulk', target: '', phase: 'running', trigger: 'manual_batch', scheduled_at: later(120), querying: 1, waiting: 23, completed: 0, failed: 0, total: 24, created_at: observed(), updated_at: observed()}
];
accounts[0].pending_query = {phase: 'queued', scheduled_at: later(90), trigger: 'manual_batch', job_id: 'fixture-bulk', hold_reason: ''};
for (const job of fixtureJobs) jobs.set(job.id, {...job, job_id: job.id, items: (job.target ? accounts.filter(account => account.id === job.target) : accounts).map((account, index) => ({account_id: account.id, phase: index < job.querying ? 'running' : 'queued', scheduled_at: job.scheduled_at, trigger: job.trigger, hold_reason: job.hold_reason || ''}))});
const identity = () => ({instance_id: 'local-browser-fixture', view_revision: revision});
const settings = () => ({...identity(), revision: settingsRevision, config: queryConfig});
const modelSettings = () => ({...identity(), revision: modelRevision, config: modelConfig});
function json(response, status, body) {
  response.writeHead(status, {'Content-Type': 'application/json', 'Cache-Control': 'no-store'}); response.end(JSON.stringify(body));
}
async function waitFor(check, message, timeout = 12000) {
  const until = Date.now() + timeout;
  while (Date.now() < until) { if (await check()) return; await new Promise(resolve => setTimeout(resolve, 50)); }
  throw new Error(message);
}
function gate(outcome = 'success') {
  const pending = {started: false, outcome};
  pending.promise = new Promise(resolve => { pending.release = resolve; });
  return pending;
}
try {
  let executable = process.env.CHROME_BIN;
  if (!executable) {
    for (const candidate of ['/usr/bin/chromium', '/usr/bin/google-chrome', join(homedir(), '.cache/ms-playwright/chromium-1234/chrome-linux64/chrome'), join(homedir(), '.cache/ms-playwright/chromium-1187/chrome-linux/chrome')]) {
      try { await access(candidate); executable = candidate; break; } catch {}
    }
  }
  assert.ok(executable, 'Set CHROME_BIN to a local Chromium binary');
  assert.equal(typeof WebSocket, 'function', 'Node.js 22 or newer is required');
  server = createServer(async (request, response) => {
    try {
      const url = new URL(request.url, 'http://127.0.0.1');
      if (url.pathname.startsWith(base)) {
        const name = url.pathname.slice(base.length), filenames = {status: 'index.html', 'panel.js': 'panel.js', 'panel.css': 'panel.css'};
        if (!filenames[name]) { response.writeHead(404); response.end(); return; }
        response.writeHead(200, {'Content-Type': name === 'status' ? 'text/html; charset=utf-8' : name.endsWith('.js') ? 'text/javascript' : 'text/css',
          'Content-Security-Policy': "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'self'; base-uri 'none'; form-action 'none'"});
        response.end(await readFile(join(root, 'internal/v2/daemon/web', filenames[name]))); return;
      }
      if (!url.pathname.startsWith(api)) { response.writeHead(404); response.end(); return; }
      if (request.headers.authorization !== 'Bearer ' + fixtureKey || unauthorized) { json(response, 401, {error: 'unauthorized'}); return; }
      const resource = url.pathname.slice(api.length);
      if (request.method === 'POST') {
        let body = ''; for await (const chunk of request) body += chunk;
        posts.push({resource, body, headers: request.headers});
        if (resource === 'wake-settings') {
          const data = JSON.parse(body); assert.deepEqual(Object.keys(data).sort(), ['config', 'revision']);
          assert.deepEqual(Object.keys(data.config).sort(), Object.keys(wakeConfig).sort());
          if (wakeConflictOnce) { wakeConflictOnce = false; wakeRevision++; revision++; }
          if (data.revision !== wakeRevision) { json(response, 409, {error: 'wake_settings_conflict'}); return; }
          wakeConfig = data.config; wakeRevision++; revision++; json(response, 200, {...identity(), revision: wakeRevision, config: wakeConfig}); return;
        }
        if (resource === 'wake-jobs') {
          const data = JSON.parse(body), key = request.headers['idempotency-key'];
          assert.deepEqual(Object.keys(data), ['account_id']); assert.equal(data.account_id, 'codex-01'); assert.ok(key);
          const existing = [...wakeJobs.values()].find(job => job.key === key);
          if (existing) { assert.equal(existing.request, body); json(response, 202, {...identity(), ...existing}); return; }
          const job = {job_id: 'manual-wake-' + wakeJobs.size, account_id: data.account_id, reason: 'manual', phase: 'queued', key, request: body, created_at: observed(), updated_at: observed()};
          wakeJobs.set(job.job_id, job); revision++; json(response, 202, {...identity(), ...job}); return;
        }
        if (resource === 'sync') { revision++; json(response, 202, {status: 'queued'}); return; }
        if (resource === 'model-settings') {
          const data = JSON.parse(body);
          assert.deepEqual(Object.keys(data).sort(), ['config', 'revision']);
          assert.deepEqual(Object.keys(data.config).sort(), ['default_model', 'default_prompt', 'five_hour_model', 'five_hour_prompt', 'weekly_model', 'weekly_prompt', 'generation_prompt', 'prompt_mode', 'phrases'].sort());
          if (modelConflictOnce) { modelConflictOnce = false; modelRevision++; revision++; modelConfig = {...modelConfig, default_model: 'fixture-concurrent-model'}; }
          if (data.revision !== modelRevision) { json(response, 409, {error: 'model_settings_conflict'}); return; }
          modelConfig = data.config; modelRevision++; revision++; json(response, 200, modelSettings()); return;
        }
        if (resource === 'prompt-generations') {
          const data = JSON.parse(body), key = request.headers['idempotency-key'];
          assert.deepEqual(Object.keys(data).sort(), ['account_id', 'count', 'model', 'prompt']);
          assert.ok(key); assert.equal(typeof data.account_id, 'string'); assert.equal(typeof data.model, 'string');
          assert.ok(Number.isInteger(data.count) && data.count >= 1 && data.count <= 20);
          const existing = [...generations.values()].find(job => job.key === key);
          if (existing) { assert.equal(existing.request, body); json(response, 202, existing); return; }
          const job = {...identity(), job_id: 'candidate-job-' + generations.size, phase: 'queued', error_code: '', candidates: [], account_id: data.account_id, model: data.model, count: data.count, key, request: body, created_at: observed(), updated_at: observed()};
          generations.set(job.job_id, job); json(response, 202, job); return;
        }
        if (resource === 'settings') {
          const data = JSON.parse(body);
          assert.deepEqual(Object.keys(data).sort(), ['config', 'revision']);
          assert.deepEqual(Object.keys(data.config).sort(), ['automatic', 'excluded_hours', 'interval_seconds', 'on_five_hour_expiry', 'on_weekly_expiry', 'random_delay', 'timezone']);
          const held = settingsPostGate;
          if (held) { held.started = true; await held.promise; }
          if (held?.outcome === 'reject') { json(response, 400, {error: 'invalid_query_settings'}); return; }
          if (settingsConflictOnce) { settingsConflictOnce = false; settingsRevision++; revision++; queryConfig = {...queryConfig, interval_seconds: 180}; }
          if (data.revision !== settingsRevision) { json(response, 409, {error: 'settings_conflict'}); return; }
          queryConfig = data.config; settingsRevision++; revision++;
          if (held?.outcome === 'disconnect') {
            response.writeHead(200, {'Content-Type': 'application/json', 'Cache-Control': 'no-store'});
            response.write('{"instance_id":');
            setTimeout(() => response.destroy(), 50); return;
          }
          json(response, 200, settings()); return;
        }
        if (resource !== 'jobs') { json(response, 404, {error: 'not_found'}); return; }
        const data = JSON.parse(body);
        assert.deepEqual(Object.keys(data).sort(), ['account_id', 'kind']); assert.equal(data.kind, 'quota_query');
        const id = 'browser-job-' + posts.length;
        const batch = !data.account_id, due = batch ? later(60) : undefined;
        const job = {...identity(), job_id: id, account_id: data.account_id, phase: batch ? 'running' : 'completed', trigger: batch ? 'manual_batch' : 'manual_single', scheduled_at: due, querying: 0, waiting: batch ? accounts.length : 0,
          created_at: observed(), updated_at: observed(), items: (batch ? accounts : accounts.filter(account => account.id === data.account_id)).map(account => ({account_id: account.id, phase: batch ? 'queued' : 'completed', scheduled_at: due, trigger: batch ? 'manual_batch' : 'manual_single', hold_reason: batch ? 'excluded_hours' : ''}))};
        for (const account of accounts) {
          if (batch) account.pending_query = {phase: 'queued', scheduled_at: due, trigger: 'manual_batch', job_id: id, hold_reason: 'excluded_hours'};
          else if (account.id === data.account_id) delete account.pending_query;
        }
        jobs.set(id, job); revision++;
        json(response, 202, {job_id: id, instance_id: 'local-browser-fixture'}); return;
      }
      reads.push(request.url);
      if (resource === 'wake-settings') { json(response, 200, {...identity(), revision: wakeRevision, config: wakeConfig}); return; }
      if (resource === 'wake-jobs') {
        const id = url.searchParams.get('job_id');
        if (!id) { assert.equal(url.searchParams.get('limit'), '20'); json(response, 200, {...identity(), jobs: [...wakeJobs.values()]}); return; }
        const job = wakeJobs.get(id); if (!job) { json(response, 404, {error: 'not_found'}); return; }
        job.phase = 'unknown'; job.hold_reason = 'verification_pending'; job.updated_at = observed(); revision++;
        json(response, 200, {...identity(), ...job}); return;
      }
      if (resource === 'generation-accounts') { json(response, 200, {instance_id: identity().instance_id, accounts: generationAccounts}); return; }
      if (resource === 'model-settings') { json(response, 200, modelSettings()); return; }
      if (resource === 'prompt-generations') {
        const job = generations.get(url.searchParams.get('job_id'));
        if (!job) { json(response, 404, {error: 'not_found'}); return; }
        if (job.phase === 'queued') { job.phase = 'completed'; job.candidates = Array.from({length: job.count}, (_, index) => '浏览器候选短句 ' + (index + 1)); job.updated_at = observed(); }
        json(response, 200, {...job, ...identity()}); return;
      }
      if (resource === 'settings') {
        if (settingsReadGate) { settingsReadGate.started = true; await settingsReadGate.promise; }
        json(response, 200, settings()); return;
      }
      if (resource === 'overview') {
        json(response, 200, {...identity(), version: '0.2.0-rc.4', accounts_revision: accountsRevision, query_settings: {revision: settingsRevision, config: queryConfig}, read_only: true, query_concurrency: 8,
          capabilities: {quota_query: true, passive_quota: true, model_execution: wakeCapable, wake_settings: wakeCapable, automatic_wake: wakeCapable, inventory_sync: true, query_settings: true, automatic_quota_query: true, randomized_quota_query: true, model_settings: true, prompt_generation: true, generation_accounts: true, prompt_generation_providers: ['codex', 'antigravity'], editable_generation_prompt: true}, activity: {connected: true},
          inventory: {status: 'ready', last_success_at: observed()}, dashboard: {total: 24, available: 20, disabled: 2, unavailable: 2, capacity: 16, unknown_quota: 4, errors: 2,
            tasks: {active: 3, querying: 2}, recent_jobs: fixtureJobs, active_jobs: fixtureJobs,
            recent_events: [{kind: 'quota_query', at: observed(), account_id: 'codex-07', phase: 'running'}, {kind: 'quota_query', at: observed(), account_id: '', phase: 'queued'}, {kind: 'inventory', at: observed(), phase: 'ready'}]}}); return;
      }
      if (resource === 'jobs') { const job = jobs.get(url.searchParams.get('job_id')); json(response, job ? 200 : 404, job ? {...job, ...identity()} : {error: 'not_found'}); return; }
      if (resource === 'accounts') {
        if (accountReadGate) { accountReadGate.started = true; await accountReadGate.promise; }
        if (url.searchParams.has('after') && Number(url.searchParams.get('revision')) !== accountsRevision) { json(response, 409, {error: 'view_changed'}); return; }
        const search = (url.searchParams.get('search') || '').toLowerCase(), status = url.searchParams.get('status') || 'all';
        const filtered = accounts.filter(account => account.id.toLowerCase().includes(search) && (status === 'all' || status === account.status || status === 'attention' && (account.status === 'unavailable' || account.quota?.permission === 'blocked')));
        const page = filtered.filter(account => account.id > (url.searchParams.get('after') || '')).slice(0, Number(url.searchParams.get('limit') || 6));
        const last = page.at(-1)?.id, next = filtered.some(account => account.id > last) ? last : '';
        json(response, 200, {...identity(), accounts_revision: accountsRevision, accounts: page, next_after: next, total: 24, filter_total: filtered.length}); return;
      }
      json(response, 404, {error: 'not_found'});
    } catch (error) { exceptions.push(String(error)); json(response, 500, {error: 'fixture_failed'}); }
  });
  await new Promise((resolve, reject) => { server.once('error', reject); server.listen(0, '127.0.0.1', resolve); });
  const origin = 'http://127.0.0.1:' + server.address().port;
  browser = spawn(executable, ['--headless', '--no-sandbox', '--disable-dev-shm-usage', '--disable-background-networking', '--disable-gpu', '--no-first-run', '--no-default-browser-check', '--remote-debugging-port=0', '--user-data-dir=' + temporary, 'about:blank'], {stdio: ['ignore', 'ignore', 'pipe']});
  let log = '';
  browser.stderr.on('data', chunk => { log = (log + chunk.toString()).slice(-8000); });
  browser.on('error', error => exceptions.push(String(error)));
  let port;
  await waitFor(async () => {
    if (browser.exitCode !== null || browser.signalCode !== null) throw new Error('Chromium exited: ' + log);
    try { port = (await readFile(join(temporary, 'DevToolsActivePort'), 'utf8')).split('\n')[0]; return !!port; } catch { return false; }
  }, 'Chromium did not start', 30000);
  const target = (await (await fetch('http://127.0.0.1:' + port + '/json/list')).json()).find(target => target.type === 'page');
  socket = new WebSocket(target.webSocketDebuggerUrl);
  await new Promise((resolve, reject) => { socket.addEventListener('open', resolve, {once: true}); socket.addEventListener('error', reject, {once: true}); });
  let serial = 0;
  const pending = new Map();
  function cdp(method, params = {}) {
    return new Promise((resolve, reject) => {
      const id = ++serial, timer = setTimeout(() => { pending.delete(id); reject(new Error('CDP timeout: ' + method)); }, 15000);
      pending.set(id, {resolve, reject, timer}); socket.send(JSON.stringify({id, method, params}));
    });
  }
  socket.addEventListener('message', event => {
    const data = JSON.parse(event.data);
    if (data.id) {
      const operation = pending.get(data.id); if (!operation) return;
      clearTimeout(operation.timer); pending.delete(data.id); data.error ? operation.reject(new Error(JSON.stringify(data.error))) : operation.resolve(data.result);
    }
    if (data.method === 'Runtime.exceptionThrown') pageErrors.push(data.params.exceptionDetails);
    if (data.method === 'Log.entryAdded' && data.params.entry.source === 'security') pageErrors.push(data.params.entry.text);
  });
  async function evaluate(expression) {
    const result = await cdp('Runtime.evaluate', {expression, returnByValue: true, awaitPromise: true});
    assert.ok(!result.exceptionDetails, JSON.stringify(result.exceptionDetails)); return result.result.value;
  }
  async function clickWhenEnabled(id) {
    await waitFor(() => evaluate(`(() => {
      const button = document.getElementById(${JSON.stringify(id)});
      if (!button || button.disabled) return false;
      button.click(); return true;
    })()`), 'Button did not become enabled: ' + id);
  }
  async function assertOverviewRules() {
    assert.equal(await evaluate('document.querySelector("[data-view=overview]").hidden'), false, 'Home switch navigated away from the overview');
    assert.equal(await evaluate('document.querySelector("[data-view=query]").hidden'), true, 'Home switch opened the settings editor');
    assert.equal(await evaluate('document.getElementById("auto-wake").disabled && !document.getElementById("auto-wake").checked'), true, 'Automatic wake must remain unavailable');
    assert.equal(await evaluate('document.getElementById("rules-message").getAttribute("role")'), 'status');
    assert.equal(await evaluate('document.getElementById("rules-message").hidden'), false, 'Overview rule feedback is hidden');
    assert.notEqual(await evaluate('document.getElementById("rules-message").textContent.trim()'), '', 'Overview rule feedback is empty');
  }
  async function assertRulesLocked() {
    assert.equal(await evaluate('document.getElementById("five-hour-query").disabled && document.getElementById("weekly-query").disabled'), true, 'Both home switches must be locked');
    const count = posts.length;
    await evaluate('for(const id of ["five-hour-query","weekly-query"]){const field=document.getElementById(id);field.click();field.dispatchEvent(new Event("change"));}');
    await new Promise(resolve => setTimeout(resolve, 150));
    assert.equal(posts.length, count, 'Locked home switches dispatched another write');
  }
  async function editorDraft() {
    return evaluate('Array.from(document.querySelectorAll("#settings-fields input, #settings-fields select, #settings-fields textarea"), field => [field.id, field.type === "checkbox" ? field.checked : field.value])');
  }
  async function verifyRules(draft) {
    const postCount = posts.length, readCount = reads.filter(path => path.endsWith('/settings')).length;
    settingsReadGate = gate();
    await clickWhenEnabled('rules-reload');
    await waitFor(() => settingsReadGate.started, 'Rules reload did not GET settings');
    await assertOverviewRules();
    await assertRulesLocked();
    assert.equal(await evaluate('document.getElementById("rules-reload").disabled'), true, 'Verification button was not locked');
    await evaluate('document.getElementById("rules-reload").click()');
    assert.deepEqual(await editorDraft(), draft, 'Starting verification discarded editor drafts');
    settingsReadGate.release(); settingsReadGate = undefined;
    await waitFor(() => evaluate('!document.getElementById("five-hour-query").disabled && !document.getElementById("weekly-query").disabled && document.getElementById("rules-reload").hidden'), 'GET settings did not unlock home switches');
    await assertOverviewRules();
    assert.equal(posts.length, postCount, 'Verification dispatched a write');
    assert.equal(reads.filter(path => path.endsWith('/settings')).length, readCount + 1, 'Verification did not issue exactly one settings read');
    assert.deepEqual(await editorDraft(), draft, 'Verification discarded editor drafts');
    assert.equal(await evaluate('document.getElementById("five-hour-query").checked'), queryConfig.on_five_hour_expiry, 'Verification did not restore the actual five-hour state');
    assert.equal(await evaluate('document.getElementById("weekly-query").checked'), queryConfig.on_weekly_expiry, 'Verification did not restore the actual weekly state');
  }
  async function screenshot(name) {
    await mkdir(output, {recursive: true});
    const capture = await cdp('Page.captureScreenshot', {format: 'png', captureBeyondViewport: false});
    await writeFile(join(output, name), Buffer.from(capture.data, 'base64'));
  }
  await cdp('Page.enable'); await cdp('Runtime.enable'); await cdp('Log.enable');
  await cdp('Page.addScriptToEvaluateOnNewDocument', {source: `localStorage.setItem('cli-proxy-auth', JSON.stringify({state:{managementKey:${JSON.stringify(fixtureKey)},apiBase:${JSON.stringify(origin + '/cpa')}}}));`});
  await cdp('Emulation.setDeviceMetricsOverride', {width: 1586, height: 992, deviceScaleFactor: 1, mobile: false});
  await cdp('Page.navigate', {url: origin + base + 'status'});
  await waitFor(() => evaluate('document.getElementById("login-section")?.hidden && document.getElementById("accounts")?.rows.length === 6'), 'Dashboard did not auto-connect');
  await evaluate('document.fonts.ready.then(() => true)');
  assert.equal(await evaluate('document.getElementById("metric-total").textContent'), '24');
  assert.equal(await evaluate('document.getElementById("metric-capacity").textContent'), '16 / 20');
  assert.equal(posts.length, 0, 'Opening the page dispatched a write');
  assert.equal(await evaluate('document.querySelector("progress").value'), 82);
  assert.equal(await evaluate('document.getElementById("auto-wake").disabled'), true);
  assert.equal(await evaluate('document.getElementById("five-hour-query").disabled'), false);
  assert.match(await evaluate('document.getElementById("accounts").rows[0].textContent'), /等待查询 · 手动批量.*后计划查询/);
  assert.equal(await evaluate('document.getElementById("accounts").rows[0].querySelector("button").textContent'), '立即查询');
  assert.match(await evaluate('Array.from(document.querySelectorAll("#tasks .task-item")).find(node=>node.textContent.includes("等待 23")).querySelector(".task-heading").textContent'), /查询中/);
  assert.match(await evaluate('Array.from(document.querySelectorAll("#tasks .task-item")).find(node=>node.textContent.includes("处于排除时段")).textContent'), /等待查询.*已到计划时间/s);
  assert.equal(await evaluate('document.body.textContent.includes("近期活跃用户")'), false);
  assert.equal(await evaluate('document.documentElement.scrollWidth <= document.documentElement.clientWidth'), true, 'Desktop overflow');
  await screenshot('desktop.png');
  assert.equal(await evaluate('document.getElementById("sidebar-toggle").getAttribute("aria-expanded")'), 'true');
  const mainBeforeCollapse = await evaluate('document.querySelector(".main-content").getBoundingClientRect().left');
  await evaluate('document.getElementById("sidebar-toggle").click()');
  assert.equal(await evaluate('document.getElementById("sidebar-toggle").getAttribute("aria-expanded")'), 'false');
  assert.equal(await evaluate('Math.round(document.getElementById("app-sidebar").getBoundingClientRect().width)'), 76);
  assert.ok(await evaluate('document.querySelector(".main-content").getBoundingClientRect().left') < mainBeforeCollapse);
  assert.equal(await evaluate('document.querySelector(".nav-item[data-page=models]").getAttribute("aria-label")'), '模型与提示词');
  assert.equal(await evaluate('document.documentElement.scrollWidth <= innerWidth'), true, 'Collapsed desktop overflow');
  await screenshot('sidebar-collapsed.png');
  await cdp('Page.reload');
  await waitFor(() => evaluate('document.getElementById("login-section")?.hidden && document.getElementById("accounts")?.rows.length===6'), 'Reload did not reconnect after sidebar collapse');
  assert.equal(await evaluate('document.getElementById("sidebar-toggle").getAttribute("aria-expanded")'), 'false', 'Collapsed preference was not restored');
  assert.equal(posts.length, 0, 'Collapsing or reloading sidebar dispatched a write');
  await evaluate('document.getElementById("sidebar-toggle").click()');
  assert.equal(await evaluate('document.getElementById("sidebar-toggle").getAttribute("aria-expanded")'), 'true');
  assert.equal(reads.filter(path => path.endsWith('/generation-accounts')).length, 0, 'Overview fetched the generation directory before visiting models');
  for (const page of ['accounts', 'query', 'wake', 'models', 'history', 'diagnostics', 'overview']) {
    await evaluate(`document.querySelector('[data-page="${page}"]').click()`);
    assert.equal(await evaluate(`document.querySelector('[data-view="${page}"]').hidden`), false, 'Navigation failed: ' + page);
  }
  assert.equal(posts.length, 0, 'Navigating dispatched a write');
  assert.equal(reads.some(path => path.endsWith('/settings')), false, 'Overview config should avoid redundant settings reads');
  const initialConfig = structuredClone(queryConfig);
  settingsPostGate = gate();
  await evaluate('document.getElementById("five-hour-query").click()');
  await waitFor(() => settingsPostGate.started, 'Home switch did not POST settings directly');
  assert.deepEqual(JSON.parse(posts[0].body), {revision: 1, config: {...initialConfig, on_five_hour_expiry: true}});
  await assertOverviewRules();
  await assertRulesLocked();
  assert.equal(await evaluate('document.getElementById("five-hour-query").checked'), true, 'Pending home switch lost its intended state');
  assert.equal(await evaluate('document.getElementById("settings-five-hour").checked'), false, 'Pending home switch modified the editor');
  const readsBeforeRuleSave = reads.filter(path => path.endsWith('/overview')).length;
  await waitFor(() => reads.filter(path => path.endsWith('/overview')).length > readsBeforeRuleSave, 'Pending rule save was not polled');
  await assertRulesLocked();
  settingsPostGate.release(); settingsPostGate = undefined;
  await waitFor(() => evaluate('document.getElementById("five-hour-query").checked && !document.getElementById("five-hour-query").disabled'), 'Home switch save not applied');
  await assertOverviewRules();
  assert.match(await evaluate('document.getElementById("rules-message").textContent'), /已.*(?:保存|应用|生效)/);
  assert.equal(posts.length, 1, 'Home switch submitted more than once');
  assert.equal(settingsRevision, 2);
  assert.deepEqual(queryConfig, {...initialConfig, on_five_hour_expiry: true});
  assert.equal(reads.some(path => path.endsWith('/settings')), false, 'Direct save ignored the overview settings snapshot');
  await evaluate('document.querySelector("[data-page=query]").click()');
  assert.equal(await evaluate('document.getElementById("settings-five-hour").checked'), true);
  assert.equal(await evaluate('document.getElementById("five-hour-query").checked'), true, 'Home switch must reflect saved state');
  await evaluate('document.getElementById("settings-interval").value="90";document.getElementById("settings-interval").dispatchEvent(new Event("input"))');
  const readsBeforeDraft = reads.filter(path => path.endsWith('/overview')).length;
  await waitFor(() => reads.filter(path => path.endsWith('/overview')).length > readsBeforeDraft, 'Settings did not receive actual polling');
  assert.equal(await evaluate('document.getElementById("settings-interval").value'), '90', 'Polling overwrote a dirty draft');
  await evaluate('document.querySelector("[data-page=overview]").click();document.querySelector("[data-page=query]").click()');
  assert.equal(await evaluate('document.getElementById("settings-interval").value'), '90', 'Navigation lost the draft');
  assert.equal(posts.length, 1, 'Editing dispatched an automatic write');
  await evaluate('for(const [id,value] of [["settings-interval","20"],["settings-delay-max","4"],["settings-timezone","Mars/Colony"],["settings-excluded","00:00-01:00, 00:30-02:00"]]){const field=document.getElementById(id);field.value=value;field.dispatchEvent(new Event("input"));}');
  assert.equal(await evaluate('document.getElementById("settings-save").disabled'), true);
  assert.equal(await evaluate('document.getElementById("settings-validation").hidden'), false);
  await evaluate('document.getElementById("query-settings-form").dispatchEvent(new Event("submit",{cancelable:true}))');
  assert.equal(posts.length, 1, 'Invalid settings were submitted');
  await evaluate('for(const [id,value] of [["settings-interval","60"],["settings-delay-min","0"],["settings-delay-max","60"],["settings-timezone","Asia/Shanghai"],["settings-excluded","23:00-07:00, 12:00-13:00"]]){const field=document.getElementById(id);field.value=value;field.dispatchEvent(new Event("input"));}');
  assert.match(await evaluate('document.getElementById("settings-diff").textContent'), /300 → 60/);
  await screenshot('query-settings-desktop.png');
  await evaluate('document.getElementById("settings-save").click()');
  await waitFor(() => evaluate('document.getElementById("settings-message").textContent.startsWith("已保存并应用")'), 'Settings save not acknowledged');
  const settingsPost = posts.filter(post => post.resource === 'settings')[1];
  assert.deepEqual(JSON.parse(settingsPost.body), {revision: 2, config: {...queryConfig}});
  assert.equal(await evaluate('document.getElementById("five-hour-query").checked'), true);
  queryConfig = {...queryConfig, automatic: true, interval_seconds: 120, on_weekly_expiry: true}; settingsRevision++; revision++;
  await waitFor(() => evaluate('document.getElementById("settings-interval").value==="120" && document.getElementById("settings-weekly").checked'), 'Clean editor did not follow backend settings polling');
  assert.equal(posts.filter(post => post.resource === 'settings').length, 2, 'Polling wrote settings');
  await evaluate('document.getElementById("settings-interval").value="90";document.getElementById("settings-interval").dispatchEvent(new Event("input"))');
  settingsConflictOnce = true;
  await evaluate('document.getElementById("settings-save").click()');
  await waitFor(() => evaluate('!document.getElementById("settings-conflict").hidden'), '409 conflict was not displayed');
  const readsBeforeConflict = reads.filter(path => path.endsWith('/overview')).length;
  await waitFor(() => reads.filter(path => path.endsWith('/overview')).length > readsBeforeConflict, 'Conflict was not polled');
  assert.equal(await evaluate('document.getElementById("settings-interval").value'), '90', 'Conflict polling discarded the draft');
  assert.equal(await evaluate('document.getElementById("settings-save").disabled'), true);
  await evaluate('document.getElementById("settings-reload").click()');
  await waitFor(() => evaluate('document.getElementById("settings-interval").value==="180" && document.getElementById("settings-conflict").hidden'), 'Explicit reload did not discard conflicting draft');
  assert.ok(reads.some(path => path.endsWith('/settings')), 'Reload did not read actual settings endpoint');
  assert.equal(posts.filter(post => post.resource === 'settings').length, 3, 'Conflict was automatically retried');
  queryConfig = {...queryConfig, timezone: 'Factory'}; settingsRevision++; revision++;
  await waitFor(() => evaluate('document.getElementById("settings-timezone").value==="Factory"'), 'Server timezone alias broke overview polling');
  assert.equal(await evaluate('document.getElementById("error").hidden && document.getElementById("metric-total").textContent==="24"'), true, 'Unsupported browser timezone made the overview unusable');
  await evaluate('document.getElementById("settings-reload").onclick()');
  assert.equal(await evaluate('document.getElementById("settings-timezone").value'), 'Factory');
  assert.equal(await evaluate('document.getElementById("error").hidden'), true, 'Server timezone alias broke explicit settings reload');
  queryConfig = {...queryConfig, timezone: 'Asia/Shanghai'}; settingsRevision++; revision++;
  await waitFor(() => evaluate('document.getElementById("settings-timezone").value==="Asia/Shanghai" && document.getElementById("settings-validation").hidden'), 'Supported timezone did not recover the editor');
  await evaluate('for(const [id,value] of [["settings-interval","90"],["settings-timezone","UTC"],["settings-delay-min","9"],["settings-delay-max","45"],["settings-excluded","01:00-02:00"]]){const field=document.getElementById(id);field.value=value;field.dispatchEvent(new Event("input"));}for(const id of ["settings-automatic","settings-five-hour","settings-weekly"]){const field=document.getElementById(id);field.checked=false;field.dispatchEvent(new Event("input"));}');
  const isolatedDraft = await editorDraft(), savedBeforeWeekly = structuredClone(queryConfig), revisionBeforeWeekly = settingsRevision;
  assert.deepEqual(isolatedDraft.map(([id]) => id), ['settings-automatic', 'settings-five-hour', 'settings-weekly', 'settings-interval', 'settings-timezone', 'settings-delay-min', 'settings-delay-max', 'settings-excluded'], 'Draft snapshot omitted settings fields');
  assert.equal(posts.filter(post => post.resource === 'settings').length, 3, 'Editor draft dispatched a write');
  await evaluate('document.querySelector("[data-page=overview]").click()');
  settingsPostGate = gate();
  await clickWhenEnabled('weekly-query');
  await waitFor(() => settingsPostGate.started, 'Weekly home switch did not save');
  assert.deepEqual(JSON.parse(posts.at(-1).body), {revision: revisionBeforeWeekly, config: {...savedBeforeWeekly, on_weekly_expiry: false}}, 'Weekly save included unrelated editor drafts');
  await assertOverviewRules();
  await assertRulesLocked();
  assert.equal(await evaluate('document.getElementById("weekly-query").checked'), false, 'Pending weekly switch lost its intended state');
  assert.equal(await evaluate('document.getElementById("settings-fields").disabled && document.getElementById("settings-save").disabled'), true, 'Editor can race a home switch save');
  assert.deepEqual(await editorDraft(), isolatedDraft, 'Pending weekly save modified editor drafts');
  settingsPostGate.release(); settingsPostGate = undefined;
  await waitFor(() => evaluate('!document.getElementById("weekly-query").checked && !document.getElementById("weekly-query").disabled'), 'Weekly home save not applied');
  await assertOverviewRules();
  assert.match(await evaluate('document.getElementById("rules-message").textContent'), /周限额到期查询已关闭并保存/);
  assert.deepEqual(queryConfig, {...savedBeforeWeekly, on_weekly_expiry: false}, 'Weekly save changed other saved settings');
  assert.equal(settingsRevision, revisionBeforeWeekly + 1);
  assert.deepEqual(await editorDraft(), isolatedDraft, 'Successful weekly save discarded unrelated drafts');
  const savedBeforeFailure = structuredClone(queryConfig), revisionBeforeFailure = settingsRevision;
  settingsPostGate = gate('reject');
  await clickWhenEnabled('five-hour-query');
  await waitFor(() => settingsPostGate.started, 'Rejected home switch was not submitted');
  assert.deepEqual(JSON.parse(posts.at(-1).body), {revision: revisionBeforeFailure, config: {...savedBeforeFailure, on_five_hour_expiry: false}});
  await assertRulesLocked();
  settingsPostGate.release(); settingsPostGate = undefined;
  await waitFor(() => evaluate('!document.getElementById("five-hour-query").disabled && document.getElementById("rules-message").textContent.includes("未保存")'), 'Known failure did not restore the home switch');
  await assertOverviewRules();
  assert.equal(await evaluate('document.getElementById("five-hour-query").checked'), true, 'Known failure did not restore saved state');
  assert.equal(await evaluate('document.getElementById("rules-reload").hidden'), true, 'Known rejection was treated as an unknown result');
  assert.deepEqual(queryConfig, savedBeforeFailure);
  assert.equal(settingsRevision, revisionBeforeFailure);
  assert.deepEqual(await editorDraft(), isolatedDraft, 'Rejected save discarded editor drafts');
  settingsConflictOnce = true;
  await clickWhenEnabled('five-hour-query');
  await waitFor(() => evaluate('!document.getElementById("rules-reload").hidden && document.getElementById("rules-message").textContent.includes("后台设置已变化")'), 'Home 409 did not offer verification');
  assert.deepEqual(JSON.parse(posts.at(-1).body), {revision: revisionBeforeFailure, config: {...savedBeforeFailure, on_five_hour_expiry: false}});
  await assertOverviewRules();
  assert.equal(await evaluate('document.getElementById("five-hour-query").checked'), true, '409 did not restore last-known state');
  await assertRulesLocked();
  const postsAfterRuleConflict = posts.length, readsBeforeRuleConflict = reads.filter(path => path.endsWith('/overview')).length;
  await waitFor(() => reads.filter(path => path.endsWith('/overview')).length > readsBeforeRuleConflict, 'Home conflict was not polled');
  await assertRulesLocked();
  assert.equal(posts.length, postsAfterRuleConflict, 'Home conflict was automatically retried');
  assert.deepEqual(await editorDraft(), isolatedDraft, 'Home conflict polling discarded editor drafts');
  assert.equal(await evaluate('document.getElementById("settings-conflict").hidden'), false, 'Backend revision conflict was not retained in the editor');
  await verifyRules(isolatedDraft);
  assert.equal(await evaluate('document.getElementById("settings-conflict").hidden'), false, 'Rules verification cleared the editor conflict');
  const revisionBeforeDisconnect = settingsRevision, savedBeforeDisconnect = structuredClone(queryConfig);
  settingsPostGate = gate('disconnect');
  await clickWhenEnabled('five-hour-query');
  await waitFor(() => settingsPostGate.started, 'Uncertain home switch was not submitted');
  assert.deepEqual(JSON.parse(posts.at(-1).body), {revision: revisionBeforeDisconnect, config: {...savedBeforeDisconnect, on_five_hour_expiry: false}}, 'Home save used the stale editor revision/config');
  await assertRulesLocked();
  settingsPostGate.release(); settingsPostGate = undefined;
  try {
    await waitFor(() => evaluate('!document.getElementById("rules-reload").hidden && document.getElementById("rules-message").textContent.includes("尚未确认")'), 'Disconnected response did not require verification');
  } catch (error) {
    throw new Error(error.message + ': ' + JSON.stringify(await evaluate('({message:document.getElementById("rules-message").textContent,reloadHidden:document.getElementById("rules-reload").hidden,disabled:document.getElementById("five-hour-query").disabled,status:document.getElementById("rules-status").textContent})')), {cause: error});
  }
  await assertOverviewRules();
  assert.equal(await evaluate('document.getElementById("five-hour-query").checked'), true, 'Unknown result did not restore last-known state');
  assert.equal(queryConfig.on_five_hour_expiry, false, 'Disconnect fixture must commit before dropping the response');
  assert.equal(settingsRevision, revisionBeforeDisconnect + 1);
  await assertRulesLocked();
  const postsAfterDisconnect = posts.length, readsBeforeDisconnect = reads.filter(path => path.endsWith('/overview')).length;
  await waitFor(() => reads.filter(path => path.endsWith('/overview')).length > readsBeforeDisconnect, 'Unknown home save was not polled');
  await assertRulesLocked();
  assert.equal(posts.length, postsAfterDisconnect, 'Unknown home save was automatically retried');
  await verifyRules(isolatedDraft);
  assert.equal(await evaluate('document.getElementById("settings-conflict").hidden'), false, 'Unknown-result verification cleared the editor conflict');
  await evaluate('document.querySelector("[data-page=query]").click()');
  assert.deepEqual(await editorDraft(), isolatedDraft, 'Navigation after rules verification discarded editor drafts');
  await evaluate('document.getElementById("settings-reload").click()');
  await waitFor(() => evaluate('document.getElementById("settings-interval").value==="180" && document.getElementById("settings-conflict").hidden'), 'Explicit editor reload did not discard the preserved draft');
  await evaluate('document.getElementById("settings-interval").value="150";document.getElementById("settings-interval").dispatchEvent(new Event("input"))');
  const uncertainEditorDraft = await editorDraft(), revisionBeforeEditorSave = settingsRevision, savedBeforeEditorSave = structuredClone(queryConfig);
  assert.equal(posts.filter(post => post.resource === 'settings').length, 7, 'Editing or rules verification dispatched a write');
  settingsPostGate = gate('disconnect');
  await clickWhenEnabled('settings-save');
  await waitFor(() => settingsPostGate.started, 'Explicit editor save did not submit');
  assert.deepEqual(JSON.parse(posts.at(-1).body), {revision: revisionBeforeEditorSave, config: {...savedBeforeEditorSave, interval_seconds: 150}});
  settingsPostGate.release(); settingsPostGate = undefined;
  await waitFor(() => evaluate('document.getElementById("settings-message").textContent.includes("尚未确认") && !document.getElementById("rules-reload").hidden'), 'Unknown editor save did not require verification');
  await evaluate('document.querySelector("[data-page=overview]").click()');
  await assertOverviewRules();
  await assertRulesLocked();
  await verifyRules(uncertainEditorDraft);
  assert.equal(await evaluate('document.getElementById("settings-conflict").hidden'), false, 'Editor save uncertainty was cleared without discarding its draft');
  await evaluate('document.querySelector("[data-page=query]").click();document.getElementById("settings-reload").click()');
  await waitFor(() => evaluate('document.getElementById("settings-interval").value==="150" && document.getElementById("settings-conflict").hidden'), 'Explicit editor reload did not resolve its uncertain save');
  assert.equal(posts.filter(post => post.resource === 'settings').length, 8, 'Settings were automatically retried');
  assert.equal(settingsRevision, 11, 'Unexpected settings revision after explicit saves and verification');
  await evaluate('document.querySelector("[data-page=overview]").click()');
  const countdownBefore = await evaluate('document.querySelector(".query-pending").textContent');
  await new Promise(resolve => setTimeout(resolve, 2100));
  assert.notEqual(await evaluate('document.querySelector(".query-pending").textContent'), countdownBefore, 'Queued countdown did not update');
  await evaluate('document.getElementById("accounts").querySelector("button").click()');
  await waitFor(() => posts.some(post => post.resource === 'jobs'), 'Single account query not dispatched');
  assert.deepEqual(JSON.parse(posts.find(post => post.resource === 'jobs').body), {kind: 'quota_query', account_id: 'codex-01'});
  assert.ok(posts.find(post => post.resource === 'jobs').headers['idempotency-key']);
  await waitFor(() => evaluate('!document.getElementById("accounts").querySelector("button").disabled'), 'Single query did not restore its button');
  await waitFor(() => evaluate('!document.getElementById("accounts").rows[0].querySelector(".query-pending")'), 'Single query did not promote queued account');
  await evaluate('document.getElementById("query").click()');
  await waitFor(() => posts.filter(post => post.resource === 'jobs').length === 2, 'Batch query not dispatched');
  assert.deepEqual(JSON.parse(posts.filter(post => post.resource === 'jobs')[1].body), {kind: 'quota_query', account_id: ''});
  await waitFor(() => evaluate('document.getElementById("accounts").rows[0].textContent.includes("处于排除时段")'), 'Queue-only revision did not reload account pending state');
  assert.equal(await evaluate('document.getElementById("accounts").rows[0].querySelector("button").textContent'), '立即查询');
  await evaluate('document.querySelector("[data-page=history]").click()');
  assert.match(await evaluate('document.getElementById("history-list").textContent'), /处于排除时段/);
  assert.match(await evaluate('document.getElementById("history-list").textContent'), /手动批量/);
  await evaluate('document.querySelector("[data-page=overview]").click()');
  await waitFor(() => evaluate('!document.getElementById("query").disabled'), 'Batch submission did not finish');
  accountReadGate = {started: false};
  accountReadGate.promise = new Promise(resolve => { accountReadGate.release = resolve; });
  await evaluate('document.getElementById("sync").click()');
  await waitFor(() => posts.some(post => post.resource === 'sync'), 'CPA sync not dispatched');
  assert.equal(posts.find(post => post.resource === 'sync').body, '');
  await waitFor(() => accountReadGate.started, 'Sync did not refresh accounts');
  assert.equal(await evaluate('document.getElementById("next").disabled'), true, 'Pagination must wait for the pending account read');
  await evaluate('document.getElementById("next").click()');
  assert.equal(reads.some(path => path.includes('/accounts?') && path.includes('&after=')), false, 'Disabled pagination dispatched a request');
  accountReadGate.release(); accountReadGate = undefined;
  await waitFor(() => evaluate('!document.getElementById("sync").disabled'), 'CPA sync did not finish');
  await clickWhenEnabled('next');
  await waitFor(() => evaluate('document.getElementById("accounts-page-info").textContent === "7–12 / 24" && document.getElementById("accounts").rows[0]?.querySelector("strong")?.textContent === "codex-07"'), 'Next account page not displayed');
  assert.deepEqual(await evaluate('Array.from(document.querySelectorAll("#accounts .account-name strong"), node => node.textContent)'), accounts.slice(6, 12).map(account => account.id));
  assert.ok(reads.some(path => path === api + 'accounts?limit=6&after=codex-06&revision=1'), 'Next page did not use the account cursor and revision');
  await clickWhenEnabled('previous');
  await waitFor(() => evaluate('document.getElementById("accounts-page-info").textContent === "1–6 / 24" && document.getElementById("accounts").rows[0]?.querySelector("strong")?.textContent === "codex-01"'), 'Previous account page not displayed');
  assert.deepEqual(await evaluate('Array.from(document.querySelectorAll("#accounts .account-name strong"), node => node.textContent)'), accounts.slice(0, 6).map(account => account.id));
  await evaluate('document.getElementById("accounts-search").value="codex-24";document.getElementById("accounts-search").dispatchEvent(new Event("input"))');
  await waitFor(() => evaluate('document.getElementById("accounts").textContent.includes("codex-24") && document.getElementById("accounts").rows.length===1'), 'Full-pool search not applied');
  assert.match(await evaluate('document.getElementById("accounts-page-info").textContent'), /1–1 \/ 1/);
  await evaluate('document.getElementById("accounts-search").value="";document.getElementById("accounts-search").dispatchEvent(new Event("input"))');
  await waitFor(() => evaluate('document.getElementById("accounts").rows.length===6'), 'Search did not reset');
  accounts[0].quota.five_hour.Remaining.Value = 11; revision++; accountsRevision++;
  await waitFor(() => evaluate('document.querySelector("progress").value===11'), 'Background refresh not applied');
  assert.equal(reads.filter(path => path.endsWith('/generation-accounts')).length, 1, 'Overview or quota actions refreshed the generation directory');
  await evaluate('document.querySelector("[data-page=models]").click()');
  await waitFor(() => evaluate('!document.getElementById("model-fields").disabled'), 'Model settings did not load');
  await waitFor(() => evaluate('document.getElementById("generation-account-options").options.length===3'), 'Generation directory did not load');
  assert.equal(reads.filter(path => path.endsWith('/generation-accounts')).length, 1, 'Opening models fetched the directory more than once');
  assert.equal(await evaluate('document.getElementById("generation-account").value'), '', 'Directory silently selected an account');
  assert.match(await evaluate('document.getElementById("generation-account-options").textContent'), /反重力.*已禁用/);
  assert.equal(await evaluate('document.getElementById("metric-total").textContent'), '24', 'Antigravity accounts leaked into quota metrics');
  assert.equal(await evaluate('document.getElementById("generation-prompt").value'), modelConfig.generation_prompt, 'Saved generation prompt did not load');
  const modelWritesBeforeEditing = posts.length;
  await clickWhenEnabled('generation-reset-prompt');
  assert.equal(await evaluate('document.getElementById("generation-prompt").value'), '生成简短、自然、适合日常问候的中文短句，避免重复。', 'Prompt reset did not stage the default Chinese prompt');
  assert.equal(posts.length, modelWritesBeforeEditing, 'Prompt reset caused a write');
  await evaluate('document.getElementById("generation-prompt").value="保存的候选提示词\\n请生成自然问候。";document.getElementById("generation-prompt").dispatchEvent(new Event("input"))');
  await evaluate('for(const [id,value] of [["model-default","fixture-model"],["model-phrases","第一句\\n第二句\\n第一句"],["model-prompt-mode","random"]]){const node=document.getElementById(id);node.value=value.replaceAll("\\\\n","\\n");node.dispatchEvent(new Event("input"));}');
  assert.equal(await evaluate('document.getElementById("model-save").disabled'), true, 'Duplicate phrases were accepted');
  await evaluate('document.getElementById("model-deduplicate").click()');
  assert.equal(await evaluate('document.getElementById("model-confirmation").hidden'), false, 'Deduplication lacked confirmation');
  await evaluate('document.getElementById("model-confirm-action").click()');
  assert.equal(await evaluate('document.getElementById("model-phrases").value.split("\\n").length'), 2);
  await evaluate('document.getElementById("model-transfer").value="第三句";document.getElementById("model-import").click()');
  assert.equal(await evaluate('document.getElementById("model-phrases").value.split("\\n").length'), 3);
  await evaluate('document.getElementById("model-export").click()');
  assert.match(await evaluate('document.getElementById("model-transfer").value'), /第三句/);
  await evaluate('document.getElementById("model-preview-rule").value="five_hour";document.getElementById("model-preview").click()');
  assert.match(await evaluate('document.getElementById("model-preview-result").textContent'), /fixture-model/);
  assert.match(await evaluate('document.getElementById("model-preview-result").textContent'), /继承默认模型/);
  assert.equal(posts.length, modelWritesBeforeEditing, 'Local phrase editing or preview dispatched a request');
  await screenshot('model-settings-desktop.png');
  await clickWhenEnabled('model-save');
  await waitFor(async () => modelRevision === 1 && await evaluate('!document.getElementById("model-fields").disabled && document.getElementById("model-save").disabled && document.getElementById("model-message").textContent.startsWith("配置已保存")'), 'Model settings were not saved');
  assert.equal(modelConfig.default_model, 'fixture-model'); assert.equal(modelConfig.phrases.length, 3);
  assert.equal(modelConfig.generation_prompt, '保存的候选提示词\n请生成自然问候。', 'Explicit model save did not persist the generation prompt');
  assert.deepEqual(JSON.parse(posts.find(post => post.resource === 'model-settings').body), {revision: 0, config: modelConfig});
  await evaluate('document.getElementById("model-default").value="unsaved-model";document.getElementById("model-default").dispatchEvent(new Event("input"))');
  const modelReadsBeforePoll = reads.filter(path => path.endsWith('/model-settings')).length;
  await waitFor(() => reads.filter(path => path.endsWith('/model-settings')).length > modelReadsBeforePoll, 'Active model editor did not poll saved settings');
  assert.equal(reads.filter(path => path.endsWith('/generation-accounts')).length, 1, 'Model polling also polled the generation directory');
  assert.equal(await evaluate('document.getElementById("model-default").value'), 'unsaved-model', 'Polling overwrote model draft');
  modelConflictOnce = true;
  await clickWhenEnabled('model-save');
  await waitFor(() => evaluate('!document.getElementById("model-conflict").hidden'), 'Model settings conflict not displayed');
  assert.equal(await evaluate('document.getElementById("model-default").value'), 'unsaved-model');
  assert.equal(posts.filter(post => post.resource === 'model-settings').length, 2, 'Model save retried automatically');
  await clickWhenEnabled('model-reload');
  await waitFor(() => evaluate('document.getElementById("model-default").value === "fixture-concurrent-model" && document.getElementById("model-conflict").hidden'), 'Explicit model reload did not resolve conflict');
  const poolBeforeGeneration = [...modelConfig.phrases];
  const writesBeforeDirectoryRefresh = posts.length;
  await evaluate('document.getElementById("generation-account").value="antigravity-disabled";document.getElementById("generation-account").dispatchEvent(new Event("input"))');
  await evaluate('document.getElementById("generation-consent").click()');
  assert.equal(await evaluate('document.getElementById("generation-submit").disabled'), true, 'Disabled Antigravity account was allowed');
  assert.match(await evaluate('document.getElementById("generation-account-help").textContent'), /反重力.*已禁用/);
  await clickWhenEnabled('generation-refresh-accounts');
  await waitFor(() => reads.filter(path => path.endsWith('/generation-accounts')).length === 2 && evaluate('!document.getElementById("generation-refresh-accounts").disabled'), 'Explicit generation directory refresh did not finish');
  assert.equal(await evaluate('document.getElementById("generation-account").value'), 'antigravity-disabled', 'Directory refresh silently replaced disabled selection');
  assert.equal(posts.length, writesBeforeDirectoryRefresh, 'Directory refresh sent a model or settings write');
  await evaluate('document.getElementById("generation-account").value="codex-01";document.getElementById("generation-account").dispatchEvent(new Event("input"));document.getElementById("generation-model").value="codex-fixture-model";document.getElementById("generation-account").value="antigravity-fixture";document.getElementById("generation-account").dispatchEvent(new Event("input"))');
  assert.equal(await evaluate('document.getElementById("generation-model").value'), '', 'Codex model was carried into Antigravity automatically');
  await evaluate('for(const [id,value] of [["generation-model","antigravity-fixture-model"],["generation-count","2"]]){const node=document.getElementById(id);node.value=value;node.dispatchEvent(new Event("input"));}');
  await evaluate('document.getElementById("generation-consent").click();document.getElementById("generation-prompt").value="本次草稿提示词\\n请生成简短、不重复的候选。";document.getElementById("generation-prompt").dispatchEvent(new Event("input"))');
  assert.equal(await evaluate('document.getElementById("generation-consent").checked'), false, 'Editing generation prompt retained spend consent');
  assert.equal(modelConfig.generation_prompt, '保存的候选提示词\n请生成自然问候。', 'Editing a generation draft automatically saved settings');
  assert.match(await evaluate('document.getElementById("generation-consent-label").textContent'), /反重力.*额度/);
  assert.equal(await evaluate('document.getElementById("generation-submit").disabled'), true, 'Generation does not require consent');
  await evaluate('document.getElementById("generation-consent").click()');
  await clickWhenEnabled('generation-submit');
  await waitFor(() => evaluate('!document.getElementById("generation-results").hidden'), 'Generated candidates did not appear');
  assert.equal(posts.filter(post => post.resource === 'prompt-generations').length, 1, 'Model generation submitted more than once');
  assert.deepEqual(JSON.parse(posts.find(post => post.resource === 'prompt-generations').body), {account_id: 'antigravity-fixture', model: 'antigravity-fixture-model', count: 2, prompt: '本次草稿提示词\n请生成简短、不重复的候选。'});
  assert.equal(await evaluate('document.getElementById("metric-total").textContent'), '24', 'Generation changed quota account totals');
  assert.deepEqual(modelConfig.phrases, poolBeforeGeneration, 'Generation replaced saved pool automatically');
  assert.equal(await evaluate('document.getElementById("generation-apply").disabled'), true, 'Candidates were selected without confirmation');
  await evaluate('document.getElementById("generation-select-all").click();document.getElementById("generation-apply").click()');
  assert.match(await evaluate('document.getElementById("model-phrases").value'), /浏览器候选短句 1/);
  assert.deepEqual(modelConfig.phrases, poolBeforeGeneration, 'Applying candidates automatically saved settings');
  await screenshot('model-generation-desktop.png');
  assert.equal(reads.some(path => /\/wake-(settings|jobs)/.test(path)), false, 'Legacy capability fallback read unsupported wake APIs');
  const beforeWakeReads = posts.length; wakeCapable = true; revision++;
  await evaluate('document.querySelector("[data-page=overview]").click()');
  await clickWhenEnabled('refresh');
  await waitFor(() => evaluate('!document.getElementById("auto-wake").disabled'), 'Wake capability did not enable overview controls');
  await evaluate('document.querySelector("[data-page=wake]").click()');
  await waitFor(() => evaluate('!document.getElementById("wake-settings-fields").disabled'), 'Wake settings did not load');
  assert.equal(posts.length, beforeWakeReads, 'Wake opening or reads dispatched a write');
  assert.equal(await evaluate('document.getElementById("wake-enabled").checked'), false, 'Automatic wake was not off by default');
  assert.equal(await evaluate('document.getElementById("wake-manual-submit").disabled'), true, 'Manual wake lacked spend consent');
  await evaluate('document.getElementById("wake-five-hour").click();document.getElementById("wake-enabled").click()');
  assert.equal(await evaluate('document.getElementById("wake-settings-save").disabled'), true, 'Enabled rules did not require spend consent');
  await evaluate('document.getElementById("wake-enabled").click();document.getElementById("wake-per-user").value="0.5";document.getElementById("wake-per-user").dispatchEvent(new Event("input"))');
  await clickWhenEnabled('wake-settings-save');
  await waitFor(() => wakeRevision === 1 && evaluate('!document.getElementById("wake-settings-fields").disabled'), 'Disabled wake rules did not save');
  assert.equal(wakeConfig.accounts_per_user, 0.5); assert.equal(wakeConfig.enabled, false);
  await evaluate('document.getElementById("wake-per-user").value="0.75";document.getElementById("wake-per-user").dispatchEvent(new Event("input"));document.querySelector("[data-page=overview]").click();document.getElementById("auto-wake").click()');
  assert.equal(posts.filter(post => post.resource === 'wake-settings').length, 1, 'Overview enabled wake without spend consent');
  assert.equal(await evaluate('document.getElementById("auto-wake").checked'), false);
  await evaluate('document.getElementById("wake-overview-consent").click();document.getElementById("auto-wake").click()');
  await waitFor(() => wakeRevision === 2 && evaluate('!document.getElementById("auto-wake").disabled'), 'Direct overview auto-wake save did not complete');
  assert.equal(wakeConfig.enabled, true); assert.equal(wakeConfig.accounts_per_user, 0.5, 'Overview saved an unrelated editor draft');
  assert.equal(await evaluate('document.querySelector("[data-view=overview]").hidden'), false, 'Wake switch navigated away');
  assert.equal(await evaluate('document.getElementById("wake-per-user").value'), '0.75', 'Overview save lost the rule draft');
  await evaluate('document.getElementById("auto-wake").click()');
  await waitFor(() => wakeRevision === 3 && evaluate('!document.getElementById("auto-wake").disabled'), 'Disabling automatic wake did not save without consent');
  assert.equal(wakeConfig.enabled, false);
  await evaluate('document.querySelector("[data-page=wake]").click()'); wakeConflictOnce = true;
  await clickWhenEnabled('wake-settings-save');
  await waitFor(() => evaluate('!document.getElementById("wake-settings-conflict").hidden'), 'Wake settings conflict did not appear');
  assert.equal(await evaluate('document.getElementById("wake-per-user").value'), '0.75', 'Wake conflict lost draft');
  assert.equal(posts.filter(post => post.resource === 'wake-settings').length, 4);
  await clickWhenEnabled('wake-settings-reload');
  await waitFor(() => evaluate('document.getElementById("wake-per-user").value==="0.5" && document.getElementById("wake-settings-conflict").hidden'), 'Explicit wake settings GET did not resolve conflict');
  await evaluate('document.getElementById("wake-account").value="codex-01";document.getElementById("wake-account").dispatchEvent(new Event("input"));document.getElementById("wake-manual-consent").click()');
  await clickWhenEnabled('wake-manual-submit');
  await waitFor(() => evaluate('document.getElementById("wake-manual-status").textContent==="结果未知"'), 'Manual wake unknown outcome was not read');
  assert.equal(posts.filter(post => post.resource === 'wake-jobs').length, 1, 'Manual wake submitted more than once');
  assert.deepEqual(JSON.parse(posts.find(post => post.resource === 'wake-jobs').body), {account_id: 'codex-01'});
  assert.equal(await evaluate('document.getElementById("wake-manual-fields").disabled'), true);
  await clickWhenEnabled('wake-history-refresh'); await clickWhenEnabled('wake-manual-verify');
  assert.equal(posts.filter(post => post.resource === 'wake-jobs').length, 1, 'Unknown wake verification resent the model action');
  assert.match(await evaluate('document.getElementById("wake-history").textContent'), /禁止重发/);
  assert.equal(await evaluate('document.getElementById("metric-total").textContent'), '24', 'Wake changed the quota account pool');
  await screenshot('wake-desktop.png');
  await evaluate('document.querySelector("[data-page=overview]").click()');
  for (const width of [768, 390]) {
    await cdp('Emulation.setDeviceMetricsOverride', {width, height: 844, deviceScaleFactor: 1, mobile: false});
    const layout = await evaluate('({width:document.documentElement.clientWidth,scroll:document.documentElement.scrollWidth,wide:Array.from(document.querySelectorAll("body *")).filter(node=>!node.closest(".navigation,.table-scroll")).map(node=>({tag:node.tagName,id:node.id,classes:typeof node.className==="string"?node.className:"",right:Math.round(node.getBoundingClientRect().right)})).filter(node=>node.right>document.documentElement.clientWidth+1).slice(0,12)})');
    assert.ok(layout.scroll <= layout.width, 'Responsive page overflow at ' + width + ': ' + JSON.stringify(layout));
    assert.equal(await evaluate('getComputedStyle(document.getElementById("account-card")).display !== "none"'), true);
    await screenshot('responsive-' + width + '.png');
    await evaluate('document.querySelector("[data-page=query]").click()');
    assert.equal(await evaluate('document.documentElement.scrollWidth <= document.documentElement.clientWidth'), true, 'Settings page overflow at ' + width);
    await screenshot('query-settings-' + width + '.png');
    await evaluate('document.querySelector("[data-page=models]").click()');
    assert.equal(await evaluate('document.documentElement.scrollWidth <= document.documentElement.clientWidth'), true, 'Model editor overflow at ' + width);
    await screenshot('model-settings-' + width + '.png');
    await evaluate('document.querySelector("[data-page=wake]").click()');
    assert.equal(await evaluate('document.documentElement.scrollWidth <= document.documentElement.clientWidth'), true, 'Wake editor overflow at ' + width);
    assert.equal(await evaluate('document.getElementById("wake-manual-fields").disabled'), true, 'Wake navigation unlocked an unknown model action');
    await screenshot('wake-' + width + '.png');
    if (width === 390) {
      await evaluate('document.getElementById("sidebar-toggle").click()');
      assert.equal(await evaluate('getComputedStyle(document.getElementById("primary-navigation")).display'), 'none', 'Mobile navigation did not collapse');
      assert.equal(await evaluate('document.documentElement.scrollWidth <= innerWidth'), true, 'Collapsed mobile overflow');
      await screenshot('sidebar-collapsed-mobile.png');
      await evaluate('document.getElementById("sidebar-toggle").click()');
      assert.notEqual(await evaluate('getComputedStyle(document.getElementById("primary-navigation")).display'), 'none', 'Mobile navigation did not expand');
    }
    await evaluate('document.querySelector("[data-page=overview]").click()');
  }
  await cdp('Emulation.setDeviceMetricsOverride', {width: 1586, height: 992, deviceScaleFactor: 1, mobile: false});
  unauthorized = true;
  await waitFor(() => evaluate('!document.getElementById("login-section").hidden && document.getElementById("query").disabled'), 'Expired authentication did not stop polling');
  const readCount = reads.length;
  await new Promise(resolve => setTimeout(resolve, 1800));
  assert.equal(reads.length, readCount, 'Authentication failure kept polling');
  assert.equal(exceptions.length, 0, JSON.stringify(exceptions));
  assert.equal(pageErrors.length, 0, JSON.stringify(pageErrors));
  assert.equal(posts.filter(post => post.resource === 'settings').length, 8, 'Unexpected settings write after browser checks');
  assert.equal(posts.filter(post => post.resource === 'model-settings').length, 2, 'Unexpected model settings write count');
  assert.equal(posts.filter(post => post.resource === 'prompt-generations').length, 1, 'Unexpected generation write count');
  assert.equal(posts.filter(post => post.resource === 'wake-settings').length, 4, 'Unexpected wake settings writes');
  assert.equal(posts.filter(post => post.resource === 'wake-jobs').length, 1, 'Unexpected model wake writes');
  assert.equal(posts.length, 19, 'Unexpected total API write count');
  assert.equal(posts.every(post => ['settings', 'jobs', 'sync', 'model-settings', 'prompt-generations', 'wake-settings', 'wake-jobs'].includes(post.resource)), true, 'Unexpected API write');
  assert.equal(reads.filter(path => path.endsWith('/generation-accounts')).length, 2, 'Navigation or polling unexpectedly refreshed generation accounts');
  console.log('V2 browser checks passed: desktop/mobile layout, direct overview settings saves/locking/draft isolation/rejections/conflicts/unknown-result GET verification, explicit editor saves, settings validation/drafts/polling/timezone aliases, mixed batches, overdue exclusion holds, queued countdowns, immediate single/randomized bulk query contracts, sync, filtering, paging, authentication, local generation directory, disabled Antigravity selection, explicit catalog refresh, editable generation prompt reset/save/draft/consent isolation, exact Antigravity generation prompt snapshot, wake rules and spend consent, direct auto-wake saves with draft isolation, wake settings conflicts and unknown manual-wake read-only verification.');
  console.log('Local fixture screenshots: ' + output);
} finally {
  accountReadGate?.release();
  settingsPostGate?.release();
  settingsReadGate?.release();
  if (socket) socket.close();
  if (browser && browser.exitCode === null && browser.signalCode === null) {
    browser.kill('SIGTERM');
    await Promise.race([new Promise(resolve => browser.once('exit', resolve)), new Promise(resolve => setTimeout(resolve, 3000))]);
    if (browser.exitCode === null && browser.signalCode === null) browser.kill('SIGKILL');
  }
  if (server) await new Promise(resolve => server.close(resolve));
  await rm(temporary, {recursive: true, force: true, maxRetries: 5, retryDelay: 200});
}
