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
let unauthorized = false, conflict = false, revision = 'a'.repeat(64);
let config = {
  schema_version: 1, enabled: false, timezone: 'Asia/Shanghai',
  quota_query: {automatic: true, manual: true, interval: '30s'},
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
      const path = req.url.slice(api.length);
      if (req.method === 'POST') {
        let body = ''; for await (const chunk of req) body += chunk;
        posts.push({ path, body });
        if (path === 'quota-query') {
          if (!config.quota_query.manual) {json(res,409,{error:'manual_quota_query_disabled'});return;}
          if (body) {
            const selected = JSON.parse(body);
            assert.deepEqual(Object.keys(selected), ['account_id'], 'Account query received model input');
            assert.ok(Object.hasOwn(accounts, selected.account_id), 'Wrong account selected');
            const accountID = selected.account_id, failed = accountID !== 'fixture-account';
            // Do not publish immediately: the panel must retain its optimistic
            // queued state until the runtime's next snapshot catches up.
            setTimeout(() => {
              summary.scheduler.quota_query = {account_id:accountID,phase:'completed',total:1,completed:failed?0:1,failed:failed?1:0,skipped:0,started_at:new Date().toISOString(),finished_at:new Date().toISOString()};
              if (!failed) {
                accounts[accountID].FiveHour.Remaining.Value = 70;
                accounts[accountID].FiveHour.Source = 'cpa_wham_usage';
              }
            }, 150);
            json(res,202,{status:'queued',account_id:accountID});return;
          }
          summary.scheduler.quota_query = {phase:'completed',total:2,completed:2,failed:0,skipped:0,started_at:new Date().toISOString(),finished_at:new Date().toISOString()};
          accounts['fixture-account'].FiveHour.Remaining.Value = 71;
          accounts['fixture-account'].FiveHour.Source = 'cpa_wham_usage';
          json(res,202,{status:'queued'});return;
        }
        if (path === 'config') {
          const edit = JSON.parse(body);
          if (conflict || edit.revision !== revision) { json(res, 409, { error: 'policy changed; reload the editor before saving' }); return; }
          config = edit.config; revision = 'b'.repeat(64);
          json(res, 200, { config, revision }); return;
        }
        json(res, path === 'recheck' ? 202 : 200, { status: 'scheduled' }); return;
      }
      const values = { summary, accounts, config, capabilities, 'config-editor': { config, revision, disk_matches_running: true } };
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
  const targets = await (await fetch(`http://127.0.0.1:${port}/json/list`)).json();
  ws = new WebSocket(targets.find(t => t.type === 'page').webSocketDebuggerUrl);
  await new Promise((r, j) => { ws.addEventListener('open', r, { once: true }); ws.addEventListener('error', j, { once: true }); });
  let serial = 0; const pending = new Map(), caughtExceptions = [];
  function cdp(method, params = {}) { return new Promise((r, j) => { const id = ++serial; const timer = setTimeout(() => { pending.delete(id); j(new Error(`CDP timeout: ${method}`)); }, 15000); pending.set(id, { r, j, timer }); ws.send(JSON.stringify({ id, method, params })); }); }
  ws.addEventListener('message', event => {
    const data = JSON.parse(event.data);
    if (data.id) { const p = pending.get(data.id); if (!p) return; clearTimeout(p.timer); pending.delete(data.id); data.error ? p.j(new Error(JSON.stringify(data.error))) : p.r(data.result); }
    if (data.method === 'Runtime.exceptionThrown') errors.push(data.params.exceptionDetails.text);
    if (data.method === 'Page.javascriptDialogOpening') void cdp('Page.handleJavaScriptDialog', { accept: true });
    if (data.method === 'Debugger.paused') { caughtExceptions.push(data.params.data?.description || data.params.reason); void cdp('Debugger.resume'); }
  });
  await cdp('Page.enable'); await cdp('Runtime.enable'); await cdp('Debugger.enable');
  await cdp('Debugger.setPauseOnExceptions', { state: 'all' });
  async function evaluate(expression) {
    const result = await cdp('Runtime.evaluate', { expression, returnByValue: true, awaitPromise: true });
    assert.ok(!result.exceptionDetails, JSON.stringify(result.exceptionDetails)); return result.result.value;
  }
  await cdp('Page.navigate', { url });
  await waitFor(() => evaluate('document.readyState === "complete" && !!document.getElementById("connect")'), 'Login did not render');
  await evaluate(`document.getElementById('management-key').value=${JSON.stringify(key)}; document.getElementById('login-form').requestSubmit()`);
  try { await waitFor(() => evaluate('!document.getElementById("workspace").hidden'), 'Login did not succeed'); }
  catch (error) { console.error(await evaluate('document.getElementById("notice").textContent'), errors, caughtExceptions); throw error; }
  assert.equal(posts.length, 0, 'Read-only login must not dispatch operations');
  assert.equal(await evaluate('localStorage.length'), 0, 'Manual login persisted the key');
  assert.equal(await evaluate('document.getElementById("mode-title").textContent'), '仅查看与查询额度', 'Applied mode did not explain model behavior');
  assert.equal(await evaluate('document.getElementById("query-automatic").closest(".view").id'), 'tab-query', 'Query controls are mixed with wake rules');
  assert.equal(await evaluate('document.getElementById("save-bar").hidden'), true, 'Clean draft displayed a pending save bar');
  assert.equal(await evaluate('document.getElementById("quota-query").disabled'),true,'Unready quota bridge allowed a query');
  assert.equal(await evaluate('document.getElementById("bridge-status").textContent.includes("CPA 进程未读取到") && document.getElementById("bridge-help").textContent.includes("CPA_MANAGEMENT_KEY")'),true,'Missing backend key diagnosis was not actionable');
  capabilities.QuotaAdapterReady = true; capabilities.quota_bridge.status = 'configured';
  await waitFor(()=>evaluate('!document.getElementById("quota-query").disabled && document.getElementById("bridge-status").textContent.includes("已就绪")'),'Startup readiness remained stale after automatic polling');
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
      await cdp('Page.reload');
      await waitFor(async () => reads.length >= beforeReload + 5 && await evaluate('document.readyState === "complete" && !!document.getElementById("workspace") && !document.getElementById("workspace").hidden'), 'Saved CPA credentials did not auto-connect');
      assert.equal(await evaluate(`localStorage.getItem(${JSON.stringify(legacy ? 'managementKey' : 'cli-proxy-auth')})`), saved, 'Auto-connect modified host storage');
      assert.equal(await evaluate('document.getElementById("management-key").value'), '', 'Auto-connect exposed key in form');
      await evaluate('document.getElementById("disconnect").click()');
      assert.equal(await evaluate('document.getElementById("workspace").hidden'), true, 'Disconnect did not stay disconnected');
    }
  }
  await cdp('Page.navigate', {url: new URL('/management.html', url).href});
  await waitFor(() => evaluate('!!document.querySelector("iframe")?.contentDocument?.getElementById("workspace") && !document.querySelector("iframe").contentDocument.getElementById("workspace").hidden'), 'CPA menu iframe did not auto-connect');
  await cdp('Page.navigate', {url});
  await waitFor(() => evaluate('document.readyState === "complete" && !!document.getElementById("workspace") && !document.getElementById("workspace").hidden'), 'Reopening did not auto-connect');
  assert.equal(posts.length, 0, 'Auto-connect dispatched operations');
  await evaluate(`document.querySelector('[data-tab="accounts"]').click()`);
  await waitFor(() => evaluate('document.getElementById("account-list").textContent.includes("fixture-account")'), 'Accounts did not render');
  assert.equal(await evaluate('document.getElementById("account-list").querySelectorAll("img").length'), 0, 'Account IDs must render as text');
  assert.equal(await evaluate('document.getElementById("account-list").textContent.includes("正常请求响应")'), true, 'Quota source was not displayed');
  assert.equal(await evaluate('Array.from(document.querySelectorAll(".account-disclosure")).every(item=>!item.open)'),true,'Account diagnostics were not collapsed');
  await evaluate('document.querySelector(".account-disclosure").open=true');
  await waitFor(()=>evaluate('document.querySelector(".account-disclosure").open'),'Account details did not open');
  accounts['fixture-account'].FiveHour.Remaining.Value = 72;
  const pollStarted = Date.now();
  await waitFor(() => evaluate('document.getElementById("account-list").textContent.includes("72%")'), 'Passive quota did not appear on automatic panel polling');
  assert.ok(Date.now() - pollStarted < 8000, 'Panel polling did not use the faster snapshot interval');
  assert.equal(await evaluate('document.querySelector(".account-disclosure").open'),true,'Polling collapsed open account diagnostics');
  assert.equal(posts.length, 0, 'Snapshot polling dispatched operations');
  assert.equal(await evaluate('document.querySelectorAll("[data-quota-account]").length'),2,'Account query buttons missing');
  assert.equal(await evaluate('Array.from(document.querySelectorAll("[data-quota-account]")).every(button=>!button.closest("details"))'),true,'Account query was hidden in diagnostics');
  const singleQueryCount = posts.length;
  await evaluate(`{ const button=Array.from(document.querySelectorAll('[data-quota-account]')).find(item=>item.dataset.quotaAccount==='fixture-account'); button.click(); button.click(); }`);
  await waitFor(()=>posts.length===singleQueryCount+1,'Account query was not sent');
  await waitFor(()=>evaluate('Array.from(document.querySelectorAll("[data-quota-account]")).every(button=>button.disabled)'),'Queued query allowed another target before snapshot');
  await waitFor(()=>evaluate('document.getElementById("account-list").textContent.includes("70%") && document.getElementById("account-list").textContent.includes("查询完成")'),'Single-account result did not update');
  assert.deepEqual(JSON.parse(posts.at(-1).body),{account_id:'fixture-account'},'Account query widened to all accounts');
  assert.equal(posts.length,singleQueryCount+1,'Double click duplicated the account query');
  assert.equal(accounts['<img src=x onerror=alert(1)>'].FiveHour.State,'unknown','Selected query changed another account');
  await evaluate(`Array.from(document.querySelectorAll('[data-quota-account]')).find(item=>item.dataset.quotaAccount!=='fixture-account').click()`);
  await waitFor(()=>evaluate('document.getElementById("account-list").textContent.includes("查询失败，可手动重试")'),'Account query failure was not displayed');
  assert.equal(JSON.parse(posts.at(-1).body).account_id,'<img src=x onerror=alert(1)>','Account ID was not passed literally in JSON');
  assert.equal(await evaluate('document.getElementById("account-list").querySelectorAll("img").length'),0,'Account query introduced unsafe rendering');
  await evaluate('document.querySelector(".account-disclosure").open=false; document.querySelector("[data-tab=overview]").click()');
  await waitFor(() => evaluate('!document.getElementById("quota-query").disabled'), 'Manual query was not enabled');
  await evaluate('document.getElementById("quota-query").click()');
  await waitFor(() => evaluate('document.getElementById("quota-query-status").textContent.includes("查询结束")'), 'Manual query progress did not display');
  assert.equal(posts.filter(p=>p.path==='quota-query').length,3,'Manual query duplicated');
  assert.equal(await evaluate('document.getElementById("account-list").textContent.includes("71%") && document.getElementById("account-list").textContent.includes("主动查询")'),true,'Manual quota did not update panel');
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
  await evaluate(`{ document.querySelector('[data-tab="rules"]').click(); const ratio=document.getElementById('rule-ratio'); ratio.value='1.5'; ratio.dispatchEvent(new Event('input',{bubbles:true})); ratio.dispatchEvent(new Event('change',{bubbles:true})); }`);
  await evaluate(`document.getElementById('config-save').click()`);
  await waitFor(() => config.activity.accounts_per_user === 1.5, 'Rule save failed');
  await waitFor(() => evaluate('!document.getElementById("rule-ratio").disabled'), 'Save did not finish updating the view');
  assert.equal(config.execution.verify_delays[2], '120s', 'Unedited advanced settings were lost');
  assert.equal(config.daily_rules[0].id, 'morning', 'Daily rules were lost');
  conflict = true;
  await evaluate(`{ const ratio=document.getElementById('rule-ratio'); ratio.value='2'; ratio.dispatchEvent(new Event('change',{bubbles:true})); document.getElementById('config-save').click(); }`);
  await waitFor(() => posts.filter(p => p.path === 'config').length === 2, 'Conflict save was not sent');
  await waitFor(() => evaluate('document.getElementById("notice").textContent.includes("其他操作修改") || document.getElementById("notice").textContent.includes("冲突")'), 'Conflict did not display');
  assert.equal(await evaluate('document.getElementById("rule-ratio").value'), '2', 'Conflict discarded draft');
  await evaluate('document.querySelector("[data-tab=query]").click()');
  assert.equal(await evaluate('document.getElementById("save-bar").hidden'),false,'Cross-tab edits lost the shared save bar');
  assert.equal(await evaluate('document.getElementById("rule-ratio").value'),'2','Navigation discarded wake draft');
  conflict = false;
  await evaluate(`{document.getElementById('query-automatic').checked=false; document.getElementById('query-automatic').dispatchEvent(new Event('change',{bubbles:true})); document.getElementById('query-manual').checked=false; document.getElementById('query-manual').dispatchEvent(new Event('change',{bubbles:true})); document.getElementById('config-save').click();}`);
  await waitFor(()=>config.quota_query.automatic===false && config.quota_query.manual===false,'Query switches did not persist');
  await waitFor(()=>evaluate('document.getElementById("quota-query").disabled && document.getElementById("quota-query-status").textContent.includes("定期查询：关闭")'),'Saved manual/automatic switches did not apply');
  assert.equal(await evaluate('Array.from(document.querySelectorAll("[data-quota-account]")).every(button=>button.disabled)'),true,'Account query bypassed disabled manual permission');
  assert.equal(config.enabled,false,'Changing query settings enabled model wake');
  assert.equal(config.execution.model,'fixture-model','Query settings changed execution model');
  assert.ok(posts.every(p=>['config','quota-query'].includes(p.path)),'Query controls called a model/scheduler operation');
  await waitFor(()=>evaluate('document.getElementById("save-bar").hidden'),'Successful save did not clear pending changes');
  capture = await cdp('Page.captureScreenshot');
  await writeFile(join(temp,'panel-query-desktop.png'),Buffer.from(capture.data,'base64'));
  await evaluate('document.querySelector("[data-tab=rules]").click()');
  capture = await cdp('Page.captureScreenshot');
  await writeFile(join(temp,'panel-rules-desktop.png'),Buffer.from(capture.data,'base64'));
  await cdp('Emulation.setDeviceMetricsOverride', { width: 390, height: 844, deviceScaleFactor: 1, mobile: true });
  for (const tab of ['overview','accounts','query','rules','capabilities']) {
    await evaluate(`document.querySelector('[data-tab="${tab}"]').click()`);
    assert.ok(await evaluate('document.documentElement.scrollWidth <= innerWidth + 1'), 'Mobile viewport has horizontal overflow: '+tab);
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
  await cdp('Page.reload');
  await waitFor(async () => reads.length > beforeRejectedLogin && await evaluate('document.readyState === "complete" && !!document.getElementById("login-section") && !document.getElementById("login-section").hidden && document.getElementById("notice").textContent.includes("认证失败")'), 'Rejected saved key did not return to manual login');
  assert.equal(reads.length - beforeRejectedLogin, 1, 'Rejected saved key retried or dispatched parallel reads');
  assert.equal(await evaluate('localStorage.length'), 1, 'Failed auto-connect changed host login');
  unauthorized = false;
  await evaluate(`document.getElementById('management-key').value=${JSON.stringify(key)}; document.getElementById('login-form').requestSubmit()`);
  await waitFor(() => evaluate('!document.getElementById("workspace").hidden'), 'Manual fallback did not connect');
  assert.deepEqual(errors, [], 'Browser or fixture errors');
  console.log(`Browser smoke passed: CPA login, live bridge readiness/diagnosis, query/wake separation, mode summary, shared drafts/save bar, persistent account disclosures, keyboard tabs, quota sources/polling, manual and single-account queries, five mobile views, JSON editor, 401 cleanup. Screenshots: ${temp}`);
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
