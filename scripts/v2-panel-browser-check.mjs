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
let revision = 1, accountsRevision = 1, settingsRevision = 1, settingsConflictOnce = false, unauthorized = false, browser, server, socket;
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
function json(response, status, body) {
  response.writeHead(status, {'Content-Type': 'application/json', 'Cache-Control': 'no-store'}); response.end(JSON.stringify(body));
}
async function waitFor(check, message, timeout = 12000) {
  const until = Date.now() + timeout;
  while (Date.now() < until) { if (await check()) return; await new Promise(resolve => setTimeout(resolve, 50)); }
  throw new Error(message);
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
        if (resource === 'sync') { revision++; json(response, 202, {status: 'queued'}); return; }
        if (resource === 'settings') {
          const data = JSON.parse(body);
          assert.deepEqual(Object.keys(data).sort(), ['config', 'revision']);
          assert.deepEqual(Object.keys(data.config).sort(), ['automatic', 'excluded_hours', 'interval_seconds', 'on_five_hour_expiry', 'on_weekly_expiry', 'random_delay', 'timezone']);
          if (settingsConflictOnce) { settingsConflictOnce = false; settingsRevision++; revision++; queryConfig = {...queryConfig, interval_seconds: 180}; }
          if (data.revision !== settingsRevision) { json(response, 409, {error: 'settings_conflict'}); return; }
          queryConfig = data.config; settingsRevision++; revision++; json(response, 200, settings()); return;
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
      if (resource === 'settings') { json(response, 200, settings()); return; }
      if (resource === 'overview') {
        json(response, 200, {...identity(), version: '0.2.0-rc.4', accounts_revision: accountsRevision, query_settings: {revision: settingsRevision, config: queryConfig}, read_only: true, query_concurrency: 8,
          capabilities: {quota_query: true, passive_quota: true, model_execution: false, inventory_sync: true, query_settings: true, automatic_quota_query: true, randomized_quota_query: true}, activity: {connected: true},
          inventory: {status: 'ready', last_success_at: observed()}, dashboard: {total: 24, available: 20, disabled: 2, unavailable: 2, capacity: 16, unknown_quota: 4, errors: 2,
            tasks: {active: 3, querying: 2}, recent_jobs: fixtureJobs, active_jobs: fixtureJobs,
            recent_events: [{kind: 'quota_query', at: observed(), account_id: 'codex-07', phase: 'running'}, {kind: 'quota_query', at: observed(), account_id: '', phase: 'queued'}, {kind: 'inventory', at: observed(), phase: 'ready'}]}}); return;
      }
      if (resource === 'jobs') { const job = jobs.get(url.searchParams.get('job_id')); json(response, job ? 200 : 404, job ? {...job, ...identity()} : {error: 'not_found'}); return; }
      if (resource === 'accounts') {
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
  for (const page of ['accounts', 'query', 'wake', 'models', 'history', 'diagnostics', 'overview']) {
    await evaluate(`document.querySelector('[data-page="${page}"]').click()`);
    assert.equal(await evaluate(`document.querySelector('[data-view="${page}"]').hidden`), false, 'Navigation failed: ' + page);
  }
  assert.equal(posts.length, 0, 'Navigating dispatched a write');
  assert.equal(reads.some(path => path.endsWith('/settings')), false, 'Overview config should avoid redundant settings reads');
  await evaluate('document.getElementById("five-hour-query").click()');
  assert.equal(await evaluate('document.querySelector("[data-view=query]").hidden'), false);
  assert.equal(await evaluate('document.getElementById("settings-five-hour").checked'), true);
  assert.equal(await evaluate('document.getElementById("five-hour-query").checked'), false, 'Home switch must reflect saved state');
  await evaluate('document.getElementById("settings-interval").value="90";document.getElementById("settings-interval").dispatchEvent(new Event("input"))');
  const readsBeforeDraft = reads.filter(path => path.endsWith('/overview')).length;
  await waitFor(() => reads.filter(path => path.endsWith('/overview')).length > readsBeforeDraft, 'Settings did not receive actual polling');
  assert.equal(await evaluate('document.getElementById("settings-interval").value'), '90', 'Polling overwrote a dirty draft');
  await evaluate('document.querySelector("[data-page=overview]").click();document.querySelector("[data-page=query]").click()');
  assert.equal(await evaluate('document.getElementById("settings-interval").value'), '90', 'Navigation lost the draft');
  assert.equal(posts.length, 0, 'Editing dispatched an automatic write');
  await evaluate('for(const [id,value] of [["settings-interval","20"],["settings-delay-max","4"],["settings-timezone","Mars/Colony"],["settings-excluded","00:00-01:00, 00:30-02:00"]]){const field=document.getElementById(id);field.value=value;field.dispatchEvent(new Event("input"));}');
  assert.equal(await evaluate('document.getElementById("settings-save").disabled'), true);
  assert.equal(await evaluate('document.getElementById("settings-validation").hidden'), false);
  await evaluate('document.getElementById("query-settings-form").dispatchEvent(new Event("submit",{cancelable:true}))');
  assert.equal(posts.length, 0, 'Invalid settings were submitted');
  await evaluate('for(const [id,value] of [["settings-interval","60"],["settings-delay-min","0"],["settings-delay-max","60"],["settings-timezone","Asia/Shanghai"],["settings-excluded","23:00-07:00, 12:00-13:00"]]){const field=document.getElementById(id);field.value=value;field.dispatchEvent(new Event("input"));}');
  assert.match(await evaluate('document.getElementById("settings-diff").textContent'), /300 → 60/);
  await screenshot('query-settings-desktop.png');
  await evaluate('document.getElementById("settings-save").click()');
  await waitFor(() => evaluate('document.getElementById("settings-message").textContent.startsWith("已保存并应用")'), 'Settings save not acknowledged');
  const settingsPost = posts.find(post => post.resource === 'settings');
  assert.deepEqual(JSON.parse(settingsPost.body), {revision: 1, config: {...queryConfig}});
  assert.equal(await evaluate('document.getElementById("five-hour-query").checked'), true);
  queryConfig = {...queryConfig, automatic: true, interval_seconds: 120, on_weekly_expiry: true}; settingsRevision++; revision++;
  await waitFor(() => evaluate('document.getElementById("settings-interval").value==="120" && document.getElementById("settings-weekly").checked'), 'Clean editor did not follow backend settings polling');
  assert.equal(posts.filter(post => post.resource === 'settings').length, 1, 'Polling wrote settings');
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
  assert.equal(posts.filter(post => post.resource === 'settings').length, 2, 'Conflict was automatically retried');
  queryConfig = {...queryConfig, timezone: 'Factory'}; settingsRevision++; revision++;
  await waitFor(() => evaluate('document.getElementById("settings-timezone").value==="Factory"'), 'Server timezone alias broke overview polling');
  assert.equal(await evaluate('document.getElementById("error").hidden && document.getElementById("metric-total").textContent==="24"'), true, 'Unsupported browser timezone made the overview unusable');
  await evaluate('document.getElementById("settings-reload").onclick()');
  assert.equal(await evaluate('document.getElementById("settings-timezone").value'), 'Factory');
  assert.equal(await evaluate('document.getElementById("error").hidden'), true, 'Server timezone alias broke explicit settings reload');
  queryConfig = {...queryConfig, timezone: 'Asia/Shanghai'}; settingsRevision++; revision++;
  await waitFor(() => evaluate('document.getElementById("settings-timezone").value==="Asia/Shanghai" && document.getElementById("settings-validation").hidden'), 'Supported timezone did not recover the editor');
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
  await evaluate('document.getElementById("sync").click()');
  await waitFor(() => posts.some(post => post.resource === 'sync'), 'CPA sync not dispatched');
  assert.equal(posts.find(post => post.resource === 'sync').body, '');
  await evaluate('document.getElementById("next").click()');
  await waitFor(() => evaluate('document.getElementById("accounts").textContent.includes("codex-07")'), 'Next account page not displayed');
  await evaluate('document.getElementById("previous").click()');
  await waitFor(() => evaluate('document.getElementById("accounts").textContent.includes("codex-01")'), 'Previous account page not displayed');
  await evaluate('document.getElementById("accounts-search").value="codex-24";document.getElementById("accounts-search").dispatchEvent(new Event("input"))');
  await waitFor(() => evaluate('document.getElementById("accounts").textContent.includes("codex-24") && document.getElementById("accounts").rows.length===1'), 'Full-pool search not applied');
  assert.match(await evaluate('document.getElementById("accounts-page-info").textContent'), /1–1 \/ 1/);
  await evaluate('document.getElementById("accounts-search").value="";document.getElementById("accounts-search").dispatchEvent(new Event("input"))');
  await waitFor(() => evaluate('document.getElementById("accounts").rows.length===6'), 'Search did not reset');
  accounts[0].quota.five_hour.Remaining.Value = 11; revision++; accountsRevision++;
  await waitFor(() => evaluate('document.querySelector("progress").value===11'), 'Background refresh not applied');
  for (const width of [768, 390]) {
    await cdp('Emulation.setDeviceMetricsOverride', {width, height: 844, deviceScaleFactor: 1, mobile: false});
    const layout = await evaluate('({width:document.documentElement.clientWidth,scroll:document.documentElement.scrollWidth,wide:Array.from(document.querySelectorAll("body *")).filter(node=>!node.closest(".navigation,.table-scroll")).map(node=>({tag:node.tagName,id:node.id,classes:typeof node.className==="string"?node.className:"",right:Math.round(node.getBoundingClientRect().right)})).filter(node=>node.right>document.documentElement.clientWidth+1).slice(0,12)})');
    assert.ok(layout.scroll <= layout.width, 'Responsive page overflow at ' + width + ': ' + JSON.stringify(layout));
    assert.equal(await evaluate('getComputedStyle(document.getElementById("account-card")).display !== "none"'), true);
    await screenshot('responsive-' + width + '.png');
    await evaluate('document.querySelector("[data-page=query]").click()');
    assert.equal(await evaluate('document.documentElement.scrollWidth <= document.documentElement.clientWidth'), true, 'Settings page overflow at ' + width);
    await screenshot('query-settings-' + width + '.png');
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
  assert.equal(posts.every(post => ['settings', 'jobs', 'sync'].includes(post.resource)), true, 'Unexpected API write');
  console.log('V2 browser checks passed: desktop/mobile layout, settings validation/drafts/save/conflict/polling/timezone aliases, mixed batches, overdue exclusion holds, queued countdowns, immediate single/randomized bulk query contracts, sync, filtering, paging and authentication.');
  console.log('Local fixture screenshots: ' + output);
} finally {
  if (socket) socket.close();
  if (browser && browser.exitCode === null && browser.signalCode === null) {
    browser.kill('SIGTERM');
    await Promise.race([new Promise(resolve => browser.once('exit', resolve)), new Promise(resolve => setTimeout(resolve, 3000))]);
    if (browser.exitCode === null && browser.signalCode === null) browser.kill('SIGKILL');
  }
  if (server) await new Promise(resolve => server.close(resolve));
  await rm(temporary, {recursive: true, force: true, maxRetries: 5, retryDelay: 200});
}
