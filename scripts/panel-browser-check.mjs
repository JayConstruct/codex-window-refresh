// Optional real-browser smoke test. Uses only loopback fixtures, never CPA.
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { spawn } from 'node:child_process';
import { readFile, mkdtemp, rm, writeFile, access } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const base = '/v0/resource/plugins/codex-window-refresh/';
const api = '/v0/management/codex-window-refresh/';
const key = 'local-browser-test-only';
const temp = await mkdtemp(join(tmpdir(), 'window-refresh-panel-browser-'));
let browser, server, ws;
const posts = [];
const reads = [];
let snapshotSequence = 0, jobSequence = 0, quotaCompletedAt = 0;
const jobs = new Map();
function recordJob(kind, detail) { jobs.set(detail.job_id, {kind, detail}); summary.scheduler[kind] = detail; }
let previewAccountWakes = false;
let unauthorized = false, conflict = false, revision = 'a'.repeat(64);
let config = {
  schema_version: 1, enabled: false, timezone: 'Asia/Shanghai',
  quota_query: {automatic: true, manual: true, interval: '30s', on_five_hour_expiry: false, on_weekly_expiry: false},
  planning: {enabled: false, horizon_days: 7},
  reset_rules: { five_hour: { enabled: true, random_delay: { min: '0s', max: '600s' } }, weekly: { enabled: true, random_delay: { min: '0s', max: '600s' }, bypass_activity_limit: true, bypass_allowed_hours: true } },
  activity: { lookback: '1800s', accounts_per_user: 1, max_accounts: 10, identity_source: 'host_caller_scope' },
  ordinary_rules: { allowed_hours: ['08:00-23:00'], allow_outside_hours_when_users_active: true },
  daily_rules: [{ id: 'morning', enabled: true, random_time_range: '09:00-09:10', minimum_ready_accounts: 1, catch_up: true }],
  execution: { model: 'fixture-model', prompt: 'Reply OK.', max_concurrency: 1, min_dispatch_spacing: '5s', quota_read_timeout: '15s', request_timeout: '60s', quota_max_age_before_send: '30s', verify_delays: ['5s', '30s', '120s'], verification_capacity_hold: '900s', catch_up: true },
  account_overrides: {},
};
const now = new Date().toISOString(), reset = new Date(Date.now() + 3600000).toISOString();
const accounts = {
  'fixture-account': { ID: 'fixture-account', Enabled: true, Permission: 'allowed', FiveHour: { State: 'active', ResetAt: reset, ConfirmedAt: now, Source: 'cpa_response_headers', Remaining: { Value: 73, Unit: 'percent' } }, Weekly: { State: 'unknown', ResetAt: '0001-01-01T00:00:00Z' }, Task: null },
  '<img src=x onerror=alert(1)>': { ID: '<img src=x onerror=alert(1)>', Enabled: true, Permission: 'unknown', FiveHour: { State: 'unknown' }, Weekly: { State: 'unknown' }, Task: { State: 'waiting', HoldReason: 'quota_unknown', Intents: [{ Reason: 'weekly_reset', Status: 'pending', RandomizedDueAt: reset }] } },
};
const summary = { enabled: false, business_enabled: false, host_enabled: true, running: true, accounts: 2, reservations: 0, revision: 42, effective_concurrency: 1, last_error: '', scheduler: { observed_at: now, recent_users: 2, capacity: { target: 2, active: 1, reserved: 0, missing: 1, participants: 2, user_target: 2 }, hold_counts: { quota_unknown: 1 }, identity_source: 'host_caller_scope', quota_unknown_accounts: 1, foreground_busy_accounts: 0, next_inventory_at: reset } };
const capabilities = { StableAccountIdentity: true, ExactAccountExecution: true, QuotaAdapterReady: false, ActivityIdentityAtStart: true, ActivityIdentityAtCompletion: true, ForegroundGuard: false, quota_bridge: {status:'missing_management_key',management_key_env:'CPA_MANAGEMENT_KEY'} };
function json(res, status, value) { res.writeHead(status, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' }); res.end(JSON.stringify(value)); }
const errors = [];
async function waitFor(check, message, timeout = 12000) {
  const end = Date.now() + timeout;
  while (Date.now() < end) { if (await check()) return; await new Promise(r => setTimeout(r, 50)); }
  throw new Error(message);
}
try {
  assert.equal(typeof WebSocket, 'function', 'Browser smoke test requires Node.js 22 or newer');
  let executable = process.env.CHROME_BIN;
  if (!executable) {
    for (const path of ['/usr/bin/chromium', '/usr/bin/chromium-browser', '/usr/bin/google-chrome']) {
      try { await access(path); executable = path; break; } catch {}
    }
  }
  assert.ok(executable, 'Set CHROME_BIN to a Chromium executable');
  server = createServer(async (req, res) => {
    try {
      if (req.url === '/management.html') {
        res.writeHead(200, {'Content-Type': 'text/html'});
        res.end('<!doctype html><iframe src="' + base + 'status"></iframe>'); return;
      }
      if (req.url.startsWith(base)) {
        const name = req.url.slice(base.length), files = { status: 'panel.html', 'panel.js': 'panel.js', 'panel.css': 'panel.css' };
        if (!files[name]) { res.writeHead(404); res.end(); return; }
        const type = name === 'status' ? 'text/html' : name.endsWith('.js') ? 'text/javascript' : 'text/css';
        res.writeHead(200, { 'Content-Type': type, 'Content-Security-Policy': "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; base-uri 'none'; frame-ancestors 'self'; form-action 'none'", 'X-Content-Type-Options': 'nosniff' });
        res.end(await readFile(join(root, 'internal/plugin/web', files[name]))); return;
      }
      if (!req.url.startsWith(api)) { res.writeHead(404); res.end(); return; }
      if (req.method === 'GET') reads.push(req.url);
      if (unauthorized || req.headers.authorization !== `Bearer ${key}`) { json(res, 401, { error: 'unauthorized' }); return; }
      const parsed = new URL(req.url, 'http://127.0.0.1');
      const path = parsed.pathname.slice(api.length);
      if (req.method === 'POST') {
        let body = ''; for await (const chunk of req) body += chunk;
        posts.push({ path, body });
        if (path === 'prompt-generation') {
          const request = JSON.parse(body);
          assert.equal(request.account_id, 'fixture-account');
          recordJob('prompt_generation', {job_id:request.request_id, request, phase:'completed', candidates:['Hello.', 'Hi.', 'Ready.', 'Yes.', 'OK.']});
          json(res,202,{status:'queued',request_id:request.request_id,job_id:request.request_id}); return;
        }
        if (path === 'quota-connection-check') {
          assert.equal(body, '', 'Connection check received credential input');
          capabilities.quota_bridge.host_http_status = 'verified';
          json(res, 200, {status:'verified'}); return;
        }
        if (path === 'quota-query') {
          const jobID = 'browser-job-' + (++jobSequence);
          recordJob('quota_query', {job_id:jobID, account_id: body ? JSON.parse(body).account_id : '', phase:'queued'});
          if (body) {
            const selected = JSON.parse(body);
            assert.deepEqual(Object.keys(selected), ['account_id'], 'Account query received model input');
            assert.ok(Object.hasOwn(accounts, selected.account_id), 'Wrong account selected');
            const accountID = selected.account_id, failed = accountID !== 'fixture-account';
            // Publish acceptance immediately; execution completes asynchronously.
            setTimeout(() => {
              quotaCompletedAt = Date.now();
              recordJob('quota_query', {job_id:jobID,account_id:accountID,phase:'completed',total:1,completed:failed?0:1,failed:failed?1:0,skipped:0,started_at:new Date().toISOString(),finished_at:new Date().toISOString()});
              if (!failed) {
                accounts[accountID].FiveHour.Remaining.Value = 70;
                accounts[accountID].FiveHour.Source = 'cpa_wham_usage';
              }
            }, 150);
            json(res,202,{status:'queued',account_id:accountID,job_id:jobID});return;
          }
          setTimeout(() => {
            recordJob('quota_query', {job_id:jobID,phase:'completed',total:2,completed:2,failed:0,skipped:0,started_at:new Date().toISOString(),finished_at:new Date().toISOString()});
            accounts['fixture-account'].FiveHour.Remaining.Value = 71;
            accounts['fixture-account'].FiveHour.Source = 'cpa_wham_usage';
          }, 150);
          json(res,202,{status:'queued',job_id:jobID});return;
        }
        if (path === 'config') {
          const edit = JSON.parse(body);
          if (conflict || edit.revision !== revision) { json(res, 409, { error: 'policy changed; reload the editor before saving' }); return; }
          config = edit.config; revision = 'b'.repeat(64);
          json(res, 200, { config, revision }); return;
        }
        json(res, path === 'recheck' ? 202 : 200, { status: 'scheduled' }); return;
      }
      summary.scheduler.plan = {enabled: config.planning.enabled, horizon_days: config.planning.horizon_days, generated_at: now, until: new Date(Date.now()+config.planning.horizon_days*86400000).toISOString(), entries: config.planning.enabled && config.quota_query.on_five_hour_expiry ? [{kind:'query',trigger:'five_hour_reset',account_id:'fixture-account',earliest_at:reset,latest_at:reset,status:'estimated',detail:'Only quota, no model'}] : []};
      if (path === 'history') {
        json(res,200,{entries:[{id:1,kind:'wake',account_id:'fixture-account',started_at:now,status:'verified',prompt:'<script>text-only</script>',sources:[{trigger:'daily_schedule',rule_id:'morning',evidence_source:'cpa_wham_usage',evidence_at:now}]}],next_cursor:0,retention_days:30,maximum_entries:10000}); return;
      }
      summary.enabled = config.enabled;
      summary.scheduler.jobs = [...jobs.entries()].map(([job_id, {kind, detail}]) => ({job_id, kind, phase:detail.phase, account_id:detail.account_id || detail.request?.account_id || '', [kind]:detail}));
      if (previewAccountWakes) summary.scheduler.plan.next_wakes = {
        'fixture-account': {kind:'wake',account_id:'fixture-account',earliest_at:reset,latest_at:reset,status:'scheduled'},
        '<img src=x onerror=alert(1)>': {kind:'wake',account_id:'<img src=x onerror=alert(1)>',earliest_at:new Date(Date.parse(reset)+600000).toISOString(),latest_at:new Date(Date.parse(reset)+1200000).toISOString(),status:'estimated'}
      };
      const values = { snapshot: {instance_id:'browser-instance',snapshot_seq:++snapshotSequence,summary,accounts,config,capabilities}, summary, accounts, config, capabilities, 'config-editor': { config, revision, disk_matches_running: true } };
      json(res, 200, values[path] ?? {});
    } catch (err) { errors.push(String(err)); json(res, 500, { error: 'fixture error' }); }
  });
  await new Promise(r => server.listen(0, '127.0.0.1', r));
  const url = `http://127.0.0.1:${server.address().port}${base}status`;
  browser = spawn(executable, ['--headless', '--no-sandbox', '--disable-dev-shm-usage', '--disable-background-networking', '--disable-gpu', '--no-first-run', '--no-default-browser-check', '--remote-debugging-port=0', `--user-data-dir=${temp}`, 'about:blank'], { stdio: ['ignore', 'ignore', 'pipe'] });
  let browserLog = '';
  browser.stderr.on('data', chunk => { browserLog = (browserLog + chunk.toString()).slice(-8000); });
  browser.on('error', err => errors.push(String(err)));
  let port;
  try {
    await waitFor(async () => {
      if (browser.exitCode !== null || browser.signalCode !== null) throw new Error('Chromium exited before starting: ' + browserLog);
      try { port = (await readFile(join(temp, 'DevToolsActivePort'), 'utf8')).split('\n')[0]; return !!port; } catch { return false; }
    }, 'Chromium did not start', 30000);
  } catch (error) { console.error(browserLog); throw error; }
  let pageTarget;
  await waitFor(async () => {
    const targets = await (await fetch(`http://127.0.0.1:${port}/json/list`)).json();
    pageTarget = targets.find(target => target.type === 'page');
    return !!pageTarget;
  }, 'Chromium did not create a page target');
  ws = new WebSocket(pageTarget.webSocketDebuggerUrl);
  await new Promise((r, j) => { ws.addEventListener('open', r, { once: true }); ws.addEventListener('error', j, { once: true }); });
  let serial = 0; const pending = new Map(), caughtExceptions = [], dialogs = [];
  function cdp(method, params = {}) { return new Promise((r, j) => { const id = ++serial; const timer = setTimeout(() => { pending.delete(id); j(new Error(`CDP timeout: ${method}`)); }, 15000); pending.set(id, { r, j, timer }); ws.send(JSON.stringify({ id, method, params })); }); }
  ws.addEventListener('message', event => {
    const data = JSON.parse(event.data);
    if (data.id) { const p = pending.get(data.id); if (!p) return; clearTimeout(p.timer); pending.delete(data.id); data.error ? p.j(new Error(JSON.stringify(data.error))) : p.r(data.result); }
    if (data.method === 'Runtime.exceptionThrown') errors.push(data.params.exceptionDetails.text);
    if (data.method === 'Page.javascriptDialogOpening') { dialogs.push(data.params.message); void cdp('Page.handleJavaScriptDialog', { accept: true }); }
    if (data.method === 'Debugger.paused') { caughtExceptions.push(data.params.data?.description || data.params.reason); void cdp('Debugger.resume'); }
  });
  await cdp('Page.enable'); await cdp('Runtime.enable'); await cdp('Debugger.enable');
  await cdp('Debugger.setPauseOnExceptions', { state: 'all' });
  await cdp('Emulation.setDeviceMetricsOverride', {width:1280,height:960,deviceScaleFactor:1,mobile:false});
  async function evaluate(expression) {
    const result = await cdp('Runtime.evaluate', { expression, returnByValue: true, awaitPromise: true });
    assert.ok(!result.exceptionDetails, JSON.stringify(result.exceptionDetails)); return result.result.value;
  }
  let navigationSerial = 0;
  async function navigate(method, params = {}) {
    // CDP acknowledges a reload before the old document disappears. A ready
    // workspace in that document must not satisfy the next page's assertions.
    const marker = `smoke-navigation-${++navigationSerial}`;
    await evaluate(`window.__panelSmokeNavigation = ${JSON.stringify(marker)}`);
    const result = await cdp(method, params);
    assert.ok(!result.errorText, result.errorText);
    await waitFor(async () => {
      try {
        const state = await cdp('Runtime.evaluate', {expression: `window.__panelSmokeNavigation !== ${JSON.stringify(marker)} && document.readyState === "complete"`, returnByValue:true});
        return !state.exceptionDetails && state.result?.value === true;
      } catch (error) {
        if (/Execution context|Cannot find context|Inspected target navigated/i.test(error.message)) return false;
        throw error;
      }
    }, 'Navigation did not finish in a new document');
  }
  await navigate('Page.navigate', { url });
  await waitFor(() => evaluate('document.readyState === "complete" && !!document.getElementById("connect")'), 'Login did not render');
  await evaluate(`document.getElementById('management-key').value=${JSON.stringify(key)}; document.getElementById('login-form').requestSubmit()`);
  try { await waitFor(() => evaluate('!document.getElementById("workspace").hidden'), 'Login did not succeed'); }
  catch (error) { console.error(await evaluate('document.getElementById("notice").textContent'), errors, caughtExceptions); throw error; }
  assert.equal(posts.length, 0, 'Read-only login must not dispatch operations');
  assert.equal(await evaluate('document.getElementById("auto-refresh").checked'), false, 'Page refresh must require explicit opt-in');
  assert.equal(await evaluate('document.getElementById("query-manual")'), null, 'Manual queries must not have a permission switch');
  assert.equal(await evaluate('document.querySelector(".overview-guide").open || document.querySelector(".mode-help").open'), false, 'Overview help should start collapsed');
  assert.equal(await evaluate('document.getElementById("quota-query").getBoundingClientRect().top < innerHeight'), true, 'Primary quota action is below the desktop fold');
  assert.equal(await evaluate('document.getElementById("updated-at").textContent.includes("页面更新于") && document.getElementById("quota-freshness").textContent.includes("额度观测")'), true, 'Page reads were not separated from quota observations');
  await evaluate('document.getElementById("refresh-options").open=true');
  const initialReads = reads.length;
  await new Promise(resolve => setTimeout(resolve, 5500));
  assert.equal(reads.length, initialReads, 'Snapshots refreshed without explicit opt-in');
  await evaluate('document.getElementById("auto-refresh").click()');
  assert.equal(await evaluate('localStorage.length'), 1, 'Only panel preferences should be persisted');
  assert.deepEqual(await evaluate('JSON.parse(localStorage.getItem("codex-window-refresh:panel-preferences:v1:/v0/management/codex-window-refresh"))'), {version:1,autoRefresh:true,backgroundRefresh:false}, 'Manual key leaked into preferences');
  assert.equal(await evaluate('document.getElementById("mode-title").textContent'), '仅查看与查询额度', 'Applied mode did not explain model behavior');
  assert.equal(await evaluate('document.getElementById("query-automatic").closest(".view").id'), 'tab-query', 'Query controls are mixed with wake rules');
  assert.equal(await evaluate('document.getElementById("save-bar").hidden'), true, 'Clean draft displayed a pending save bar');
  assert.equal(await evaluate('document.getElementById("quota-query").disabled'),true,'Unready quota bridge allowed a query');
  assert.equal(await evaluate('document.getElementById("bridge-status").textContent.includes("CPA 进程未读取到") && document.getElementById("bridge-help").textContent.includes("CPA_MANAGEMENT_KEY")'),true,'Missing backend key diagnosis was not actionable');
  capabilities.QuotaAdapterReady = true; capabilities.quota_bridge.status = 'configured';
  await waitFor(()=>evaluate('!document.getElementById("quota-query").disabled && document.getElementById("bridge-status").textContent.includes("已就绪")'),'Startup readiness remained stale after automatic polling');
  capabilities.quota_bridge = {mode:'host',status:'host_callback'};
  await evaluate('document.getElementById("refresh").click()');
  await waitFor(()=>evaluate('document.getElementById("bridge-help").textContent.includes("不需要配置后台管理地址或管理密钥环境变量") && !document.getElementById("quota-query").disabled'),'Host query diagnosis still required backend management credentials');
  // Seed only the mock host's saved credentials; exercise the real startup path.
  for (const legacy of [false, true]) {
    for (const encoded of [false, true]) {
      const saved = await evaluate(`(() => {
        localStorage.clear();
        const name = ${JSON.stringify(legacy ? 'managementKey' : 'cli-proxy-auth')};
        const value = ${legacy ? JSON.stringify(key) : `{state: {managementKey: ${JSON.stringify(key)}, apiBase: location.origin}, version: 0}`};
        let raw = JSON.stringify(value);
        if (${encoded}) {
          const bytes = new TextEncoder().encode(raw);
          const mask = new TextEncoder().encode('cli-proxy-api-webui::secure-storage|' + location.host + '|' + navigator.userAgent);
          raw = 'enc::v1::' + btoa(Array.from(bytes, (byte, i) => String.fromCharCode(byte ^ mask[i % mask.length])).join(''));
        }
        localStorage.setItem(name, raw); return raw;
      })()`);
      const beforeReload = reads.length;
      await navigate('Page.reload');
      await waitFor(async () => reads.length >= beforeReload + 2 && await evaluate('document.readyState === "complete" && !!document.getElementById("workspace") && !document.getElementById("workspace").hidden'), 'Saved CPA credentials did not auto-connect');
      assert.equal(await evaluate(`localStorage.getItem(${JSON.stringify(legacy ? 'managementKey' : 'cli-proxy-auth')})`), saved, 'Auto-connect modified host storage');
      assert.equal(await evaluate('document.getElementById("management-key").value'), '', 'Auto-connect exposed key in form');
      await evaluate('document.getElementById("disconnect").click()');
      assert.equal(await evaluate('document.getElementById("workspace").hidden'), true, 'Disconnect did not stay disconnected');
    }
  }
  await navigate('Page.navigate', {url: new URL('/management.html', url).href});
  try { await waitFor(() => evaluate('!!document.querySelector("iframe")?.contentDocument?.getElementById("workspace") && !document.querySelector("iframe").contentDocument.getElementById("workspace").hidden'), 'CPA menu iframe did not auto-connect'); }
  catch (error) {
    console.error('Iframe diagnostic:', await evaluate('JSON.stringify({url:location.pathname,frame:document.querySelector("iframe")?.contentDocument?.readyState,notice:document.querySelector("iframe")?.contentDocument?.getElementById("notice")?.textContent})'), errors, caughtExceptions);
    throw error;
  }
  await navigate('Page.navigate', {url});
  await waitFor(() => evaluate('document.readyState === "complete" && !!document.getElementById("workspace") && !document.getElementById("workspace").hidden'), 'Reopening did not auto-connect');
  assert.equal(posts.length, 0, 'Auto-connect dispatched operations');
  assert.equal(await evaluate('document.getElementById("auto-refresh").checked'), false, 'Reopening enabled page refresh without opt-in');
  assert.equal(await evaluate('document.getElementById("background-refresh").disabled && !document.getElementById("background-refresh").checked'), true, 'Background refresh was not explicit');
  await evaluate('document.getElementById("auto-refresh").click()');
  await evaluate('document.getElementById("background-refresh").click()');
  await waitFor(()=>evaluate('!document.getElementById("refresh").disabled && document.getElementById("refresh-status").textContent.includes("含后台标签页")'),'Background refresh opt-in did not apply');
  const hostLogin = await evaluate('localStorage.getItem("managementKey")');
  await navigate('Page.reload');
  await waitFor(()=>evaluate('document.readyState === "complete" && !document.getElementById("workspace").hidden && document.getElementById("auto-refresh").checked && document.getElementById("background-refresh").checked'),'Reload did not restore saved refresh preferences');
  assert.equal(await evaluate('localStorage.getItem("managementKey")'), hostLogin, 'Preference restoration changed host login');
  await evaluate(`document.querySelector('[data-tab="accounts"]').click()`);
  await waitFor(() => evaluate('document.getElementById("account-list").textContent.includes("fixture-account")'), 'Accounts did not render');
  assert.equal(await evaluate('document.getElementById("account-list").querySelectorAll("img").length'), 0, 'Account IDs must render as text');
  assert.equal(await evaluate('document.getElementById("account-list").textContent.includes("正常请求响应")'), true, 'Quota source was not displayed');
  const beforeDisplayChange = posts.length;
  await evaluate('document.getElementById("account-sort").value="five-remaining";document.getElementById("account-sort").dispatchEvent(new Event("change"));document.getElementById("account-layout").value="compact";document.getElementById("account-layout").dispatchEvent(new Event("change"))');
  assert.equal(await evaluate('document.querySelector(".account-card").dataset.accountId'), 'fixture-account', 'Unknown quota sorted ahead of known percentage');
  assert.equal(await evaluate('document.getElementById("account-list").classList.contains("compact")'), true, 'Compact layout did not apply');
  assert.equal(posts.length, beforeDisplayChange, 'Changing account display wrote server rules');
  await navigate('Page.reload');
  await waitFor(()=>evaluate('document.readyState === "complete" && !document.getElementById("workspace").hidden && document.getElementById("account-layout").value==="compact" && document.getElementById("account-sort").value==="five-remaining"'),'Account display preferences did not survive reload');
  await evaluate('document.querySelector("[data-tab=accounts]").click()');
  assert.equal(await evaluate('Array.from(document.querySelectorAll(".account-disclosure")).every(item=>!item.open)'),true,'Account diagnostics were not collapsed');
  await evaluate('document.querySelector(".account-disclosure").open=true');
  await waitFor(()=>evaluate('document.querySelector(".account-disclosure").open'),'Account details did not open');
  await evaluate('window.retainedAccountCard=document.querySelector(".account-card");window.retainedAccountButton=window.retainedAccountCard.querySelector("[data-quota-account]");window.retainedAccountButton.focus()');
  await waitFor(() => evaluate('!document.getElementById("tab-accounts").hidden && document.activeElement===window.retainedAccountButton'), 'Account focus was not established before polling');
  const accountInteractionState = () => evaluate('({active:document.activeElement?.outerHTML?.slice(0,400),retained:window.retainedAccountCard?.dataset.accountId,retainedConnected:window.retainedAccountCard?.isConnected,retainedOpen:window.retainedAccountCard?.querySelector(".account-disclosure")?.open,cards:Array.from(document.querySelectorAll(".account-card")).map(card=>({id:card.dataset.accountId,open:card.querySelector(".account-disclosure").open,retained:card===window.retainedAccountCard,focused:card.contains(document.activeElement)}))})');
  console.log('Account interaction before polling:', JSON.stringify(await accountInteractionState()));
  const otherAccount = '<img src=x onerror=alert(1)>', originalOtherWindow = structuredClone(accounts[otherAccount].FiveHour);
  accounts[otherAccount].FiveHour = {...originalOtherWindow, Remaining: {Value: 1, Unit: 'percent'}};
  accounts['fixture-account'].FiveHour.Remaining.Value = 72;
  const pollStarted = Date.now();
  await waitFor(() => evaluate('document.getElementById("account-list").textContent.includes("72%")'), 'Passive quota did not appear on automatic panel polling');
  assert.ok(Date.now() - pollStarted < 8000, 'Panel polling did not use the faster snapshot interval');
  assert.equal(await evaluate('document.querySelector(".account-disclosure").open'),true,'Polling collapsed open account diagnostics: '+JSON.stringify(await accountInteractionState()));
  assert.equal(await evaluate('document.activeElement===window.retainedAccountButton && document.querySelector(".account-card")===window.retainedAccountCard'),true,'Passive refresh replaced the focused account or moved its sort position');
  await evaluate('window.retainedAccountCard.querySelector(".account-disclosure").open=false;document.getElementById("account-layout").dispatchEvent(new Event("change"))');
  assert.equal(await evaluate('!window.retainedAccountCard.querySelector(".account-disclosure").open'),true,'Synchronous refresh reopened user-collapsed diagnostics');
  await evaluate('window.retainedAccountCard.querySelector(".account-disclosure").open=true;document.getElementById("account-layout").dispatchEvent(new Event("change"));window.retainedAccountButton.focus()');
  assert.equal(await evaluate('window.retainedAccountCard.querySelector(".account-disclosure").open'),true,'Synchronous refresh collapsed user-expanded diagnostics');
  await evaluate('window.retainedAccountText=window.retainedAccountCard.querySelector(".account-id").firstChild;const range=document.createRange();range.selectNodeContents(window.retainedAccountText);getSelection().removeAllRanges();getSelection().addRange(range)');
  accounts['fixture-account'].FiveHour.Remaining.Value = 71;
  await evaluate('document.getElementById("refresh").click()');
  await waitFor(()=>evaluate('document.getElementById("account-list").textContent.includes("71%")'),'Selected account snapshot did not update');
  assert.equal(await evaluate('getSelection().toString()'), 'fixture-account', 'Passive updates cleared selected account text');
  const selectedObservation = await evaluate('const observed=window.retainedAccountCard.querySelector("[data-observed]");const selectionRange=document.createRange();selectionRange.selectNodeContents(observed);getSelection().removeAllRanges();getSelection().addRange(selectionRange);getSelection().toString()');
  await new Promise(resolve => setTimeout(resolve, 1200));
  assert.equal(await evaluate('getSelection().toString()'), selectedObservation, 'Local clock updates cleared selected observation time');
  await evaluate('getSelection().removeAllRanges();document.getElementById("account-sort").dispatchEvent(new Event("change"))');
  assert.equal(await evaluate('document.querySelector(".account-card").dataset.accountId'), otherAccount, 'An explicit sort failed to update order');
  accounts[otherAccount].FiveHour = originalOtherWindow;
  await evaluate('document.getElementById("account-sort").focus();document.getElementById("refresh").click()');
  await waitFor(()=>evaluate('!document.getElementById("refresh").disabled && document.querySelector(".account-card").dataset.accountId==="fixture-account"'),'Restoring unknown quota did not restore sort order');
  await evaluate('window.retainedAccountCard.querySelector(".account-footer button").click()');
  assert.equal(await evaluate('JSON.parse(document.getElementById("config-json").value).account_overrides["fixture-account"].paused'), true, 'Pause did not update the current account draft');
  await evaluate('window.retainedAccountCard.querySelector(".account-footer button").click()');
  assert.equal(await evaluate('JSON.parse(document.getElementById("config-json").value).account_overrides["fixture-account"].paused'), false, 'Reused pause listener retained stale account state');
  await evaluate('document.getElementById("draft-restore").click()');
  await waitFor(()=>evaluate('document.getElementById("save-bar").hidden'),'Discarding pause draft did not restore clean state');
  assert.equal(posts.length, 0, 'Snapshot polling dispatched operations');
  await evaluate('document.querySelector("[data-tab=capabilities]").click(); document.getElementById("quota-connection-check").click()');
  await waitFor(()=>evaluate('document.getElementById("bridge-status").textContent.includes("创建与取消已验证") && !document.getElementById("quota-connection-check").disabled'),'Explicit host connection check failed');
  assert.equal(posts.length, 1, 'Connection check dispatched unexpected operations');
  assert.equal(posts[0].path, 'quota-connection-check', 'Connection check used quota/model endpoint');
  await evaluate('document.querySelector("[data-tab=accounts]").click()');
  assert.equal(await evaluate('document.querySelectorAll("[data-quota-account]").length'),2,'Account query buttons missing');
  assert.equal(await evaluate('Array.from(document.querySelectorAll("[data-quota-account]")).every(button=>!button.closest("details"))'),true,'Account query was hidden in diagnostics');
  await evaluate('document.getElementById("auto-refresh").click()');
  assert.equal(await evaluate('document.getElementById("auto-refresh").checked'),false,'Manual query regression requires page polling off');
  const singleQueryCount = posts.length;
  await evaluate(`{ const button=Array.from(document.querySelectorAll('[data-quota-account]')).find(item=>item.dataset.quotaAccount==='fixture-account'); button.click(); button.click(); }`);
  await waitFor(()=>posts.length===singleQueryCount+1,'Account query was not sent');
  await waitFor(()=>evaluate('Array.from(document.querySelectorAll("[data-quota-account]")).every(button=>button.disabled)'),'Queued query allowed another target before snapshot');
  await waitFor(()=>evaluate('document.getElementById("account-list").textContent.includes("70%") && document.getElementById("account-list").textContent.includes("查询完成")'),'Single-account result did not update');
  const singleQueryLatency = Date.now() - quotaCompletedAt;
  assert.ok(quotaCompletedAt > 0 && singleQueryLatency < 2000, 'Single-account result took more than two seconds to appear after backend completion');
  assert.deepEqual(JSON.parse(posts.at(-1).body),{account_id:'fixture-account'},'Account query widened to all accounts');
  assert.equal(posts.length,singleQueryCount+1,'Double click duplicated the account query');
  assert.equal(accounts['<img src=x onerror=alert(1)>'].FiveHour.State,'unknown','Selected query changed another account');
  await evaluate(`Array.from(document.querySelectorAll('[data-quota-account]')).find(item=>item.dataset.quotaAccount!=='fixture-account').click()`);
  await waitFor(()=>evaluate('document.getElementById("account-list").textContent.includes("查询失败，可手动重试")'),'Account query failure was not displayed');
  summary.scheduler.quota_query.last_error = 'quota_credential_expired';
  await evaluate('document.getElementById("refresh").click()');
  await waitFor(()=>evaluate('document.getElementById("account-list").textContent.includes("账号凭据已过期")'),'Expired upstream credentials lacked an actionable diagnosis');
  assert.equal(JSON.parse(posts.at(-1).body).account_id,'<img src=x onerror=alert(1)>','Account ID was not passed literally in JSON');
  summary.scheduler.quota_query.last_error = 'quota_management_auth_failed';
  summary.scheduler.quota_query.diagnostic = {code:'quota_management_auth_failed',stage:'management_http',http_status:403,elapsed_ms:120};
  await evaluate('document.getElementById("refresh").click()');
  await waitFor(()=>evaluate('document.getElementById("account-list").textContent.includes("后台管理密钥认证失败") && document.getElementById("account-list").textContent.includes("管理接口 · HTTP 403 · 120 毫秒")'),'Structured quota failure did not reach account card');
  assert.equal(await evaluate('document.getElementById("account-list").querySelectorAll("img").length'),0,'Account query introduced unsafe rendering');
  await evaluate('document.querySelector(".account-disclosure").open=false; document.querySelector("[data-tab=overview]").click()');
  await waitFor(() => evaluate('!document.getElementById("quota-query").disabled'), 'Manual query was not enabled');
  await evaluate('document.getElementById("quota-query").click()');
  await waitFor(() => evaluate('document.getElementById("quota-query-status").textContent.includes("全部账号 · 查询结束") && document.getElementById("account-list").textContent.includes("71%")'), 'Current batch query result did not display');
  assert.equal(posts.filter(p=>p.path==='quota-query').length,3,'Manual query duplicated');
  assert.equal(await evaluate('document.getElementById("account-list").textContent.includes("71%") && document.getElementById("account-list").textContent.includes("主动查询")'),true,'Manual quota did not update panel');
  assert.equal(await evaluate('document.getElementById("auto-refresh").checked'),false,'Query tracking enabled continuous page polling');
  const completedReads = reads.length;
  await new Promise(resolve => setTimeout(resolve, 5500));
  assert.equal(reads.length,completedReads,'Query tracking kept polling after completion');
  await evaluate('document.getElementById("auto-refresh").click()');
  await cdp('Emulation.setDeviceMetricsOverride', { width: 1280, height: 960, deviceScaleFactor: 1, mobile: false });
  let capture = await cdp('Page.captureScreenshot');
  await writeFile(join(temp, 'panel-overview-desktop.png'), Buffer.from(capture.data, 'base64'));
  await evaluate('document.querySelector("[data-open-tab=query]").click()');
  assert.equal(await evaluate('!document.getElementById("tab-query").hidden'),true,'Query shortcut failed');
  await evaluate('document.getElementById("nav-query").dispatchEvent(new KeyboardEvent("keydown",{key:"ArrowRight",bubbles:true}))');
  assert.equal(await evaluate('document.activeElement.id'), 'nav-rules', 'Keyboard tab navigation failed');
  await evaluate('document.querySelector("[data-tab=accounts]").click()');
  capture = await cdp('Page.captureScreenshot');
  await writeFile(join(temp, 'panel-desktop.png'), Buffer.from(capture.data, 'base64'));
  const beforeInvalidInput = posts.length;
  await evaluate('document.querySelector("[data-tab=rules]").click();const ratioField=document.getElementById("rule-ratio");ratioField.value="";ratioField.dispatchEvent(new Event("input",{bubbles:true}));ratioField.dispatchEvent(new Event("change",{bubbles:true}));document.querySelector("[data-tab=overview]").click();document.getElementById("config-save").click()');
  assert.equal(await evaluate('document.activeElement.id'), 'rule-ratio', 'Save did not locate the invalid number across tabs');
  assert.equal(await evaluate('document.getElementById("rule-ratio").value'), '', 'Invalid number was overwritten');
  assert.equal(await evaluate('document.getElementById("rule-ratio").getAttribute("aria-invalid")'), 'true', 'Invalid number was not marked');
  capture = await cdp('Page.captureScreenshot');
  await writeFile(join(temp, 'panel-field-error-desktop.png'), Buffer.from(capture.data, 'base64'));
  assert.equal(posts.length,beforeInvalidInput,'Invalid numeric input submitted a write');
  await evaluate('document.getElementById("rule-ratio").value="1";document.getElementById("rule-ratio").dispatchEvent(new Event("change",{bubbles:true}));document.getElementById("rule-five-min").value="10m";document.getElementById("rule-five-min").dispatchEvent(new Event("change",{bubbles:true}));document.getElementById("rule-five-max").value="1m";document.getElementById("rule-five-max").dispatchEvent(new Event("change",{bubbles:true}));document.getElementById("config-save").click()');
  assert.equal(await evaluate('document.activeElement.id'), 'rule-five-max', 'Inverted delay range did not focus its maximum field');
  assert.equal(posts.length,beforeInvalidInput,'Invalid delay range submitted a write');
  await evaluate('document.getElementById("rule-five-min").value="0s";document.getElementById("rule-five-min").dispatchEvent(new Event("change",{bubbles:true}));document.getElementById("rule-five-max").value="600s";document.getElementById("rule-five-max").dispatchEvent(new Event("change",{bubbles:true}));document.getElementById("rule-timezone").value="Factory";document.getElementById("rule-timezone").dispatchEvent(new Event("change",{bubbles:true}))');
  assert.equal(await evaluate('document.getElementById("rule-timezone").getAttribute("aria-invalid")'), 'false', 'Browser tzdata rejected a server-valid alias');
  await evaluate('document.getElementById("rule-timezone").value="Asia/Shanghai";document.getElementById("rule-timezone").dispatchEvent(new Event("change",{bubbles:true}))');
  await waitFor(()=>evaluate('document.getElementById("save-bar").hidden'),'Correcting invalid input did not return to a clean draft');
  await evaluate(`{ document.querySelector('[data-tab="rules"]').click(); const ratio=document.getElementById('rule-ratio'); ratio.value='1.5'; ratio.dispatchEvent(new Event('input',{bubbles:true})); ratio.dispatchEvent(new Event('change',{bubbles:true})); }`);
  await evaluate(`{ document.querySelector('[data-tab="query"]').click(); const timeout=document.getElementById('query-timeout'); timeout.value='60s'; timeout.dispatchEvent(new Event('change',{bubbles:true})); }`);
  await evaluate(`{for(const [id,value] of [['rule-five-model','five-fixture-model'],['rule-five-prompt','Five hour content'],['rule-week-model','weekly-fixture-model'],['rule-week-prompt','Weekly content']]){const field=document.getElementById(id);field.value=value;field.dispatchEvent(new Event('change',{bubbles:true}));}document.getElementById('query-five-expiry').checked=true;document.getElementById('query-five-expiry').dispatchEvent(new Event('change',{bubbles:true}));document.getElementById('plan-enabled').checked=true;document.getElementById('plan-enabled').dispatchEvent(new Event('change',{bubbles:true}));document.getElementById('plan-days').value='3';document.getElementById('plan-days').dispatchEvent(new Event('change',{bubbles:true}));}`);
  await evaluate(`{for(const [id,value] of [['query-five-expiry-hours','22:00-08:00，12:00-13:00'],['query-five-expiry-min','1m'],['query-five-expiry-max','5m'],['query-week-periodic-hours','00:00-06:00'],['query-week-periodic-max','2m']]){const field=document.getElementById(id);field.value=value;field.dispatchEvent(new Event('change',{bubbles:true}));}document.getElementById('query-week-periodic').checked=false;document.getElementById('query-week-periodic').dispatchEvent(new Event('change',{bubbles:true}));}`);
  assert.equal(await evaluate('document.getElementById("query-five-expiry").closest(".window-settings") !== document.getElementById("query-week-expiry").closest(".window-settings")'), true, 'Window query settings are not separated');
  assert.equal(await evaluate('document.getElementById("rule-five-model").closest(".window-settings") !== document.getElementById("rule-week-model").closest(".window-settings")'), true, 'Window wake settings are not separated');
  await evaluate('document.getElementById("changes-preview").open=true;document.getElementById("save-bar").scrollIntoView({block:"end"})');
  assert.equal(await evaluate('document.getElementById("config-changes").textContent.includes("每用户账号数") && document.getElementById("config-changes").textContent.includes("60s")'), true, 'Save review omitted edits from another tab');
  assert.equal(await evaluate('document.querySelectorAll("#config-changes script, #config-changes img").length'), 0, 'Review interpreted data as markup');
  assert.ok(await evaluate('document.documentElement.scrollWidth <= innerWidth + 1'), 'Save review overflowed on desktop');
  capture = await cdp('Page.captureScreenshot');
  await writeFile(join(temp, 'panel-save-review-desktop.png'), Buffer.from(capture.data, 'base64'));
  await cdp('Emulation.setDeviceMetricsOverride', {width:390,height:844,deviceScaleFactor:1,mobile:true});
  assert.ok(await evaluate('document.documentElement.scrollWidth <= innerWidth + 1'), 'Save review overflowed on mobile');
  capture = await cdp('Page.captureScreenshot');
  await writeFile(join(temp, 'panel-save-review-mobile.png'), Buffer.from(capture.data, 'base64'));
  await cdp('Emulation.setDeviceMetricsOverride', {width:1280,height:960,deviceScaleFactor:1,mobile:false});
  await evaluate(`document.getElementById('config-save').click()`);
  await waitFor(() => config.activity.accounts_per_user === 1.5, 'Rule save failed');
  await waitFor(() => evaluate('!document.getElementById("rule-ratio").disabled'), 'Save did not finish updating the view');
  assert.ok(dialogs.some(message => message.includes('每用户账号数：1 → 1.5') && message.includes('额度查询超时：15s → 60s')), 'Save confirmation did not include specific changes');
  assert.equal(config.execution.verify_delays[2], '120s', 'Unedited advanced settings were lost');
  assert.equal(config.execution.quota_read_timeout, '60s', 'Quota timeout setting was not saved');
  assert.equal(config.daily_rules[0].id, 'morning', 'Daily rules were lost');
  assert.equal(config.quota_query.on_five_hour_expiry,true,'Expiry query switch did not persist independently');
  assert.equal(config.quota_query.on_weekly_expiry,false,'Five-hour switch changed weekly query setting');
  assert.deepEqual(config.quota_query.five_hour.expiry.excluded_hours,['22:00-08:00','12:00-13:00'],'Excluded hours did not serialize as a list');
  assert.deepEqual(config.quota_query.five_hour.expiry.random_delay,{min:'1m',max:'5m'},'Expiry delay did not persist');
  assert.equal(config.quota_query.weekly.periodic_enabled,false,'Weekly periodic participation did not persist');
  assert.equal(config.quota_query.weekly.periodic.random_delay.max,'2m','Independent periodic delay did not persist');
  assert.equal(config.reset_rules.five_hour.model,'five-fixture-model','Five-hour model did not save');
  assert.equal(config.reset_rules.weekly.prompt,'Weekly content','Weekly message did not save');
  assert.deepEqual(config.planning,{enabled:true,horizon_days:3},'Plan settings did not save');
  await evaluate('document.querySelector("[data-tab=plan]").click()');
  await waitFor(()=>evaluate('document.getElementById("plan-list").textContent.includes("fixture-account") && document.getElementById("plan-status").textContent.includes("未来 3 天")'),'Saved future plan did not display');
  assert.equal(await evaluate('document.querySelectorAll("#plan-list tr").length === 2 && document.querySelectorAll("#plan-list tr:first-child > *").length === 2 && document.querySelector("#plan-list .plan-query") === null'),true,'Account schedule did not render compact rows');
  previewAccountWakes = true; config.enabled = true;
  await evaluate('document.getElementById("refresh").click()');
  await waitFor(()=>evaluate('document.getElementById("plan-list").textContent.includes("预计")'), 'Account wake times did not render');
  assert.equal(await evaluate('document.querySelector("#plan-list tr th").textContent'), 'fixture-account', 'Account wake rows were not sorted by time');
  assert.ok(await evaluate('Array.from(document.querySelectorAll("#plan-list tr")).every(row => row.getBoundingClientRect().height <= 52)'), 'Account schedule rows are too tall on desktop');
  capture = await cdp('Page.captureScreenshot');
  await writeFile(join(temp, 'panel-next-wake-desktop.png'), Buffer.from(capture.data, 'base64'));
  await cdp('Emulation.setDeviceMetricsOverride', {width:390,height:844,deviceScaleFactor:1,mobile:true});
  assert.ok(await evaluate('document.documentElement.scrollWidth <= innerWidth + 1'), 'Account schedule overflowed on mobile');
  capture = await cdp('Page.captureScreenshot');
  await writeFile(join(temp, 'panel-next-wake-mobile.png'), Buffer.from(capture.data, 'base64'));
  await cdp('Emulation.setDeviceMetricsOverride', {width:1280,height:960,deviceScaleFactor:1,mobile:false});
  previewAccountWakes = false; config.enabled = false;
  await evaluate('document.getElementById("refresh").click()');
  await waitFor(()=>evaluate('document.getElementById("plan-list").textContent.includes("自动唤醒未开启")'), 'Account wake rows did not reflect disabled rules');
  assert.ok(posts.every(post=>['quota-query','quota-connection-check','config'].includes(post.path)),'Planning dispatched unexpected operations');
  await evaluate('document.getElementById("plan-view-history").click()');
  await waitFor(()=>evaluate('document.getElementById("history-list").textContent.includes("每日规则") && document.getElementById("history-list").textContent.includes("<script>text-only</script>")'),'Task history or sources did not render');
  assert.equal(await evaluate('document.querySelectorAll("#history-list script").length'),0,'History prompt was interpreted as markup');
  const historyWrites = posts.length;
  await evaluate('document.getElementById("history-today").click()');
  await waitFor(()=>reads.some(path=>path.includes('/history?') && path.includes('from=')),'Today shortcut did not apply a lower time bound');
  await evaluate('document.getElementById("history-failed").click()');
  await waitFor(()=>reads.some(path=>path.includes('/history?') && path.includes('status=failed')),'Failure shortcut did not filter result');
  await evaluate('document.querySelector("[data-tab=accounts]").click();Array.from(document.querySelectorAll("[data-history-account]")).find(button=>button.dataset.historyAccount==="fixture-account").click()');
  await waitFor(()=>evaluate('!document.getElementById("tab-plan").hidden && !document.getElementById("plan-history").hidden && document.getElementById("history-account").value==="fixture-account"'),'Account history link did not open the correct history view');
  await waitFor(()=>reads.some(path=>path.includes('/history?') && path.includes('account_id=fixture-account')),'Account history link did not scope the read');
  assert.equal(posts.length,historyWrites,'History controls dispatched a write');
  assert.equal(await evaluate('document.getElementById("generation-start").disabled'),false,'Saving settings did not restore generation controls');
  await evaluate(`{document.querySelector('[data-tab=rules]').click();document.getElementById('generation-account').value='fixture-account';document.getElementById('generation-model').value='generator-fixture';document.getElementById('generation-count').value='5';document.getElementById('generation-start').click();}`);
  await waitFor(()=>evaluate('document.getElementById("generation-preview").value.includes("Hello.")'),'Explicit content generation did not show candidate preview');
  const generationWrites = posts.filter(post=>post.path==='prompt-generation');
  assert.equal(generationWrites.length,1,'Generation repeated a model request');
  const beforeCandidateApply = posts.length;
  await evaluate('document.getElementById("rule-ratio").value="";document.getElementById("rule-ratio").dispatchEvent(new Event("input",{bubbles:true}));document.getElementById("rule-ratio").dispatchEvent(new Event("change",{bubbles:true}))');
  await evaluate('document.getElementById("generation-apply").click()');
  assert.deepEqual(await evaluate('JSON.parse(document.getElementById("config-json").value).execution.prompt_pool'),['Hello.','Hi.','Ready.','Yes.','OK.'],'Candidates were not added to draft');
  assert.equal(posts.length,beforeCandidateApply,'Applying candidates automatically saved or generated');
  assert.equal(await evaluate('document.getElementById("rule-ratio").value'), '', 'Applying generated candidates discarded another invalid input');
  conflict = true;
  await evaluate(`{ const ratio=document.getElementById('rule-ratio'); ratio.value='2'; ratio.dispatchEvent(new Event('change',{bubbles:true})); document.getElementById('config-save').click(); }`);
  await waitFor(() => posts.filter(p => p.path === 'config').length === 2, 'Conflict save was not sent');
  await waitFor(() => evaluate('document.getElementById("notice").textContent.includes("其他操作修改") || document.getElementById("notice").textContent.includes("冲突")'), 'Conflict did not display');
  assert.equal(await evaluate('document.getElementById("rule-ratio").value'), '2', 'Conflict discarded draft');
  await evaluate('document.querySelector("[data-tab=query]").click()');
  assert.equal(await evaluate('document.getElementById("save-bar").hidden'),false,'Cross-tab edits lost the shared save bar');
  assert.equal(await evaluate('document.getElementById("rule-ratio").value'),'2','Navigation discarded wake draft');
  conflict = false;
  await evaluate(`{document.getElementById('query-automatic').checked=false; document.getElementById('query-automatic').dispatchEvent(new Event('change',{bubbles:true})); document.getElementById('config-save').click();}`);
  await waitFor(()=>config.quota_query.automatic===false,'Automatic query switch did not persist');
  await waitFor(()=>evaluate('document.getElementById("save-bar").hidden'),'Query settings save did not finish');
  config.quota_query.manual = false;
  await evaluate('document.getElementById("refresh").click()');
  await waitFor(()=>evaluate('!document.getElementById("refresh").disabled'),'Legacy config refresh did not finish');
  await waitFor(()=>evaluate('!document.getElementById("quota-query").disabled && document.getElementById("quota-query-status").textContent.includes("定期查询：关闭")'),'Disabling automatic queries blocked manual queries');
  assert.equal(await evaluate('Array.from(document.querySelectorAll("[data-quota-account]")).every(button=>!button.disabled)'),true,'Account queries were blocked with automatic queries disabled');
  await evaluate('document.getElementById("auto-refresh").click()');
  assert.equal(await evaluate('document.getElementById("background-refresh").disabled && !document.getElementById("background-refresh").checked'),true,'Disabling polling retained background polling');
  await new Promise(resolve => setTimeout(resolve, 100));
  const stoppedReads = reads.length;
  await new Promise(resolve => setTimeout(resolve, 5500));
  assert.equal(reads.length, stoppedReads, 'Turning page refresh off did not stop polling');
  assert.equal(config.enabled,false,'Changing query settings enabled model wake');
  assert.equal(config.execution.model,'fixture-model','Query settings changed execution model');
  assert.ok(posts.every(p=>['config','quota-query','quota-connection-check','prompt-generation'].includes(p.path)),'Query controls called an unexpected model/scheduler operation');
  assert.equal(posts.filter(post=>post.path==='prompt-generation').length,1,'Query controls repeated content generation');
  await waitFor(()=>evaluate('document.getElementById("save-bar").hidden'),'Successful save did not clear pending changes');
  capture = await cdp('Page.captureScreenshot');
  await writeFile(join(temp,'panel-query-desktop.png'),Buffer.from(capture.data,'base64'));
  await evaluate('document.querySelector("[data-tab=rules]").click()');
  capture = await cdp('Page.captureScreenshot');
  await writeFile(join(temp,'panel-rules-desktop.png'),Buffer.from(capture.data,'base64'));
  await cdp('Emulation.setDeviceMetricsOverride', { width: 390, height: 844, deviceScaleFactor: 1, mobile: true });
  for (const tab of ['overview','accounts','query','rules','plan','capabilities']) {
    await evaluate(`document.querySelector('[data-tab="${tab}"]').click()`);
    assert.ok(await evaluate('document.documentElement.scrollWidth <= innerWidth + 1'), 'Mobile viewport has horizontal overflow: '+tab);
    if (tab === 'overview') assert.ok(await evaluate('document.getElementById("quota-query").getBoundingClientRect().top < innerHeight'), 'Primary quota action is below the mobile fold');
    if (tab === 'accounts') assert.equal(await evaluate('getComputedStyle(document.querySelector(".desktop-layout")).display'), 'none', 'Desktop layout selector occupied the mobile screen');
    capture = await cdp('Page.captureScreenshot');
    await writeFile(join(temp,`panel-${tab}-mobile.png`),Buffer.from(capture.data,'base64'));
  }
  await evaluate('document.querySelector("[data-tab=capabilities]").click(); document.getElementById("advanced-editor").open=true');
  assert.ok(await evaluate('document.documentElement.scrollWidth <= innerWidth + 1'),'Expanded mobile JSON editor overflowed');
  await evaluate('document.querySelector("[data-tab=accounts]").click()');
  capture = await cdp('Page.captureScreenshot');
  await writeFile(join(temp, 'panel-mobile.png'), Buffer.from(capture.data, 'base64'));
  unauthorized = true;
  await evaluate(`document.querySelector('[data-tab="overview"]').click(); document.getElementById('refresh').click()`);
  await waitFor(() => evaluate('document.getElementById("workspace").hidden'), '401 did not disconnect');
  assert.equal(await evaluate('document.getElementById("management-key").value'), '', '401 retained key input');
  assert.equal(await evaluate('document.getElementById("account-list").textContent.includes("fixture-account")'), false, '401 retained account data');
  assert.equal(await evaluate('document.getElementById("config-json").value'), '', '401 retained editor content');
  const beforeRejectedLogin = reads.length;
  await navigate('Page.reload');
  await waitFor(async () => reads.length > beforeRejectedLogin && await evaluate('document.readyState === "complete" && !!document.getElementById("login-section") && !document.getElementById("login-section").hidden && document.getElementById("notice").textContent.includes("认证失败")'), 'Rejected saved key did not return to manual login');
  assert.equal(reads.length - beforeRejectedLogin, 1, 'Rejected saved key retried or dispatched parallel reads');
  assert.equal(await evaluate('localStorage.length'), 3, 'Failed auto-connect changed host login or preferences');
  assert.equal(await evaluate('localStorage.getItem("managementKey")'), hostLogin, 'Failed auto-connect changed host login');
  unauthorized = false;
  await evaluate(`document.getElementById('management-key').value=${JSON.stringify(key)}; document.getElementById('login-form').requestSubmit()`);
  await waitFor(() => evaluate('!document.getElementById("workspace").hidden'), 'Manual fallback did not connect');
  assert.deepEqual(errors, [], 'Browser or fixture errors');
  console.log(`Browser smoke passed: CPA login, bridge diagnosis, field validation and error focus, stable account nodes/focus/selection/order, current pause handlers, plan/history filters and account links, preserved form input after candidate application, config diff, manual queries (${singleQueryLatency} ms after backend completion), six mobile views, JSON editor, 401 cleanup. Screenshots: ${temp}`);
} finally {
  if (ws) ws.close();
  if (browser && browser.exitCode === null && browser.signalCode === null) { browser.kill('SIGTERM'); await new Promise(r => { browser.once('exit', r); setTimeout(r, 1500); }); }
  if (server) await new Promise(r => server.close(r));
  // Keep screenshots for inspection, remove the browser profile and its data.
  for (const name of ['Default', 'Local State', 'DevToolsActivePort', 'ShaderCache', 'GrShaderCache', 'GraphiteDawnCache', 'DawnGraphiteCache', 'DawnWebGPUCache']) {
    try { await rm(join(temp, name), { recursive: true, force: true, maxRetries: 5, retryDelay: 100 }); }
    catch { console.error(`Could not remove temporary browser data: ${join(temp, name)}`); }
  }
}
