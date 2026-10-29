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
const autorefreshOnly = process.env.CWR_AUTOREFRESH_ONLY === '1';
const schedulingOnly = process.env.CWR_SCHEDULING_ONLY === '1';
const projectsOnly = process.env.CWR_PROJECTS_ONLY === '1';
const output = process.env.CWR_SCREENSHOT_DIR || join(tmpdir(), 'cwr-v2-panel-preview');
const temporary = await mkdtemp(join(tmpdir(), 'cwr-v2-browser-'));
const posts = [], patches = [], reads = [], exceptions = [], pageErrors = [];
const jobs = new Map();
const resourceReadGates = new Map(), unavailableResources = new Set();
let accountReadGate, settingsPostGate, settingsReadGate, modelReadGate, wakeReadGate, historyDetailGate;
let historyUnavailable = false;
let authReadGate, authPatchGate, authFilesOverride;
let revision = 1, accountsRevision = 1, settingsRevision = 1, settingsConflictOnce = false, unauthorized = false, browser, server, socket;
let modelRevision = 0, modelConflictOnce = false;
let schedulingRevision = 0;
let resetCardsFixture = null;
let schedulingConfig = {enabled:false,prefer_reset:true,quota_max_age_seconds:300,session_ttl_seconds:1800,max_inflight:2,projects:[],cards:{enabled:false,auto_consume:false,poll_seconds:900,expiry_seconds:86400,force_before_seconds:1800,min_used_percent:80}};
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
const overviewObservedAt = observed();
const later = seconds => new Date(Date.now() + seconds * 1000).toISOString();
const accounts = Array.from({length: 24}, (_, index) => {
  const percent = [82, 100, 46, 65, 0, 0][index] ?? 72;
  return {id: 'codex-' + String(index + 1).padStart(2, '0'), status: [4, 22].includes(index) ? 'disabled' : [5, 23].includes(index) ? 'unavailable' : 'available', observed_at: observed(),
    quota: index === 4 ? undefined : {source: 'cpa_wham_usage', observed_at: observed(), permission: index === 5 ? 'blocked' : 'allowed',
      five_hour: {State: 'active', Remaining: {Value: percent, Unit: 'percent'}, ResetAt: later(4320), ConfirmedAt: observed()},
      weekly: {State: 'active', Remaining: {Value: [67, 91, 58, 34, 0, 12][index] ?? 67, Unit: 'percent'}, ResetAt: later(201600), ConfirmedAt: observed()}}};
});
const authFiles = accounts.map((account, index) => ({id: account.id, name: index === 0 ? 'real+primary.codex.json' : account.id + '.json',
  provider: 'codex', source: 'memory', auth_index: 'fixture-index-' + index, disabled: account.status === 'disabled', runtime_only: index === 2,
  ...(index === 3 ? {plugin_virtual_child: true} : {}), status: account.status === 'disabled' ? 'disabled' : 'active'}));
const fixtureJobs = [
  {id: 'fixture-running', target: 'codex-07', phase: 'running', trigger: 'manual_single', querying: 1, waiting: 0, completed: 0, failed: 0, total: 1, created_at: observed(), updated_at: observed()},
  {id: 'fixture-waiting', target: 'codex-02', phase: 'running', trigger: 'periodic', scheduled_at: later(-60), hold_reason: 'excluded_hours', querying: 0, waiting: 1, completed: 0, failed: 0, total: 1, created_at: observed(), updated_at: observed()},
  {id: 'fixture-bulk', target: '', phase: 'running', trigger: 'manual_batch', scheduled_at: later(120), querying: 1, waiting: 23, completed: 0, failed: 0, total: 24, created_at: observed(), updated_at: observed()}
];
accounts[0].pending_query = {phase: 'queued', scheduled_at: later(90), trigger: 'manual_batch', job_id: 'fixture-bulk', hold_reason: ''};
for (const job of fixtureJobs) jobs.set(job.id, {...job, job_id: job.id, items: (job.target ? accounts.filter(account => account.id === job.target) : accounts).map((account, index) => ({account_id: account.id, phase: index < job.querying ? 'running' : 'queued', scheduled_at: job.scheduled_at, trigger: job.trigger, hold_reason: job.hold_reason || ''}))});
for (let index = 0; index < 36; index++) {
  const jobID = 'persisted-query-' + String(index).padStart(2, '0');
  const phase = ['completed', 'failed', 'queued', 'running', 'cancelled'][index % 5];
  const trigger = ['periodic', 'five_hour_expiry', 'weekly_expiry', 'wake_verification', 'manual_single', 'manual_batch', 'initial_sync', 'wake_preflight'][index % 8];
  const stamp = later(-3600 - index * 3600);
  const error = phase === 'failed' ? 'quota_upstream_forbidden' : '';
  jobs.set(jobID, {job_id: jobID, account_id: 'persisted-account-' + String(index).padStart(2, '0'), phase, trigger, created_at: stamp, updated_at: stamp,
    started_at: ['running', 'completed', 'failed'].includes(phase) ? new Date(Date.parse(stamp) - 2500).toISOString() : '', error_code: error,
    items: [{account_id: 'persisted-account-' + String(index).padStart(2, '0'), phase, trigger, error_code: error}]});
}
const responseRecord = {recorded_at: observed(), stage: 'read', http_status: 429, event: 'response.failed', status: 'failed', upstream_error_code: 'rate_limit_exceeded',
  error_message: 'Please try later [已隐藏] <img src=x onerror=alert(1)>', response_text: 'Partial safe response [已隐藏]\n' + 'Long persisted response line for scroll verification.\n'.repeat(60), error_code: 'wake_delivery_unknown', bytes: 4096, completed: false, truncated: true};
for (const [index, phase, reason] of [[0, 'unknown', 'weekly_reset'], [1, 'completed', 'five_hour_reset'], [2, 'skipped', 'five_hour_reset'], [3, 'queued', 'daily'], [4, 'cancelled', 'daily']]) {
  const jobID = 'persisted-wake-' + index, stamp = later(-1800 - index * 3600);
  wakeJobs.set(jobID, {job_id: jobID, account_id: 'persisted-wake-account-' + index, phase, reason, model: ['unknown', 'completed'].includes(phase) ? 'fixture-recorded-model' : '', created_at: stamp, updated_at: stamp,
    error_code: phase === 'unknown' ? 'wake_outcome_unknown' : phase === 'completed' ? 'window_verified' : '', hold_reason: phase === 'unknown' ? 'verification_pending' : '', response: phase === 'unknown' ? responseRecord : undefined});
}
const identity = () => ({instance_id: 'local-browser-fixture', view_revision: revision});
function taskHistory(parameters) {
  const running = ['running', 'checking', 'prepared', 'sending', 'verifying'], queued = ['queued', 'blocked'];
  const records = [...[...jobs.values()].map(job => ({...job, type: 'query', trigger: job.trigger || 'manual_single'})),
    ...[...wakeJobs.values()].map(job => ({...job, type: 'wake', trigger: job.reason || 'manual'}))].map(job => ({
    job_id: job.job_id, type: job.type,
    source: (job.type === 'query' ? ['manual_single', 'manual_batch'].includes(job.trigger) : job.trigger === 'manual') ? 'manual' : 'automatic',
    trigger: job.trigger, phase: job.phase,
    status: job.type === 'query' && job.phase === 'running' && job.items?.some(item => item.phase === 'queued') && !job.items.some(item => item.phase === 'running') ? 'queued' : running.includes(job.phase) ? 'running' : queued.includes(job.phase) ? 'queued' : job.phase,
    account_id: job.account_id || job.target || '', model: job.model || '', created_at: job.created_at, updated_at: job.updated_at,
    scheduled_at: job.scheduled_at || '', started_at: job.type === 'wake' ? '' : job.started_at || '',
    finished_at: job.type === 'query' && ['completed', 'failed', 'cancelled'].includes(job.phase) ? job.updated_at : '', error_code: job.error_code || '', hold_reason: job.hold_reason || '',
    total: job.total ?? job.items?.length ?? 1, completed: job.completed ?? (job.phase === 'completed' ? 1 : 0), failed: job.failed ?? (job.phase === 'failed' ? 1 : 0),
    waiting: job.waiting ?? (queued.includes(job.phase) ? 1 : 0), querying: job.querying ?? (running.includes(job.phase) ? 1 : 0)
  }));
  const value = name => parameters.get(name) || '';
  const matches = (record, name, field = name) => !value(name) || value(name) === 'all' || record[field] === value(name);
  const filtered = records.filter(record => matches(record, 'type') && matches(record, 'source') && matches(record, 'trigger') &&
    (!value('search') || [record.job_id, record.account_id, record.model, record.error_code].join(' ').toLowerCase().includes(value('search').toLowerCase())) &&
    (!value('from') || Date.parse(record.created_at) >= Date.parse(value('from'))) && (!value('to') || Date.parse(record.created_at) < Date.parse(value('to'))));
  const stats = {total: filtered.length, today: 0, active: 0, running: 0, queued: 0, completed: 0, failed: 0, cancelled: 0, skipped: 0, unknown: 0, today_manual: 0, today_automatic: 0, by_status: {}};
  const todayFrom = Date.parse(value('today_from') || new Date().toISOString().slice(0, 10)), todayTo = value('today_to') ? Date.parse(value('today_to')) : todayFrom + 86400000;
  for (const record of filtered) {
    stats.by_status[record.status] = (stats.by_status[record.status] || 0) + 1;
    if (['completed', 'failed', 'cancelled', 'skipped', 'unknown'].includes(record.status)) stats[record.status]++;
    if (running.includes(record.status)) stats.running++;
    if (queued.includes(record.status)) stats.queued++;
    if (Date.parse(record.created_at) >= todayFrom && Date.parse(record.created_at) < todayTo) { stats.today++; stats['today_' + record.source]++; }
  }
  stats.active = stats.running + stats.queued + stats.unknown;
  const status = value('status'), selected = filtered.filter(record => !status || status === 'all' || record.status === status);
  selected.sort((first, second) => Date.parse(second.created_at) - Date.parse(first.created_at) || first.type.localeCompare(second.type) || second.job_id.localeCompare(first.job_id));
  const limit = Math.min(100, Math.max(1, Number(value('limit') || 10))), pages = Math.max(1, Math.ceil(selected.length / limit)), page = Math.min(pages, Math.max(1, Number(value('page') || 1)));
  return {...identity(), records: selected.slice((page - 1) * limit, page * limit), page, limit, total: selected.length,
    status_counts: {all: stats.total, running: stats.running, queued: stats.queued, completed: stats.completed, failed: stats.failed, unknown: stats.unknown, skipped: stats.skipped, cancelled: stats.cancelled},
    summary: {today: stats.today, today_manual: stats.today_manual, today_automatic: stats.today_automatic, running: stats.running, queued: stats.queued, completed: stats.completed, failed: stats.failed, unknown: stats.unknown}};
}
function accountDisplayStatus(account) {
  if (account.status !== 'available') return account.status;
  if (['five_hour', 'weekly'].some(name => account.quota?.[name]?.Remaining?.Value === 0)) return 'quota_exhausted';
  return account.quota?.permission === 'blocked' ? 'quota_restricted' : 'available';
}
const settings = () => ({...identity(), revision: settingsRevision, config: queryConfig});
const modelSettings = () => ({...identity(), revision: modelRevision, config: modelConfig});
function json(response, status, body) {
  response.writeHead(status, {'Content-Type': 'application/json', 'Cache-Control': 'no-store'}); response.end(JSON.stringify(body));
}
async function waitFor(check, message, timeout = 12000) {
  const until = Date.now() + timeout;
  while (Date.now() < until) { if (await check()) return; await new Promise(resolve => setTimeout(resolve, 50)); }
  throw new Error(typeof message === 'function' ? await message() : message);
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
      if (url.pathname.startsWith('/other-cpa/')) url.pathname = '/cpa/' + url.pathname.slice('/other-cpa/'.length);
      if (url.pathname.startsWith(base)) {
        const name = url.pathname.slice(base.length), filenames = {status: 'index.html', 'panel.js': 'panel.js', 'panel.css': 'panel.css'};
        if (!filenames[name]) { response.writeHead(404); response.end(); return; }
        response.writeHead(200, {'Content-Type': name === 'status' ? 'text/html; charset=utf-8' : name.endsWith('.js') ? 'text/javascript' : 'text/css',
          'Content-Security-Policy': "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'self'; base-uri 'none'; form-action 'none'"});
        response.end(await readFile(name === 'panel.js' && process.env.CWR_FIXTURE_PANEL_JS || join(root, 'internal/v2/daemon/web', filenames[name]))); return;
      }
      if (url.pathname === '/cpa/v0/management/auth-files/status') {
        if (request.headers.authorization !== 'Bearer ' + fixtureKey || unauthorized) { json(response, 401, {error: 'unauthorized'}); return; }
        assert.equal(request.method, 'PATCH', 'Auth status must never upload or overwrite credentials');
        let body = ''; for await (const chunk of request) body += chunk;
        const data = JSON.parse(body); patches.push({body: data, headers: request.headers, path: url.pathname});
        assert.deepEqual(Object.keys(data).sort(), ['auth_index', 'disabled', 'name']);
        assert.equal(typeof data.disabled, 'boolean');
        const matches = authFiles.filter(file => file.name === data.name);
        assert.equal(matches.length, 1, 'Status PATCH must target the exact unique filename, not an account ID');
        const file = matches[0]; assert.equal(data.auth_index, file.auth_index); assert.equal(file.runtime_only, false); assert.notEqual(file.plugin_virtual_child, true);
        const held = authPatchGate;
        if (held) { held.started = true; await held.promise; }
        if (held?.outcome === 'reject') { json(response, 400, {error: 'invalid_request'}); return; }
        file.disabled = data.disabled; file.status = data.disabled ? 'disabled' : 'active';
        const account = accounts.find(account => account.id === file.id); account.status = data.disabled ? 'disabled' : 'available';
        accountsRevision++; revision++;
        if (held?.outcome === 'persist-hook-failed') { json(response, 500, {error: 'post_persist_hook_failed'}); return; }
        if (held?.outcome === 'disconnect') {
          response.writeHead(200, {'Content-Type': 'application/json'}); response.write('{"status":');
          setTimeout(() => response.destroy(), 50); return;
        }
        json(response, 200, {status: 'ok', disabled: file.disabled}); return;
      }
      if (url.pathname === '/cpa/v0/management/auth-files' || url.pathname === '/cpa/v0/management/auth-files/models') {
        if (request.headers.authorization !== 'Bearer ' + fixtureKey || unauthorized) { json(response, 401, {error: 'unauthorized'}); return; }
        assert.equal(request.method, 'GET'); reads.push(url.pathname + url.search);
        if (url.pathname.endsWith('/models')) {
          assert.equal(url.searchParams.get('name'), 'codex-01');
          json(response, 200, {models: [{id: 'fixture-codex-catalog'}, {id: 'fixture-codex-other'}]});
        } else {
          const files = structuredClone(authFilesOverride || authFiles), held = authReadGate;
          if (held) { held.started = true; await held.promise; }
          if (held?.outcome === 'reject') { json(response, 503, {error: 'inventory_unavailable'}); return; }
          json(response, 200, {files});
        }
        return;
      }
      if (!url.pathname.startsWith(api)) { response.writeHead(404); response.end(); return; }
      if (request.headers.authorization !== 'Bearer ' + fixtureKey || unauthorized) { json(response, 401, {error: 'unauthorized'}); return; }
      const resource = url.pathname.slice(api.length);
      if (request.method === 'POST') {
        let body = ''; for await (const chunk of request) body += chunk;
        posts.push({resource, body, headers: request.headers});
        if (resource === 'scheduling') {
          const data = JSON.parse(body);
          assert.equal(data.revision, schedulingRevision);
          assert.equal(typeof data.config.subagent_affinity, 'boolean');
          assert.ok(['quota', 'any'].includes(data.config.rebind_on));
          schedulingConfig = data.config; schedulingRevision++; revision++;
          json(response, 200, {instance_id: identity().instance_id, revision: schedulingRevision, config: schedulingConfig}); return;
        }
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
      const resourceGate = resourceReadGates.get(resource);
      if (resourceGate) { resourceGate.started = true; resourceGate.arrivals = (resourceGate.arrivals || 0) + 1; await resourceGate.promise; }
      if (unavailableResources.has(resource)) { json(response, 503, {error: resource + '_unavailable'}); return; }
      if (resource === 'task-history') {
        assert.equal(request.method, 'GET');
        if (historyUnavailable) { json(response, 503, {error: 'task_history_unavailable'}); return; }
        json(response, 200, taskHistory(url.searchParams)); return;
      }
      if (resource === 'wake-settings') {
        if (wakeReadGate) { wakeReadGate.started = true; await wakeReadGate.promise; }
        json(response, 200, {...identity(), revision: wakeRevision, config: wakeConfig}); return;
      }
      if (resource === 'wake-jobs') {
        const id = url.searchParams.get('job_id');
        if (!id) { assert.equal(url.searchParams.get('limit'), '20'); json(response, 200, {...identity(), jobs: [...wakeJobs.values()]}); return; }
        const held = historyDetailGate;
        if (held?.jobID === id) { held.started = true; await held.promise; json(response, 200, held.response); return; }
        const job = wakeJobs.get(id); if (!job) { json(response, 404, {error: 'not_found'}); return; }
        if (job.reason === 'manual' && job.key) { job.phase = 'unknown'; job.hold_reason = 'verification_pending'; job.updated_at = observed(); revision++; job.response = responseRecord; }
        json(response, 200, {...identity(), ...job}); return;
      }
      if (resource === 'scheduling') { json(response,200,{instance_id:identity().instance_id,revision:schedulingRevision,config:schedulingConfig}); return; }
      if (resource === 'reset-cards') { json(response,200,{instance_id:identity().instance_id,...(resetCardsFixture || {accounts:[{account_id:'codex-01',credits:{availableCount:2,credits:null},observed_at:observed(),summary_observed_at:observed(),error_code:''}],attempts:[]})}); return; }
      if (resource === 'generation-accounts') { json(response, 200, {instance_id: identity().instance_id, accounts: generationAccounts}); return; }
      if (resource === 'model-settings') {
        if (modelReadGate) { modelReadGate.started = true; await modelReadGate.promise; }
        json(response, 200, modelSettings()); return;
      }
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
          inventory: {status: 'ready', last_success_at: overviewObservedAt}, dashboard: {total: 24, available: 20, disabled: 2, unavailable: 2, capacity: 16, unknown_quota: 4, errors: 2,
            tasks: {active: 3, querying: 2}, recent_jobs: fixtureJobs, active_jobs: fixtureJobs,
            recent_events: [{kind: 'quota_query', at: overviewObservedAt, account_id: 'codex-07', phase: 'running'}, {kind: 'quota_query', at: overviewObservedAt, account_id: '', phase: 'queued'}, {kind: 'inventory', at: overviewObservedAt, phase: 'ready'}]}}); return;
      }
      if (resource === 'jobs') {
        const job = jobs.get(url.searchParams.get('job_id')), detail = job ? {...job, ...identity()} : {error: 'not_found'};
        delete detail.started_at; delete detail.finished_at;
        json(response, job ? 200 : 404, detail); return;
      }
      if (resource === 'accounts') {
        const held = accountReadGate, snapshot = held?.snapshot ? {identity: identity(), revision: accountsRevision, accounts: structuredClone(accounts)} : undefined;
        if (held) { held.started = true; await held.promise; }
        if (url.searchParams.has('after') && Number(url.searchParams.get('revision')) !== accountsRevision) { json(response, 409, {error: 'view_changed'}); return; }
        const search = (url.searchParams.get('search') || '').toLowerCase(), status = url.searchParams.get('status') || 'all';
        const filtered = (snapshot?.accounts || accounts).filter(account => account.id.toLowerCase().includes(search) && (status === 'all' || status === accountDisplayStatus(account) || status === 'attention' && !['available', 'disabled'].includes(accountDisplayStatus(account))));
        const page = filtered.filter(account => account.id > (url.searchParams.get('after') || '')).slice(0, Number(url.searchParams.get('limit') || 6));
        const last = page.at(-1)?.id, next = filtered.some(account => account.id > last) ? last : '';
        json(response, 200, {...(snapshot?.identity || identity()), accounts_revision: snapshot?.revision ?? accountsRevision, accounts: page, next_after: next, total: 24, filter_total: filtered.length, offset: filtered.filter(account=>account.id <= (url.searchParams.get('after') || '')).length}); return;
      }
      json(response, 404, {error: 'not_found'});
    } catch (error) { exceptions.push(String(error)); json(response, 500, {error: 'fixture_failed'}); }
  });
  await new Promise((resolve, reject) => { server.once('error', reject); server.listen(0, '127.0.0.1', resolve); });
  const origin = 'http://127.0.0.1:' + server.address().port;
  const httpOrigin = 'http://cpa-fixture.test:' + server.address().port, fixtureOrigins = new Set([origin, httpOrigin]);
  browser = spawn(executable, ['--headless', '--no-sandbox', '--disable-dev-shm-usage', '--disable-background-networking', '--disable-gpu', '--no-first-run', '--no-default-browser-check', '--no-proxy-server', '--host-resolver-rules=MAP cpa-fixture.test 127.0.0.1', '--remote-debugging-port=0', '--user-data-dir=' + temporary, 'about:blank'], {stdio: ['ignore', 'ignore', 'pipe']});
  let log = '';
  browser.stderr.on('data', chunk => { log = (log + chunk.toString()).slice(-8000); });
  browser.on('error', error => exceptions.push(String(error)));
  let port;
  await waitFor(async () => {
    if (browser.exitCode !== null || browser.signalCode !== null) throw new Error('Chromium exited: ' + log);
    try { port = (await readFile(join(temporary, 'DevToolsActivePort'), 'utf8')).split('\n')[0]; return !!port; } catch { return false; }
  }, 'Chromium did not start', 30000);
  let target;
  await waitFor(async () => {
    target = (await (await fetch('http://127.0.0.1:' + port + '/json/list')).json()).find(target => target.type === 'page' && target.webSocketDebuggerUrl);
    return !!target;
  }, 'Chromium page target did not become ready', 30000);
  socket = new WebSocket(target.webSocketDebuggerUrl);
  await new Promise((resolve, reject) => { socket.addEventListener('open', resolve, {once: true}); socket.addEventListener('error', reject, {once: true}); });
  let serial = 0;
  const pending = new Map();
  const networkRequests = [], inFlightRequests = new Set();
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
    if (data.method === 'Network.requestWillBeSent' && /^https?:/.test(data.params.request.url)) {
      const request = data.params.request;
      networkRequests.push({url: request.url, method: request.method}); inFlightRequests.add(data.params.requestId);
      if (!fixtureOrigins.has(new URL(request.url).origin)) pageErrors.push('Non-fixture network request: ' + request.url);
    }
    if (['Network.loadingFinished', 'Network.loadingFailed'].includes(data.method)) inFlightRequests.delete(data.params.requestId);
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
    const feedback = await evaluate(`(() => {
      const node = ['rules-message', 'settings-message'].map(id => document.getElementById(id)).find(node => !node.hidden);
      return node && {role: node.getAttribute('role'), toast: !!node.closest('#toast-region'), text: node.textContent.trim()};
    })()`);
    assert.ok(feedback?.toast, 'Rule feedback must use the shared toast region');
    assert.ok(['status', 'alert'].includes(feedback.role));
    assert.notEqual(feedback.text, '', 'Overview rule feedback is empty');
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
  async function runSchedulingChecks() {
    const writeCount = posts.length, patchCount = patches.length;
    await evaluate('document.querySelector("[data-page=scheduling]").click()');
    await waitFor(() => evaluate('!document.getElementById("scheduling-fields").disabled'), 'Scheduling did not load');
    await evaluate('document.getElementById("cards-force").value="90000";document.getElementById("scheduling-form").requestSubmit()');
    assert.equal(await evaluate('!document.getElementById("scheduling-validation").hidden && document.getElementById("cards-advanced").open && document.activeElement.id==="cards-force"'), true, 'Invalid advanced settings were not revealed and focused');
    assert.equal(posts.length, writeCount, 'Invalid configuration reached the backend');
    await evaluate('document.getElementById("scheduling-reload").click()');
    await waitFor(() => evaluate('!document.getElementById("scheduling-fields").disabled && document.getElementById("cards-force").value==="1800"'), 'Draft discard did not finish');
    await evaluate('document.getElementById("cards-enabled").click()');
    assert.equal(await evaluate('document.getElementById("cards-auto").disabled'), false);
    await evaluate('document.getElementById("cards-auto").click();document.getElementById("cards-enabled").click()');
    assert.equal(await evaluate('document.getElementById("cards-auto").disabled && !document.getElementById("cards-auto").checked'), true);
    await evaluate('document.getElementById("scheduling-reload").click()');
    await waitFor(() => evaluate('!document.getElementById("scheduling-fields").disabled'), 'Switch draft discard did not finish');
    resetCardsFixture = {accounts: Array.from({length: 12}, (_, index) => ({
      account_id: index === 0 ? 'long-account-' + 'x'.repeat(230) : 'reset-account-' + index,
      credits: {availableCount: 1, credits: [{id: 'credit-' + index, status: 'available', expiresAt: Math.floor(Date.now() / 1000) + 3600}]},
      summary_observed_at: '2026-10-29T01:00:00Z', details_observed_at: '2026-10-28T01:00:00Z', error_code: ''
    })), attempts: [{id: 'attempt-1', account_id: 'reset-account-1', credit_id: 'credit-1', phase: 'skipped', outcome: 'nothingToReset', updated_at: '2026-10-29T02:00:00Z', error_code: ''},
      {id: 'attempt-2', account_id: 'reset-account-2', credit_id: '<img src=x onerror=alert(1)>', phase: 'unknown', outcome: '', updated_at: '2026-10-29T03:00:00Z', error_code: 'reset_timeout'}]};
    await evaluate('document.getElementById("reset-refresh").click()');
    await waitFor(() => evaluate('document.getElementById("reset-page-summary").textContent.includes("12 个账号")'), 'Reset fixture did not load');
    await evaluate('globalThis.resetDetail=document.querySelector("#reset-cards-list details");resetDetail.open=true;globalThis.resetSummary=resetDetail.querySelector("summary");resetSummary.focus()');
    resetCardsFixture.accounts[0].summary_observed_at = '2026-10-29T04:00:00Z';
    await evaluate('document.getElementById("reset-refresh").click()'); await settleRefresh();
    assert.equal(await evaluate('resetDetail.isConnected && resetDetail.open && document.activeElement===resetSummary'), true, 'Reset polling replaced expanded details');
    for (const width of [1586, 768, 390, 320]) {
      await cdp('Emulation.setDeviceMetricsOverride', {width, height: 992, deviceScaleFactor: 1, mobile: width < 600});
      await evaluate('document.getElementById("routing-text-editor").open=true');
      const layout = await evaluate('(()=>{const f=document.getElementById("scheduling-fields"),t=document.getElementById("routing-projects");return {overflow:document.documentElement.scrollWidth>innerWidth,border:getComputedStyle(f).borderWidth,textarea:t.clientWidth,parent:t.parentElement.clientWidth}})()');
      assert.equal(layout.overflow, false, 'Scheduling overflow at ' + width);
      assert.equal(layout.border, '0px', 'Scheduling kept the browser fieldset border');
      assert.ok(layout.textarea > layout.parent - 55, 'Project editor did not use the card width');
      await evaluate('document.getElementById("routing-text-editor").open=false');
      await evaluate('document.getElementById("cards-advanced").open=true;document.getElementById("routing-advanced").open=true');
      assert.equal(await evaluate('document.documentElement.scrollWidth<=innerWidth'), true, 'Advanced settings overflow at ' + width);
      await evaluate('document.getElementById("cards-advanced").open=false;document.getElementById("routing-advanced").open=false');
      await evaluate('scrollTo(0,0)');
      const metrics = await cdp('Page.getLayoutMetrics'), clip = {x: 0, y: 0, width, height: Math.ceil(metrics.cssContentSize.height), scale: 1};
      const capture = await cdp('Page.captureScreenshot', {format: 'png', captureBeyondViewport: true, clip});
      await mkdir(output, {recursive: true}); await writeFile(join(output, 'scheduling-review-' + width + '.png'), Buffer.from(capture.data, 'base64'));
    }
    await evaluate('document.getElementById("reset-next").click()');
    assert.equal(await evaluate('document.getElementById("reset-cards-list").children.length'), 2);
    await evaluate('document.getElementById("reset-search").value="credit-11";document.getElementById("reset-search").dispatchEvent(new Event("input"))');
    assert.equal(await evaluate('document.getElementById("reset-cards-list").children.length'), 1);
    await evaluate('document.getElementById("reset-search").value="";document.getElementById("reset-search").dispatchEvent(new Event("input"));document.getElementById("reset-tab-attempts").click()');
    assert.match(await evaluate('document.getElementById("reset-attempts-list").textContent'), /没有可重置的额度窗口/);
    assert.match(await evaluate('document.getElementById("reset-attempts-list").textContent'), /后台保留原兑换标识及账号保护/);
    assert.equal(await evaluate('document.querySelectorAll("#reset-attempts-list img,#reset-attempts-list script").length'), 0);
    await evaluate('document.getElementById("reset-attention").click()');
    assert.equal(await evaluate('document.getElementById("reset-attempts-list").children.length'), 1);
    await evaluate('document.getElementById("display-mode").value="masked";document.getElementById("display-mode").dispatchEvent(new Event("change"))');
    assert.equal(await evaluate('document.getElementById("reset-attempts-list").textContent.includes("reset-account-2")'), false);
    await evaluate('document.getElementById("display-mode").value="original";document.getElementById("display-mode").dispatchEvent(new Event("change"));document.getElementById("reset-attention").click();document.getElementById("reset-tab-cards").click()');
    resetCardsFixture = null;
    await evaluate('document.getElementById("reset-refresh").click()'); await settleRefresh();
    for (const width of [1586, 390]) {
      await cdp('Emulation.setDeviceMetricsOverride', {width, height: 992, deviceScaleFactor: 1, mobile: width < 600});
      await evaluate('scrollTo(0,0)');
      const metrics = await cdp('Page.getLayoutMetrics');
      const capture = await cdp('Page.captureScreenshot', {format: 'png', captureBeyondViewport: true, clip: {x: 0, y: 0, width, height: Math.ceil(metrics.cssContentSize.height), scale: 1}});
      await writeFile(join(output, 'scheduling-optimized-' + width + '.png'), Buffer.from(capture.data, 'base64'));
    }
    if (schedulingOnly) {
      await evaluate('document.getElementById("disconnect").click()');
      assert.equal(await evaluate('["scheduling-fields","scheduling-save","scheduling-reload","scheduling-verify","reset-refresh"].every(id=>document.getElementById(id).disabled)'), true, 'Disconnected scheduling controls were enabled');
    }
    await cdp('Emulation.setDeviceMetricsOverride', {width: 1586, height: 992, deviceScaleFactor: 1, mobile: false});
    assert.equal(posts.length, writeCount); assert.equal(patches.length, patchCount);
    assert.equal(exceptions.length, 0, JSON.stringify(exceptions)); assert.equal(pageErrors.length, 0, JSON.stringify(pageErrors));
    console.log('Scheduling Chromium checks passed: validation/focus, switch dependency, readonly records/search/pagination, observation times, translated outcomes, masking, preserved details/focus, full-width editor and 320/390/768/1586 layouts. Screenshots: ' + output);
  }
  async function runProjectMatchingChecks() {
    await evaluate('document.querySelector("[data-page=scheduling]").click()');
    await waitFor(() => evaluate('!document.getElementById("scheduling-fields").disabled && document.querySelectorAll("#routing-account-list .routing-account").length===12'), 'Project account list did not load');
    const mapFile = join(output, 'import-projects.json');
    await mkdir(output, {recursive:true});
    await writeFile(mapFile, JSON.stringify({projects:[
      {directory:'/work/project-a',project:'project-a'},
      {directory:'C:\\Work\\ProjectA',project:'project-a'},
      {directory:'/work/project-b',project:'project-b'}
    ]}));
    await cdp('DOM.enable');
    const doc = await cdp('DOM.getDocument'), file = await cdp('DOM.querySelector',{nodeId:doc.root.nodeId,selector:'#routing-map-file'});
    await cdp('DOM.setFileInputFiles',{nodeId:file.nodeId,files:[mapFile]});
    await waitFor(()=>evaluate('document.querySelectorAll("#routing-project-list .routing-project").length===2'),'Directory map import did not produce project rows');
    assert.equal(posts.length,0,'Directory import automatically saved');
    await evaluate('document.querySelector("#routing-account-list .routing-account button").click()');
    await evaluate('document.querySelectorAll("#routing-project-list .routing-project")[1].querySelector("button").click()');
    await evaluate('document.querySelectorAll("#routing-account-list .routing-account")[1].querySelector("button").click()');
    assert.equal(await evaluate('document.querySelectorAll("#routing-project-list .routing-account-chip").length'),2);
    assert.equal(await evaluate('document.querySelectorAll("#routing-account-list .routing-account")[4].querySelector("button").disabled'),true,'Disabled account could be added');
    await evaluate('document.querySelectorAll("#routing-project-list .routing-project")[0].querySelector("button").click();globalThis.directoryEditor=document.querySelector("#routing-project-list textarea");directoryEditor.value+="\\n";directoryEditor.dispatchEvent(new Event("input",{bubbles:true}));directoryEditor.focus();directoryEditor.setSelectionRange(directoryEditor.value.length,directoryEditor.value.length)');
    await evaluate('document.getElementById("routing-accounts-refresh").click()'); await settleRefresh();
    const directoryState = await evaluate('({connected:directoryEditor.isConnected,value:directoryEditor.value,focus:document.activeElement===directoryEditor,active:document.activeElement.id})');
    assert.equal(directoryState.connected && directoryState.value.endsWith('\n') && directoryState.focus,true,'Account refresh lost directory input/newline/focus: '+JSON.stringify(directoryState));
    await evaluate('document.getElementById("routing-account-next").click()');
    await waitFor(()=>evaluate('document.getElementById("routing-account-list").textContent.includes("codex-13")'),'Matching list next page did not load');
    await evaluate('document.getElementById("routing-account-previous").click()');
    await waitFor(()=>evaluate('document.getElementById("routing-account-list").textContent.includes("codex-01")'),'Matching list previous page did not load');
    await evaluate('document.getElementById("routing-account-search").value="codex-09";document.getElementById("routing-account-search").dispatchEvent(new Event("input"))');
    await waitFor(()=>evaluate('document.querySelectorAll("#routing-account-list .routing-account").length===1 && document.getElementById("routing-account-list").textContent.includes("codex-09")'),'Matching account search did not apply');
    await evaluate('document.getElementById("routing-account-search").value="";document.getElementById("routing-account-search").dispatchEvent(new Event("input"))');
    await waitFor(()=>evaluate('document.querySelectorAll("#routing-account-list .routing-account").length===12'),'Account search clear did not reload');
    await evaluate('document.getElementById("scheduling-form").requestSubmit()');
    await waitFor(()=>schedulingRevision===1,'Project map did not save');
    await waitFor(()=>evaluate('!document.getElementById("scheduling-fields").disabled'),'Project save did not acknowledge');
    assert.deepEqual(schedulingConfig.projects,[{project:'project-a',accounts:['codex-01'],directories:['/work/project-a','C:\\Work\\ProjectA']},{project:'project-b',accounts:['codex-02'],directories:['/work/project-b']}]);
    await cdp('Browser.setDownloadBehavior',{behavior:'allow',downloadPath:output});
    await evaluate('document.getElementById("routing-map-export").click()');
    let downloaded;
    await waitFor(async()=>{try{downloaded=JSON.parse(await readFile(join(output,'projects.json'),'utf8'));return true;}catch{return false;}},'Directory mapping did not download');
    assert.deepEqual(downloaded.projects,[{directory:'/work/project-a',project:'project-a'},{directory:'C:\\Work\\ProjectA',project:'project-a'},{directory:'/work/project-b',project:'project-b'}]);
    await evaluate('document.getElementById("display-mode").value="masked";document.getElementById("display-mode").dispatchEvent(new Event("change"))');
    assert.equal(await evaluate('document.getElementById("routing-account-list").textContent.includes("codex-01") || document.querySelector("#routing-project-list .routing-selected-accounts").textContent.includes("codex-01")'),false,'Matching account privacy failed');
    await evaluate('document.getElementById("display-mode").value="original";document.getElementById("display-mode").dispatchEvent(new Event("change"))');
    for(const width of [1586,768,390,320]) {
      await cdp('Emulation.setDeviceMetricsOverride',{width,height:992,deviceScaleFactor:1,mobile:width<600});
      assert.equal(await evaluate('document.documentElement.scrollWidth<=innerWidth'),true,'Project matching overflow at '+width);
      await evaluate('scrollTo(0,0)');
      const metrics=await cdp('Page.getLayoutMetrics');
      const capture=await cdp('Page.captureScreenshot',{format:'png',captureBeyondViewport:true,clip:{x:0,y:0,width,height:Math.ceil(metrics.cssContentSize.height),scale:1}});
      await writeFile(join(output,'project-matching-'+width+'.png'),Buffer.from(capture.data,'base64'));
    }
    assert.equal(posts.length,1); assert.equal(posts[0].resource,'scheduling'); assert.equal(patches.length,0);
    assert.equal(exceptions.length,0,JSON.stringify(exceptions)); assert.equal(pageErrors.length,0,JSON.stringify(pageErrors));
    console.log('Project matching Chromium checks passed: client map import/export, account selection, saved roundtrip, disabled accounts, independent pagination/search, privacy, multiline caret/focus and 320/390/768/1586 layouts. Screenshots: '+output);
  }
  const readCountFor = resource => reads.filter(path => new URL(path, 'http://fixture').pathname.endsWith('/' + resource)).length;
  async function settleRefresh() {
    await waitFor(() => inFlightRequests.size === 0, 'Automatic refresh did not finish its independent reads');
    await evaluate('document.hidden ? Promise.resolve() : new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)))');
  }
  async function naturalPolls(count = 1) {
    const before = readCountFor('overview');
    await waitFor(() => readCountFor('overview') >= before + count, 'No real timer-driven overview reads', count * 6500 + 1500);
    await settleRefresh();
  }
  async function assertForegroundPollingCadence() {
    const phases = fixtureJobs.map(job => job.phase);
    try {
      fixtureJobs.forEach(job => { job.phase = 'completed'; });
      await evaluate('window.dispatchEvent(new Event("focus"))');
      await settleRefresh();
      const idleAt = Date.now();
      await naturalPolls();
      const idleElapsed = Date.now() - idleAt;
      assert.ok(idleElapsed >= 4200 && idleElapsed <= 7500, 'Idle foreground polling lost its five-second cadence: ' + idleElapsed);
      console.log('Autorefresh native idle foreground poll after ' + idleElapsed + ' ms.');
    } finally { fixtureJobs.forEach((job, index) => { job.phase = phases[index]; }); }
    await evaluate('window.dispatchEvent(new Event("focus"))');
    await settleRefresh();
    const activeAt = Date.now();
    await naturalPolls();
    const activeElapsed = Date.now() - activeAt;
    assert.ok(activeElapsed >= 1100 && activeElapsed <= 3500, 'Active-task polling lost its 1.5-second cadence: ' + activeElapsed);
    console.log('Autorefresh native active foreground poll after ' + activeElapsed + ' ms.');
  }
  async function startMutationProbe(ids) {
    await evaluate(`(() => {
      const roots = ${JSON.stringify(ids)}.map(id => document.getElementById(id));
      const retained = roots.flatMap(root => [root, ...root.querySelectorAll('*')]);
      const stats = {attributes: 0, redundantAttributes: 0, elementChurn: 0, staticTextWrites: 0, redundantTimeWrites: 0, realTimeWrites: 0, transitions: 0, animations: 0, loadingFlashes: 0, samples: []};
      const sample = (kind, node, value) => { if (stats.samples.length < 12) stats.samples.push({kind, node: node.id || node.className || node.nodeName, value}); };
      const observer = new MutationObserver(records => {
        for (const [index, record] of records.entries()) {
          const node = record.target.nodeType === Node.TEXT_NODE ? record.target.parentElement : record.target;
          if (record.type === 'attributes') {
            const nextRecord = records.slice(index + 1).find(next => next.type === 'attributes' && next.target === record.target && next.attributeName === record.attributeName);
            const next = nextRecord ? nextRecord.oldValue : record.target.getAttribute(record.attributeName);
            if (record.attributeName === 'title' && record.oldValue !== next && node.matches('.quota-cell')) continue;
            stats.attributes++;
            if (record.oldValue === next) stats.redundantAttributes++;
            sample('attribute:' + record.attributeName, node, [record.oldValue, next]);
          } else if (record.type === 'childList' && [...record.addedNodes, ...record.removedNodes].some(child => child.nodeType === Node.ELEMENT_NODE)) {
            stats.elementChurn++; sample('elementChurn', node, record.removedNodes.length + '/' + record.addedNodes.length);
          } else {
            const before = record.type === 'characterData' ? record.oldValue : Array.from(record.removedNodes, child => child.textContent).join('');
            const normalizeCountdown = value => value.replace(/(?:\\d+\\s*(?:天|小时|分|秒)\\s*)+后计划查询/g, '后计划查询');
            const historyCountdown = node.matches('#history-list td:nth-child(5)') && before !== node.textContent && before.includes('后计划查询') && normalizeCountdown(before) === normalizeCountdown(node.textContent);
            const timed = historyCountdown || !!node.closest('[data-reset-at],[data-observed-at],[data-queue-phase]');
            if (!timed) { stats.staticTextWrites++; sample('staticText', node, [before, node.textContent]); }
            else if (before === record.target.textContent) { stats.redundantTimeWrites++; sample('sameTimeText', node, before); }
            else stats.realTimeWrites++;
          }
        }
      });
      roots.forEach(root => observer.observe(root, {subtree: true, childList: true, characterData: true, characterDataOldValue: true, attributes: true, attributeOldValue: true}));
      const onMotion = event => { if (roots.some(root => root.contains(event.target))) stats[event.type === 'transitionrun' ? 'transitions' : 'animations']++; };
      document.addEventListener('transitionrun', onMotion, true); document.addEventListener('animationstart', onMotion, true);
      const loadingObserver = new MutationObserver(records => {
        for (const record of records) {
          if (record.type === 'attributes' && record.oldValue === 'false' && record.target.getAttribute('aria-busy') === 'true') stats.loadingFlashes++;
          if (record.type === 'childList' && Array.from(record.addedNodes, node => node.textContent).join('').includes('正在读取')) stats.loadingFlashes++;
        }
      });
      for (const id of ['history-refresh', 'history-verify', 'history-message', 'history-detail-message']) loadingObserver.observe(document.getElementById(id), {subtree: true, childList: true, attributes: true, attributeFilter: ['aria-busy'], attributeOldValue: true});
      globalThis.autorefreshProbe = {stats, retained, focus: document.activeElement, stop() {
        observer.disconnect(); loadingObserver.disconnect(); document.removeEventListener('transitionrun', onMotion, true); document.removeEventListener('animationstart', onMotion, true);
        return {...stats, retained: retained.length, detached: retained.filter(node => !node.isConnected).length, focusRetained: document.activeElement === this.focus};
      }};
    })()`);
  }
  async function endMutationProbe(label) {
    const stats = await evaluate('autorefreshProbe.stop()');
    console.log('Autorefresh mutation budget [' + label + ']: ' + JSON.stringify(stats));
    return stats;
  }
  function assertMutationBudget(stats, label) {
    for (const key of ['attributes', 'elementChurn', 'staticTextWrites', 'redundantTimeWrites', 'transitions', 'animations', 'loadingFlashes', 'detached']) assert.equal(stats[key], 0, label + ' exceeded ' + key + ' budget: ' + JSON.stringify(stats));
    assert.equal(stats.focusRetained, true, label + ' stole keyboard focus');
  }
  async function runAutorefreshChecks() {
    const postCount = posts.length, patchCount = patches.length, requestStart = networkRequests.length;
    const initialDisabled = authFiles[0].disabled, initialMode = await evaluate('document.getElementById("display-mode").value');
    const checked = () => evaluate('document.querySelector("[data-account-status-id=codex-01]")?.getAttribute("aria-checked")');
    const changeExternalStatus = () => { authFiles[0].disabled = !authFiles[0].disabled; authFiles[0].status = authFiles[0].disabled ? 'disabled' : 'active'; };
    const awaitExternalStatus = () => waitFor(async () => await checked() === String(!authFiles[0].disabled), 'External CPA enable/disable stayed stale without a manual refresh', 9000);
    await evaluate('document.querySelector("[data-page=overview]").click()');
    await settleRefresh();
    await evaluate('(() => {document.querySelector("[data-account-alias-id=codex-01]").click();document.getElementById("account-alias-value").value="自动刷新主账号";document.getElementById("account-alias-save").click();const mode=document.getElementById("display-mode");mode.value="alias";mode.dispatchEvent(new Event("change"))})()');
    await naturalPolls();
    await evaluate('document.querySelector("[data-account-status-id=codex-01]").focus()');
    await startMutationProbe(['accounts', 'tasks', 'recent-events', 'diagnostics-content']);
    await naturalPolls(2);
    const overviewBudget = await endMutationProbe('same-state overview + alias, two native polls');
    await screenshot('autorefresh-same-state-alias.png');
    const stableRevision = revision, stableAccountsRevision = accountsRevision, authBefore = readCountFor('auth-files');
    changeExternalStatus();
    await awaitExternalStatus();
    assert.ok(readCountFor('auth-files') > authBefore, 'Timer-driven refresh skipped the live CPA status directory');
    assert.equal(revision, stableRevision); assert.equal(accountsRevision, stableAccountsRevision);
    assert.equal(await evaluate('document.querySelector("#accounts .account-name strong").textContent'), '自动刷新主账号', 'Polling lost the privacy-safe alias');
    await screenshot('autorefresh-external-cpa-status.png');
    console.log('Autorefresh external CPA change passed without accounts/view revision changes or manual refresh.');

    let backgroundTarget;
    try {
      backgroundTarget = (await cdp('Target.createTarget', {url: 'about:blank', background: false})).targetId;
      await cdp('Target.activateTarget', {targetId: backgroundTarget});
      await waitFor(() => evaluate('document.hidden'), 'Real Chromium tab activation did not hide the panel');
      await settleRefresh();
      const hiddenAt = Date.now(), hiddenReads = readCountFor('overview');
      await new Promise(resolve => setTimeout(resolve, 6500));
      assert.equal(readCountFor('overview'), hiddenReads, 'Hidden page kept the foreground 1.5/5 second polling rate');
      await waitFor(() => readCountFor('overview') > hiddenReads, 'Hidden page did not poll within its 30 second interval', 27500);
      console.log('Autorefresh native hidden-tab poll after ' + (Date.now() - hiddenAt) + ' ms.');
      await settleRefresh();
      const visibleReads = readCountFor('overview'), visibleAuthReads = readCountFor('auth-files');
      changeExternalStatus();
      await cdp('Target.activateTarget', {targetId: target.id});
      await waitFor(async () => !await evaluate('document.hidden') && readCountFor('overview') > visibleReads && readCountFor('auth-files') > visibleAuthReads && await checked() === String(!authFiles[0].disabled), 'Returning to a visible Chromium tab did not immediately GET fresh CPA state', 1400);
      await settleRefresh();
      await screenshot('autorefresh-native-visible-return.png');
    } finally {
      if (backgroundTarget) { await cdp('Target.activateTarget', {targetId: target.id}); await cdp('Target.closeTarget', {targetId: backgroundTarget}); }
    }

    let onlineReads;
    await cdp('Network.emulateNetworkConditions', {offline: true, latency: 0, downloadThroughput: -1, uploadThroughput: -1});
    try {
      await waitFor(() => evaluate('!navigator.onLine'), 'Chromium did not enter a real offline state');
      changeExternalStatus();
      onlineReads = readCountFor('auth-files');
    } finally {
      await cdp('Network.emulateNetworkConditions', {offline: false, latency: 0, downloadThroughput: -1, uploadThroughput: -1});
    }
    await waitFor(async () => await evaluate('navigator.onLine') && readCountFor('auth-files') > onlineReads && await checked() === String(!authFiles[0].disabled), 'Native online recovery did not immediately GET current CPA state', 1400);
    await settleRefresh();
    await screenshot('autorefresh-native-online-recovery.png');
    const focusReads = readCountFor('overview');
    await evaluate('window.dispatchEvent(new Event("focus"))');
    await waitFor(() => readCountFor('overview') > focusReads, 'Window focus did not immediately read the overview', 1200);
    await settleRefresh();

    const overviewGate = gate(), mergedBefore = readCountFor('overview');
    resourceReadGates.set('overview', overviewGate);
    try {
      await evaluate('for(let index=0;index<8;index++){window.dispatchEvent(new Event("focus"));window.dispatchEvent(new Event("online"));document.dispatchEvent(new Event("visibilitychange"));}');
      await waitFor(() => overviewGate.started, 'Resume event burst did not start GET overview', 1200);
      await new Promise(resolve => setTimeout(resolve, 200));
      assert.equal(overviewGate.arrivals, 1, 'Resume event burst overlapped overview requests');
    } finally { resourceReadGates.delete('overview'); overviewGate.release(); }
    await settleRefresh();
    await new Promise(resolve => setTimeout(resolve, 200));
    assert.ok(readCountFor('overview') - mergedBefore <= 2, 'Resume events queued more than one follow-up refresh');
    const mergedSettled = readCountFor('overview');
    await new Promise(resolve => setTimeout(resolve, 1800));
    assert.ok(readCountFor('overview') - mergedSettled <= 1, 'Resume events stacked polling timers');

    await evaluate('document.querySelector("[data-page=history]").click()');
    await settleRefresh();
    const accountsGate = gate(), historyBeforeGate = readCountFor('task-history');
    resourceReadGates.set('accounts', accountsGate); revision++; accountsRevision++;
    try {
      await waitFor(() => accountsGate.started, 'Independent account read did not start');
      await waitFor(() => readCountFor('task-history') > historyBeforeGate, 'Held accounts GET blocked the independent history GET', 1200);
      assert.equal(await evaluate('document.getElementById("auto-status").dataset.state'), 'online', 'A held resource misreported the entire connection');
    } finally { resourceReadGates.delete('accounts'); accountsGate.release(); }
    await settleRefresh();
    unavailableResources.add('accounts'); revision++; accountsRevision++;
    try {
      const historyBeforeFailure = readCountFor('task-history'), accountsBeforeFailure = readCountFor('accounts');
      await naturalPolls();
      assert.ok(readCountFor('accounts') > accountsBeforeFailure);
      assert.ok(readCountFor('task-history') > historyBeforeFailure, 'Failed accounts GET blocked history refresh');
      assert.notEqual(await evaluate('document.getElementById("auto-status").dataset.state'), 'offline', 'One failed resource marked the healthy overview connection offline');
      assert.equal(await evaluate('document.getElementById("login-section").hidden && !document.getElementById("refresh").disabled'), true, 'One failed resource disconnected the authenticated panel');
    } finally { unavailableResources.delete('accounts'); }
    await naturalPolls();
    await evaluate('document.querySelector(\'#history-list [data-render-key="wake:persisted-wake-0"] button\').click()');
    await settleRefresh();
    await evaluate('globalThis.autorefreshDetails=document.querySelector("#history-detail-body [data-render-key=wake-response]");autorefreshDetails.open=true;autorefreshDetails.querySelector("summary").focus()');
    await startMutationProbe(['history-list', 'history-detail-body']);
    await naturalPolls(2);
    const historyBudget = await endMutationProbe('background history + expanded response');
    assert.equal(await evaluate('autorefreshDetails.isConnected && autorefreshDetails.open'), true, 'Background history collapsed the saved response');
    await screenshot('autorefresh-history-no-loading-flash.png');
    await evaluate('document.getElementById("history-detail-close").click();document.querySelector("[data-page=query]").click()');
    const queryDraftBefore = await evaluate('document.getElementById("settings-excluded").value');
    await evaluate('(() => {const field=document.getElementById("settings-excluded");field.value="02:00-03:00";field.dispatchEvent(new Event("input"));field.focus();field.setSelectionRange(1,4)})()');
    await naturalPolls(2);
    assert.deepEqual(await evaluate('[document.activeElement.id,document.getElementById("settings-excluded").value,document.getElementById("settings-excluded").selectionStart,document.getElementById("settings-excluded").selectionEnd]'), ['settings-excluded', '02:00-03:00', 1, 4], 'Native polling changed the query draft/focus/selection');
    await evaluate(`(() => {const restoredQuery=document.getElementById('settings-excluded');restoredQuery.value=${JSON.stringify(queryDraftBefore)};restoredQuery.dispatchEvent(new Event('input'));document.querySelector('[data-page=models]').click()})()`);
    await settleRefresh();
    const modelDraftBefore = await evaluate('document.getElementById("model-default-prompt").value');
    await evaluate('(() => {const field=document.getElementById("model-default-prompt");field.value="自动刷新保留的模型草稿";field.dispatchEvent(new Event("input"));field.focus();field.setSelectionRange(2,5)})()');
    const modelGate = gate(), quotaBefore = accounts[0].quota.five_hour.Remaining.Value;
    resourceReadGates.set('model-settings', modelGate);
    accounts[0].quota.five_hour.Remaining.Value = 61; accountsRevision++; revision++;
    try {
      await waitFor(() => modelGate.started, 'Background model settings did not start');
      await waitFor(() => evaluate('document.querySelector("#accounts progress").value===61'), 'Held model settings blocked fresh account quota', 1200);
      assert.deepEqual(await evaluate('[document.activeElement.id,document.getElementById("model-default-prompt").value,document.getElementById("model-default-prompt").selectionStart,document.getElementById("model-default-prompt").selectionEnd]'), ['model-default-prompt', '自动刷新保留的模型草稿', 2, 5], 'Independent polling lost the model draft/focus/selection');
    } finally { resourceReadGates.delete('model-settings'); modelGate.release(); }
    await settleRefresh();
    accounts[0].quota.five_hour.Remaining.Value = quotaBefore; accountsRevision++; revision++;
    await evaluate(`(() => {const restoredModel=document.getElementById('model-default-prompt');restoredModel.value=${JSON.stringify(modelDraftBefore)};restoredModel.dispatchEvent(new Event('input'));document.querySelector('[data-page=overview]').click()})()`);
    if (authFiles[0].disabled !== initialDisabled) changeExternalStatus();
    await awaitExternalStatus();
    await evaluate(`(() => {document.querySelector('[data-account-alias-id=codex-01]').click();document.getElementById('account-alias-value').value='';document.getElementById('account-alias-save').click();const restoredMode=document.getElementById('display-mode');restoredMode.value=${JSON.stringify(initialMode)};restoredMode.dispatchEvent(new Event('change'))})()`);
    await settleRefresh();
    assert.equal(posts.length, postCount, 'Automatic refresh dispatched a plugin POST/model request');
    assert.equal(patches.length, patchCount, 'Automatic refresh PATCHed CPA status');
    assert.equal(networkRequests.slice(requestStart).every(request => request.method === 'GET'), true, 'Automatic refresh issued a non-GET request');
    assert.equal(networkRequests.slice(requestStart).some(request => new URL(request.url).pathname.endsWith('/auth-files/models')), false, 'Automatic refresh fetched the opt-in model catalog');
    assertMutationBudget(overviewBudget, 'Same-state overview + alias');
    assertMutationBudget(historyBudget, 'Background history');
    console.log('Autorefresh regression passed: real timer CPA changes, native hidden/visible and offline/online recovery, focus, merged resume reads, independent held/failed resources, zero same-value DOM churn/loading flashes, preserved alias/details/drafts/focus/selection and zero POST/PATCH/model traffic.');
  }
  await cdp('Page.enable'); await cdp('Runtime.enable'); await cdp('Log.enable'); await cdp('Network.enable');
  await cdp('Page.addScriptToEvaluateOnNewDocument', {source: `localStorage.setItem('cli-proxy-auth', JSON.stringify({state:{managementKey:${JSON.stringify(fixtureKey)},apiBase:location.origin+(location.pathname.startsWith('/other-cpa/')?'/other-cpa':'/cpa')}}));`});
  await cdp('Emulation.setDeviceMetricsOverride', {width: 1586, height: 992, deviceScaleFactor: 1, mobile: false});
  await cdp('Page.navigate', {url: origin + base + 'status'});
  await waitFor(() => evaluate('document.getElementById("login-section")?.hidden && document.getElementById("accounts")?.rows.length === 6'), 'Dashboard did not auto-connect');
  await evaluate('document.fonts.ready.then(() => true)');
  checks: {
  if (projectsOnly) { await runProjectMatchingChecks(); break checks; }
  if (schedulingOnly) { await runSchedulingChecks(); break checks; }
  if (autorefreshOnly) {
    await evaluate('document.querySelector("[data-account-status-id=codex-01]").click()');
    await waitFor(() => evaluate('document.querySelector("[data-account-status-id=codex-01]").dataset.state==="confirmed"'), 'Explicit fixture-only setup PATCH did not reach confirmed');
    assert.equal(patches.length, 1, 'Fixture setup must PATCH exactly once after explicit confirmation');
    await assertForegroundPollingCadence();
    await runAutorefreshChecks();
    assert.equal(posts.length, 0, 'Read-only Chromium suite submitted plugin/model work');
    assert.equal(patches.length, 1, 'Read-only Chromium suite added an unconfirmed CPA PATCH');
    await evaluate('document.getElementById("disconnect").click()');
    await settleRefresh();
    const afterDisconnect = networkRequests.length;
    await evaluate('window.dispatchEvent(new Event("online"));window.dispatchEvent(new Event("focus"));document.dispatchEvent(new Event("visibilitychange"))');
    await new Promise(resolve => setTimeout(resolve, 1800));
    assert.equal(networkRequests.length, afterDisconnect, 'Resume events restored credentials after explicit disconnect');
    assert.equal(exceptions.length, 0, JSON.stringify(exceptions)); assert.equal(pageErrors.length, 0, JSON.stringify(pageErrors));
    console.log('Autorefresh-only Chromium checks passed; screenshots: ' + output);
    break checks;
  }
  assert.equal(await evaluate('document.getElementById("metric-total").textContent'), '24');
  assert.equal(await evaluate('document.getElementById("metric-capacity").textContent'), '16 / 20');
  assert.equal(posts.length, 0, 'Opening the page dispatched a write');
  assert.equal(await evaluate('document.querySelector("progress").value'), 82);
  assert.equal(await evaluate('document.getElementById("auto-wake").disabled'), true);
  assert.equal(await evaluate('document.getElementById("five-hour-query").disabled'), false);
  assert.match(await evaluate('document.getElementById("accounts").rows[0].textContent'), /等待查询 · 手动批量.*后计划查询/);
  assert.equal(await evaluate('document.getElementById("accounts").rows[0].querySelector(".account-query").textContent'), '立即查询');
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
  for (const page of ['accounts', 'query', 'wake', 'scheduling', 'models', 'history', 'diagnostics', 'overview']) {
    await evaluate(`document.querySelector('[data-page="${page}"]').click()`);
    assert.equal(await evaluate(`document.querySelector('[data-view="${page}"]').hidden`), false, 'Navigation failed: ' + page);
  }
  assert.equal(posts.length, 0, 'Navigating dispatched a write');
  assert.equal(reads.some(path => path.endsWith('/settings')), false, 'Overview config should avoid redundant settings reads');
  await evaluate('document.querySelector("[data-page=query]").click();const queryInput=document.getElementById("settings-excluded");queryInput.focus();queryInput.value="01:00-02:00";queryInput.dispatchEvent(new Event("input"));queryInput.setSelectionRange(1,4);document.getElementById("refresh").onclick()');
  assert.deepEqual(await evaluate('[document.activeElement.id,document.getElementById("settings-excluded").value,document.getElementById("settings-excluded").selectionStart,document.getElementById("settings-excluded").selectionEnd]'), ['settings-excluded', '01:00-02:00', 1, 4], 'Query settings polling changed the draft or cursor');
  await evaluate('document.getElementById("settings-excluded").value="";document.getElementById("settings-excluded").dispatchEvent(new Event("input"))');
  await evaluate('document.querySelector("[data-page=overview]").click()');
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
  assert.equal(await evaluate('document.getElementById("settings-message").closest("#toast-region") !== null'), true, 'Editor save must use a global toast');
  assert.equal(await evaluate('document.getElementById("settings-message").parentElement.dataset.tone'), 'success');
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
  await evaluate('document.getElementById("accounts").querySelector(".account-query").click()');
  await waitFor(() => posts.some(post => post.resource === 'jobs'), 'Single account query not dispatched');
  assert.deepEqual(JSON.parse(posts.find(post => post.resource === 'jobs').body), {kind: 'quota_query', account_id: 'codex-01'});
  assert.ok(posts.find(post => post.resource === 'jobs').headers['idempotency-key']);
  await waitFor(() => evaluate('!document.getElementById("accounts").querySelector(".account-query").disabled'), 'Single query did not restore its button');
  await waitFor(() => evaluate('!document.getElementById("accounts").rows[0].querySelector(".query-pending")'), 'Single query did not promote queued account');
  await evaluate('document.getElementById("query").click()');
  await waitFor(() => posts.filter(post => post.resource === 'jobs').length === 2, 'Batch query not dispatched');
  assert.deepEqual(JSON.parse(posts.filter(post => post.resource === 'jobs')[1].body), {kind: 'quota_query', account_id: ''});
  await waitFor(() => evaluate('document.getElementById("accounts").rows[0].textContent.includes("处于排除时段")'), 'Queue-only revision did not reload account pending state');
  assert.equal(await evaluate('document.getElementById("accounts").rows[0].querySelector(".account-query").textContent'), '立即查询');
  await evaluate('document.querySelector("[data-page=history]").click()');
  await waitFor(() => evaluate(`!!document.querySelector('#history-list [data-render-key="query:fixture-bulk"] button')`), 'Persisted query history did not load');
  assert.match(await evaluate('document.getElementById("history-list").textContent'), /处于排除时段/);
  assert.match(await evaluate('document.getElementById("history-list").textContent'), /手动批量/);
  await evaluate(`document.querySelector('#history-list [data-render-key="query:fixture-bulk"] button').click()`);
  await waitFor(async () => {
    const detail = await evaluate('({loaded:!!document.querySelector("#history-detail-body .task-results"),message:document.getElementById("history-detail-message").textContent})');
    assert.doesNotMatch(detail.message, /详情读取失败/, detail.message + ': ' + JSON.stringify(jobs.get('fixture-bulk')));
    return detail.loaded;
  }, async () => 'Query history details did not load: ' + JSON.stringify({page: await evaluate('({message:document.getElementById("history-detail-message").textContent,body:document.getElementById("history-detail-body").textContent,selected:document.querySelector("#history-list [data-selected=true]")?.dataset.renderKey})'), jobs: reads.filter(path => path.includes('/jobs?')), pageErrors}));
  await evaluate('globalThis.historyDetails=document.querySelector("#history-detail-body .task-results");historyDetails.open=true;globalThis.historySummary=historyDetails.querySelector("summary");historySummary.focus();document.getElementById("refresh").onclick()');
  await waitFor(() => evaluate('!document.getElementById("sync").disabled'), 'History refresh did not finish');
  assert.equal(await evaluate('historyDetails.isConnected && historyDetails.open && document.activeElement===historySummary'), true, 'Query history polling reset expansion or focus');
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
  await evaluate('globalThis.accountQueryButton=document.getElementById("accounts").rows[0].querySelector(".account-query");accountQueryButton.focus()');
  const accountReadsBeforeFocus = reads.filter(path => path.includes('/accounts?')).length;
  accounts[0].quota.weekly.Remaining.Value = 66; revision++; accountsRevision++;
  await waitFor(() => reads.filter(path => path.includes('/accounts?')).length > accountReadsBeforeFocus, 'Account focus test did not receive background data');
  await waitFor(() => evaluate('document.getElementById("accounts").rows[0].cells[3].textContent.includes("66%")'), 'Updated quota did not render in reused account row');
  assert.equal(await evaluate('accountQueryButton.isConnected && document.activeElement===accountQueryButton'), true, 'Account refresh recreated the focused query button');
  const writesBeforeStatus = posts.length;
  accounts[0].status = 'disabled'; revision++; accountsRevision++;
  await waitFor(() => evaluate('document.getElementById("accounts").rows[0].querySelector(".status-badge").textContent==="手动禁用"'), 'Periodic host state did not update disabled badge');
  accounts[0].status = 'available'; accounts[0].quota.five_hour.Remaining.Value = 0; revision++; accountsRevision++;
  await waitFor(() => evaluate('document.getElementById("accounts").rows[0].querySelector(".status-badge").textContent==="额度耗尽"'), 'Quota evidence did not update exhausted badge');
  await evaluate('document.getElementById("accounts-filter").value="quota_exhausted";document.getElementById("accounts-filter").dispatchEvent(new Event("change"))');
  await waitFor(() => evaluate('document.getElementById("accounts").rows.length===1 && document.getElementById("accounts").rows[0].textContent.includes("codex-01")'), 'Exhausted status filter did not select the depleted account');
  accounts[0].quota.five_hour.Remaining.Value = 11; revision++; accountsRevision++;
  await waitFor(() => evaluate('document.getElementById("accounts").textContent.includes("没有符合条件的账号")'), 'Recovered quota remained in exhausted filter');
  await evaluate('document.getElementById("accounts-filter").value="all";document.getElementById("accounts-filter").dispatchEvent(new Event("change"))');
  await waitFor(() => evaluate('document.getElementById("accounts").rows[0].querySelector(".status-badge")?.textContent==="正常"'), 'Recovered quota did not restore normal badge');
  assert.equal(posts.length, writesBeforeStatus, 'Status polling issued an upstream query or model write');
  assert.equal(reads.filter(path => path.endsWith('/generation-accounts')).length, 1, 'Overview or quota actions refreshed the generation directory');
  await evaluate('document.querySelector("[data-page=models]").click()');
  await waitFor(() => evaluate('!document.getElementById("model-fields").disabled'), 'Model settings did not load');
  const beforeCatalog = posts.length;
  await evaluate('document.getElementById("model-catalog-account").value="codex-01";document.getElementById("model-catalog-account").dispatchEvent(new Event("input"))');
  await clickWhenEnabled('model-catalog-refresh');
  await waitFor(() => evaluate('document.getElementById("wake-model-options").options.length===2'), 'CPA registered model suggestions did not load');
  assert.equal(posts.length, beforeCatalog, 'Reading model catalog sent a write');
  assert.equal(await evaluate('document.getElementById("model-default").value'), '', 'Catalog automatically picked a wake model');
  modelReadGate = gate();
  await evaluate('const prompt=document.getElementById("model-default-prompt");prompt.focus();prompt.setSelectionRange(2,5);document.getElementById("refresh").click()');
  await waitFor(() => modelReadGate.started, 'Model background read did not start');
  assert.deepEqual(await evaluate('[document.activeElement.id,document.getElementById("model-default-prompt").selectionStart,document.getElementById("model-default-prompt").selectionEnd,document.getElementById("model-fields").disabled]'), ['model-default-prompt', 2, 5, false], 'Background model poll stole focus or cursor');
  modelReadGate.release(); modelReadGate = undefined;
  await waitFor(() => evaluate('!document.getElementById("sync").disabled'), 'Model poll did not finish');
  assert.deepEqual(await evaluate('[document.activeElement.id,document.getElementById("model-default-prompt").selectionStart,document.getElementById("model-default-prompt").selectionEnd]'), ['model-default-prompt', 2, 5], 'Completed unchanged model poll reset the cursor');
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
  await evaluate('globalThis.firstCandidate=document.querySelector("#generation-candidates input");firstCandidate.focus();firstCandidate.click()');
  assert.equal(await evaluate('firstCandidate.isConnected && document.activeElement===firstCandidate && firstCandidate.checked'), true, 'Selecting a candidate recreated its checkbox');
  await evaluate('document.getElementById("refresh").onclick()');
  assert.equal(await evaluate('firstCandidate.isConnected && document.activeElement===firstCandidate && firstCandidate.checked'), true, 'Polling lost candidate checkbox focus or selection');
  await cdp('Input.dispatchKeyEvent', {type: 'keyDown', key: ' ', code: 'Space', windowsVirtualKeyCode: 32});
  await cdp('Input.dispatchKeyEvent', {type: 'keyUp', key: ' ', code: 'Space', windowsVirtualKeyCode: 32});
  assert.equal(await evaluate('!firstCandidate.checked && document.getElementById("generation-apply").disabled'), true, 'Reused candidate checkbox no longer responds to keyboard toggles');
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
  wakeReadGate = gate();
  await evaluate('const input=document.getElementById("wake-per-user");input.focus();document.getElementById("refresh").click()');
  await waitFor(() => wakeReadGate.started, 'Wake background read did not start');
  assert.deepEqual(await evaluate('[document.activeElement.id,document.getElementById("wake-settings-fields").disabled]'), ['wake-per-user', false], 'Wake polling disabled the focused input');
  wakeReadGate.release(); wakeReadGate = undefined;
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
  await evaluate('document.getElementById("wake-overview-consent").click()');
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
  await evaluate('document.querySelector("[data-page=overview]").click()');
  modelReadGate = gate();
  await evaluate('document.querySelector("#accounts .account-wake").click()');
  await waitFor(() => modelReadGate.started, 'Immediate wake did not verify the saved model');
  assert.equal(await evaluate('document.querySelector("#accounts .account-wake").disabled'), true, 'Model verification did not lock duplicate wake clicks');
  await evaluate('document.querySelector("#accounts .account-wake").click()');
  assert.equal(posts.filter(post => post.resource === 'wake-jobs').length, 0, 'Wake bypassed saved model verification');
  assert.equal(await evaluate('!!document.querySelector("dialog[open]")'), false, 'Immediate wake opened a confirmation modal');
  modelReadGate.release(); modelReadGate = undefined;
  await waitFor(() => evaluate('document.getElementById("wake-manual-status").textContent==="结果未知"'), async () => 'Manual wake unknown outcome was not read: ' + JSON.stringify({
    state: await evaluate('({status:document.getElementById("wake-manual-status").textContent,message:document.getElementById("wake-manual-message").textContent,history:document.getElementById("wake-history-message").textContent,toasts:document.getElementById("toast-region").textContent,verifyDisabled:document.getElementById("wake-manual-verify").disabled})'),
    jobs: [...wakeJobs.values()].map(job => ({job_id: job.job_id, reason: job.reason, phase: job.phase})), writes: posts.filter(post => post.resource === 'wake-jobs').length,
    reads: reads.filter(path => path.includes('/wake-jobs?')).slice(-12), pageErrors, exceptions}));
  assert.equal(posts.filter(post => post.resource === 'wake-jobs').length, 1, 'Manual wake submitted more than once');
  assert.deepEqual(JSON.parse(posts.find(post => post.resource === 'wake-jobs').body), {account_id: 'codex-01'});
  assert.equal(await evaluate('!document.querySelector("dialog[open]") && !document.querySelector("[data-view=overview]").hidden'), true, 'Quick wake changed the overview page');
  await evaluate('document.querySelector("[data-page=wake]").click()');
  assert.equal(await evaluate('document.getElementById("wake-manual-fields").disabled'), true);
  await clickWhenEnabled('wake-history-refresh'); await clickWhenEnabled('wake-manual-verify');
  assert.equal(posts.filter(post => post.resource === 'wake-jobs').length, 1, 'Unknown wake verification resent the model action');
  assert.match(await evaluate('document.getElementById("wake-history").textContent'), /禁止重发/);
  assert.match(await evaluate('document.getElementById("wake-history").textContent'), /响应与执行记录/);
  assert.match(await evaluate('document.getElementById("wake-history").textContent'), /rate_limit_exceeded/);
  await evaluate('globalThis.wakeResponse=document.querySelector("#wake-history [data-render-key=wake-response]");wakeResponse.open=true;globalThis.wakeResponseSummary=wakeResponse.querySelector("summary");wakeResponseSummary.focus()');
  await clickWhenEnabled('wake-history-refresh');
  assert.equal(await evaluate('wakeResponse.isConnected && wakeResponse.open && document.activeElement===wakeResponseSummary'), true, 'Wake response polling reset the expanded record');
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
    await evaluate('document.querySelector("[data-page=scheduling]").click()');
    await waitFor(() => evaluate('!document.getElementById("scheduling-fields").disabled'), 'Scheduling page did not load');
    assert.equal(await evaluate('document.getElementById("reset-cards-list").textContent.includes("2 张，详情尚未读取")'), true, 'Quota reset summary was not shown');
    assert.equal(await evaluate('document.getElementById("routing-subagent").checked'), true, 'Old settings did not default to child affinity');
    assert.equal(await evaluate('document.getElementById("routing-rebind").value'), 'quota', 'Old settings did not preserve temporary failover');
    assert.equal(await evaluate('document.documentElement.scrollWidth <= innerWidth'), true, 'Scheduling layout overflow');
    await screenshot('scheduling-' + width + '.png');
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
  await evaluate(`(() => {
    document.querySelector('[data-page=scheduling]').click();
    document.getElementById('routing-subagent').checked = false;
    document.getElementById('routing-rebind').value = 'any';
    document.getElementById('scheduling-form').dispatchEvent(new Event('input', {bubbles:true}));
    document.getElementById('scheduling-form').requestSubmit();
  })()`);
  await waitFor(() => schedulingRevision === 1, 'Sticky settings did not save');
  await waitFor(() => evaluate('!document.getElementById("scheduling-fields").disabled'), 'Sticky settings did not reload');
  assert.equal(schedulingConfig.subagent_affinity, false); assert.equal(schedulingConfig.rebind_on, 'any');
  assert.equal(await evaluate('document.getElementById("routing-subagent").checked'), false, 'Child affinity did not persist');
  assert.equal(await evaluate('document.getElementById("routing-rebind").value'), 'any', 'Rebind choice did not persist');
  await runSchedulingChecks();
  await cdp('Emulation.setDeviceMetricsOverride', {width: 1586, height: 992, deviceScaleFactor: 1, mobile: false});
  for (const width of [1586, 390]) {
    await cdp('Emulation.setDeviceMetricsOverride', {width, height: 992, deviceScaleFactor: 1, mobile: width < 600});
    const writesBeforeToast = posts.length;
    await evaluate(`(() => {
      document.querySelector('[data-page=query]').click();
      const input = document.getElementById('account'); input.value = ''; input.focus();
      document.getElementById('query-single').onclick();
    })()`);
    const toast = await evaluate(`(() => {
      const content = document.getElementById('error'), card = content.closest('.toast'), box = card.getBoundingClientRect();
      return {visible: !content.hidden && !card.hidden, tone: card.dataset.tone, role: content.getAttribute('role'),
        inRegion: !!card.closest('#toast-region'), focus: document.activeElement.id,
        fits: box.left >= 0 && box.right <= innerWidth && box.top >= 0 && box.bottom <= innerHeight};
    })()`);
    assert.deepEqual(toast, {visible: true, tone: 'error', role: 'alert', inRegion: true, focus: 'account', fits: true});
    assert.equal(posts.length, writesBeforeToast, 'Displaying a validation toast wrote to the API');
    await screenshot('toast-error-' + width + '.png');
    await evaluate('document.getElementById("error").parentElement.querySelector(".toast-close").click()');
    assert.equal(await evaluate('document.getElementById("error").hidden'), true, 'Toast close did not dismiss');
    await evaluate('document.getElementById("refresh").click()');
    await waitFor(() => evaluate('document.getElementById("error").textContent === ""'), 'Successful refresh did not clear the toast error state');
  }
  await cdp('Emulation.setDeviceMetricsOverride', {width: 1586, height: 992, deviceScaleFactor: 1, mobile: false});
  const historyReads = () => reads.filter(path => path.includes('/task-history?'));
  const latestHistory = () => new URL(historyReads().at(-1), origin);
  async function historyFilter(id, value, event = 'change') {
    const before = historyReads().length;
    const parameter = {'history-search': 'search', 'history-kind': 'type', 'history-source': 'source', 'history-trigger': 'trigger', 'history-page-size': 'limit'}[id];
    await evaluate(`(() => {const input=document.getElementById(${JSON.stringify(id)});input.value=${JSON.stringify(value)};input.dispatchEvent(new Event(${JSON.stringify(event)}))})()`);
    await waitFor(async () => {
      if (historyReads().length <= before || parameter && (latestHistory().searchParams.get(parameter) || '') !== value) return false;
      const expected = taskHistory(latestHistory().searchParams).records.map(record => record.type + ':' + record.job_id);
      return await evaluate('document.getElementById("history-refresh").getAttribute("aria-busy")==="false" && ' +
        'JSON.stringify(Array.from(document.querySelectorAll("#history-list tr[data-render-key]"), row => row.dataset.renderKey)) === ' + JSON.stringify(JSON.stringify(expected)));
    }, 'Task history filter did not render its matching response: ' + id + '=' + value);
  }
  async function assertHistoryData() {
    const expected = taskHistory(latestHistory().searchParams);
    assert.deepEqual(await evaluate('Array.from(document.querySelectorAll("#history-list tr[data-render-key]")).map(row=>row.dataset.renderKey)'), expected.records.map(record => record.type + ':' + record.job_id));
    assert.deepEqual(await evaluate('["today","active","completed","attention"].map(name=>document.getElementById("history-"+name).textContent)'),
      [expected.summary.today, expected.summary.running + expected.summary.queued, expected.summary.completed, expected.summary.failed + expected.summary.unknown].map(String));
    assert.equal(await evaluate('document.querySelector("[data-history-count=all]").textContent'), String(expected.status_counts.all));
    for (const status of ['running', 'queued', 'completed', 'failed', 'unknown', 'skipped', 'cancelled']) {
      assert.equal(await evaluate(`document.querySelector('[data-history-count="${status}"]').textContent`), String(expected.status_counts[status]), 'Incorrect status tab count: ' + status);
    }
    assert.equal(await evaluate('document.getElementById("history-count").textContent'), expected.total + ' 条');
    return expected;
  }
  async function historyStatus(status) {
    const before = historyReads().length;
    await evaluate(`document.querySelector('[data-history-status="${status}"]').click()`);
    await waitFor(async () => historyReads().length > before && (latestHistory().searchParams.get('status') || '') === status && await evaluate('document.getElementById("history-refresh").getAttribute("aria-busy")==="false"'), 'Status tab did not finish: ' + status);
    return assertHistoryData();
  }
  async function assertUnknownSafety(inViewport = false) {
    const state = await evaluate(`(() => {
      const warning=document.getElementById('history-detail-safety'), body=document.getElementById('history-detail-body'), box=warning.getBoundingClientRect();
      return {visible:!warning.hidden && box.width>0 && box.height>0, outside:!body.contains(warning), text:warning.textContent, fits:box.top>=0 && box.bottom<=innerHeight};
    })()`);
    assert.equal(state.visible && state.outside, true, 'Unknown warning must remain outside the scrollable detail body');
    assert.match(state.text, /禁止.*(?:重复|重发)|不会.*重发/);
    if (inViewport) assert.equal(state.fits, true, 'Unknown warning left the viewport while reading a long response');
  }
  const writesBeforeHistory = posts.length;
  await evaluate('document.querySelector("[data-page=history]").click()');
  await waitFor(() => evaluate('document.getElementById("history-refresh").getAttribute("aria-busy")==="false"'), 'Persisted history did not finish loading');
  assert.ok((await assertHistoryData()).total > 20, 'History fixture does not exercise real pagination');
  assert.deepEqual(await evaluate('Array.from(document.getElementById("history-trigger").options).map(option=>option.value).sort()'),
    ['', 'manual_single', 'manual_batch', 'periodic', 'five_hour_expiry', 'weekly_expiry', 'initial_sync', 'wake_preflight', 'wake_verification', 'manual', 'five_hour_reset', 'weekly_reset', 'daily'].sort(), 'Trigger menu differs from the real backend contract');
  for (const status of ['skipped', 'cancelled']) {
    const result = await historyStatus(status);
    assert.ok(result.records.length > 0, 'Missing fixture for ' + status);
    assert.equal(result.records.every(record => record.status === status), true);
  }
  await historyStatus('');
  await historyFilter('history-kind', 'query');
  await historyFilter('history-search', 'persisted-query-00', 'input');
  assert.equal((await assertHistoryData()).records[0].finished_at, jobs.get('persisted-query-00').updated_at, 'Terminal query fixture omitted its actual finish time');
  assert.equal(await evaluate('document.querySelector("#history-list tr[data-render-key]").cells[5].textContent'), '2.5 秒', 'Completed query duration did not use finished_at');
  await evaluate('document.querySelector("#history-list tr[data-render-key] button").click()');
  await waitFor(() => evaluate('document.getElementById("history-verify").getAttribute("aria-busy")==="false"'), 'Completed query detail did not load');
  assert.match(await evaluate('document.getElementById("history-detail-body").textContent'), /2\.5 秒/, 'Query detail without timestamp fields discarded the list finish time');
  assert.equal(await evaluate('document.getElementById("history-detail-safety").hidden'), true, 'Completed query incorrectly showed unknown spend warning');
  await clickWhenEnabled('history-detail-close');
  await historyFilter('history-search', '', 'input');
  await historyFilter('history-kind', '');
  await historyFilter('history-page-size', '20');
  assert.equal((await assertHistoryData()).records.length, 20);
  const firstHistoryPage = await evaluate('Array.from(document.querySelectorAll("#history-list tr")).map(row=>row.dataset.renderKey)');
  await clickWhenEnabled('history-next');
  await waitFor(async () => latestHistory().searchParams.get('page') === '2' && await evaluate('document.getElementById("history-refresh").getAttribute("aria-busy")==="false"'), 'History next page did not request page two');
  const secondHistoryPage = (await assertHistoryData()).records.map(record => record.type + ':' + record.job_id);
  assert.equal(secondHistoryPage.some(key => firstHistoryPage.includes(key)), false, 'History pages repeat the same records');
  assert.ok(secondHistoryPage.some(key => key.startsWith('query:persisted-query-')), 'History omitted automatic tasks from before this browser session');
  await clickWhenEnabled('history-previous');
  await waitFor(async () => latestHistory().searchParams.get('page') === '1' && await evaluate('document.getElementById("history-refresh").getAttribute("aria-busy")==="false"'), 'History previous page did not return to page one');
  assert.deepEqual((await assertHistoryData()).records.map(record => record.type + ':' + record.job_id), firstHistoryPage);
  await historyFilter('history-page-size', '10');
  await historyFilter('history-source', 'manual');
  assert.equal((await assertHistoryData()).records.every(record => record.source === 'manual'), true);
  await historyFilter('history-kind', 'query');
  await historyFilter('history-trigger', 'manual_batch');
  assert.equal((await assertHistoryData()).records.every(record => record.type === 'query' && record.trigger === 'manual_batch'), true);
  await historyFilter('history-trigger', '');
  await historyFilter('history-kind', '');
  await historyFilter('history-source', 'automatic');
  assert.equal((await assertHistoryData()).records.every(record => record.source === 'automatic'), true);
  assert.equal(latestHistory().searchParams.get('page'), '1');
  await historyFilter('history-kind', 'wake');
  assert.equal((await assertHistoryData()).records.every(record => record.type === 'wake'), true);
  await historyFilter('history-trigger', 'daily');
  assert.ok((await assertHistoryData()).total > 0, 'Actual daily wake fixture was not filterable');
  assert.equal((await assertHistoryData()).records.every(record => record.trigger === 'daily'), true);
  await historyFilter('history-trigger', '');
  assert.match(await evaluate('document.getElementById("history-list").textContent'), /窗口启动已核实/);
  assert.doesNotMatch(await evaluate(`document.querySelector('#history-list [data-render-key="wake:persisted-wake-1"]').textContent`), /请求失败/);
  const automaticWakeTotal = (await assertHistoryData()).status_counts.all;
  await evaluate('document.querySelector("[data-history-status=unknown]").click()');
  await waitFor(async () => latestHistory().searchParams.get('status') === 'unknown' && await evaluate('document.getElementById("history-refresh").getAttribute("aria-busy")==="false"'), 'Unknown status filter was not sent to the server');
  assert.equal((await assertHistoryData()).records.every(record => record.status === 'unknown'), true);
  assert.equal((await assertHistoryData()).status_counts.all, automaticWakeTotal, 'Status tab incorrectly narrowed server summary counts');
  await historyFilter('history-trigger', 'weekly_reset');
  await historyFilter('history-date', 'week');
  await historyFilter('history-search', 'persisted-wake-account-0', 'input');
  assert.equal((await assertHistoryData()).total, 1);
  assert.equal(latestHistory().searchParams.get('trigger'), 'weekly_reset');
  assert.equal(latestHistory().searchParams.get('search'), 'persisted-wake-account-0');
  assert.ok(latestHistory().searchParams.has('from'));
  assert.ok(latestHistory().searchParams.has('today_from')); assert.ok(latestHistory().searchParams.has('today_to'));
  await evaluate(`document.querySelector('#history-list [data-render-key="wake:persisted-wake-0"] button').click()`);
  await waitFor(() => evaluate('!!document.querySelector("#history-detail-body [data-render-key=wake-response]")'), 'Persisted unknown wake response did not load');
  await assertUnknownSafety();
  assert.match(await evaluate('document.getElementById("history-detail-body").textContent'), /rate_limit_exceeded/);
  assert.match(await evaluate('document.getElementById("history-detail-body").textContent'), /\[已隐藏\]/);
  assert.match(await evaluate('document.getElementById("history-detail-body").textContent'), /<img src=x onerror=alert\(1\)>/);
  assert.equal(await evaluate('document.querySelectorAll("#history-detail-body img,#history-detail-body script").length'), 0, 'Response text became executable HTML');
  await evaluate('globalThis.taskResponse=document.querySelector("#history-detail-body [data-render-key=wake-response]");taskResponse.open=true;globalThis.taskResponseSummary=taskResponse.querySelector("summary");taskResponseSummary.focus()');
  const detailReads = reads.filter(path => path.includes('/wake-jobs?job_id=persisted-wake-0')).length;
  const writesBeforeVerify = posts.length;
  await clickWhenEnabled('history-verify');
  await waitFor(async () => reads.filter(path => path.includes('/wake-jobs?job_id=persisted-wake-0')).length > detailReads && await evaluate('document.getElementById("history-verify").getAttribute("aria-busy")==="false"'), 'Unknown wake verification did not read the original task');
  assert.equal(await evaluate('taskResponse.isConnected && taskResponse.open && document.activeElement===taskResponseSummary'), true, 'Read-only verification replaced response expansion or focus');
  assert.equal(posts.length, writesBeforeVerify, 'Unknown verification submitted work');
  await assertUnknownSafety();
  await evaluate(`Object.defineProperty(navigator, 'clipboard', {configurable:true, value:{writeText:async()=>{throw new DOMException('Fixture clipboard denied','NotAllowedError')}}})`);
  await clickWhenEnabled('history-copy');
  await waitFor(() => evaluate('!document.getElementById("history-copy-fallback").hidden'), 'HTTP clipboard rejection did not expose manual copy');
  assert.deepEqual(await evaluate('(() => {const input=document.getElementById("history-copy-value");return [input.readOnly,input.value,document.activeElement===input,input.selectionStart,input.selectionEnd]})()'),
    [true, 'persisted-wake-0', true, 0, 'persisted-wake-0'.length], 'Manual copy did not select the readonly original task ID');
  assert.equal(posts.length, writesBeforeVerify, 'Copy fallback submitted work');
  await evaluate(`Object.defineProperty(navigator, 'clipboard', {configurable:true, value:undefined})`);
  await clickWhenEnabled('history-copy');
  assert.equal(await evaluate('!document.getElementById("history-copy-fallback").hidden && document.activeElement.id==="history-copy-value"'), true, 'Missing clipboard API did not preserve the manual copy fallback');
  await historyStatus('');
  await evaluate('taskResponseSummary.focus()');
  const persistedWake = wakeJobs.get('persisted-wake-0');
  for (const [older, newer] of [['completed', 'unknown'], ['unknown', 'completed']]) {
    historyDetailGate = gate();
    historyDetailGate.jobID = persistedWake.job_id;
    historyDetailGate.response = {...structuredClone(persistedWake), ...identity(), phase: older, error_code: older === 'completed' ? 'window_verified' : 'wake_outcome_unknown'};
    await clickWhenEnabled('history-verify');
    await waitFor(() => historyDetailGate.started, 'Controlled older detail request did not start');
    Object.assign(persistedWake, {phase: newer, error_code: newer === 'completed' ? 'window_verified' : 'wake_outcome_unknown', updated_at: observed()}); revision++;
    const beforeSummary = historyReads().length;
    await clickWhenEnabled('history-refresh');
    await waitFor(async () => historyReads().length > beforeSummary && await evaluate('document.getElementById("history-refresh").getAttribute("aria-busy")==="false"'), 'Newer task summary did not finish while old detail was held');
    const selectedSummary = (await assertHistoryData()).records.find(record => record.type === 'wake' && record.job_id === persistedWake.job_id);
    assert.equal(selectedSummary?.phase, newer, 'Newer summary must actually contain the selected record before testing state ordering');
    assert.equal(await evaluate('document.getElementById("history-detail-safety").hidden'), newer !== 'unknown', 'Newer summary did not control unknown safety state');
    historyDetailGate.release(); historyDetailGate = undefined;
    await waitFor(() => evaluate('document.getElementById("history-verify").getAttribute("aria-busy")==="false"'), 'Older detail did not finish');
    assert.equal(await evaluate('document.getElementById("history-detail-safety").hidden'), newer !== 'unknown', 'Older detail rolled back the newer unknown safety state');
    const responseState = await evaluate('({connected:taskResponse.isConnected,open:taskResponse.open,focused:document.activeElement===taskResponseSummary,active:document.activeElement.outerHTML.slice(0,200),sameResponse:taskResponse===document.querySelector("#history-detail-body [data-render-key=wake-response]")})');
    assert.equal(responseState.connected && responseState.open && responseState.focused, true, 'Out-of-order responses disturbed focus or response expansion: ' + JSON.stringify({older, newer, ...responseState}));
  }
  Object.assign(persistedWake, {phase: 'unknown', error_code: 'wake_outcome_unknown', updated_at: observed()}); revision++;
  await clickWhenEnabled('history-reset');
  await waitFor(async () => !latestHistory().searchParams.has('status') && !latestHistory().searchParams.has('search') && await evaluate('document.getElementById("history-refresh").getAttribute("aria-busy")==="false"'), 'History reset did not clear server filters');
  await assertHistoryData();
  await assertUnknownSafety();
  await evaluate('const search=document.getElementById("history-search");search.focus();search.setSelectionRange(0,0)');
  await evaluate('document.getElementById("refresh").onclick()');
  assert.deepEqual(await evaluate('[document.activeElement.id,document.getElementById("history-search").value,document.getElementById("history-search").selectionStart,document.getElementById("history-search").selectionEnd]'), ['history-search', '', 0, 0], 'Polling stole filter focus or selection');
  assert.equal(await evaluate('taskResponse.isConnected && taskResponse.open'), true, 'Filter focus polling collapsed the response');
  await evaluate('taskResponseSummary.focus()');
  const beforeHistoryRefresh = historyReads().length;
  await clickWhenEnabled('history-refresh');
  await waitFor(async () => historyReads().length > beforeHistoryRefresh && await evaluate('document.getElementById("history-refresh").getAttribute("aria-busy")==="false" && document.getElementById("history-verify").getAttribute("aria-busy")==="false"'), 'Task refresh did not finish');
  assert.equal(await evaluate('taskResponse.isConnected && taskResponse.open && document.activeElement===taskResponseSummary'), true, 'Task polling reset response expansion or focus');
  for (const [width, height] of [[2560, 1600], [1280, 900], [390, 844]]) {
    await cdp('Emulation.setDeviceMetricsOverride', {width, height, deviceScaleFactor: 1, mobile: width < 600});
    await evaluate('document.fonts.ready.then(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))))');
    assert.equal(await evaluate('document.documentElement.scrollWidth <= document.documentElement.clientWidth'), true, 'Task history overflow at ' + width);
    assert.equal(await evaluate('document.querySelector(".history-table-scroll").clientWidth > 0 && document.getElementById("history-detail").getBoundingClientRect().width > 0'), true, 'Task table or details hidden at ' + width);
    if (width === 2560) assert.equal(await evaluate('document.getElementById("history-detail").getBoundingClientRect().left >= document.querySelector(".history-card").getBoundingClientRect().right'), true, 'Desktop task details are not right aligned');
    await evaluate('(async () => {document.activeElement?.blur?.();document.querySelectorAll("#toast-region button").forEach(button=>button.click());window.scrollTo(0,0);await new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve)))})()');
    assert.equal(await evaluate('window.scrollY'), 0, 'Task history screenshot did not return to the page top');
    if (width > 1100) {
      await evaluate('document.getElementById("history-detail-body").scrollTop=document.getElementById("history-detail-body").scrollHeight');
      assert.equal(await evaluate('document.getElementById("history-detail-body").scrollTop>0'), true, 'Long response did not exercise detail scrolling');
      await assertUnknownSafety(true);
      const geometry = await evaluate(`(() => {
        const rect = selector => {const box=document.querySelector(selector).getBoundingClientRect();return {top:box.top,bottom:box.bottom,width:box.width,height:box.height};};
        return {width:innerWidth,height:innerHeight,scrollY,actions:rect('.history-detail-actions'),body:rect('#history-detail-body'),detail:rect('#history-detail'),viewport:{height:visualViewport.height,offsetTop:visualViewport.offsetTop}};
      })()`);
      console.log('Task history desktop action geometry: ' + JSON.stringify(geometry));
      if (geometry.actions.bottom > geometry.height) await screenshot('task-history-actions-outside-' + width + '.png');
      assert.equal(geometry.actions.bottom <= geometry.height, true, 'Detail actions left the desktop viewport: ' + JSON.stringify(geometry));
    }
    await screenshot('task-history-' + width + 'x' + height + '.png');
    if (width === 390) {
      await evaluate('document.getElementById("history-detail-close").click()');
      assert.equal(await evaluate('document.getElementById("history-detail-safety").hidden && document.getElementById("history-copy-fallback").hidden'), true, 'Closing detail retained stale safety or copy text');
      assert.equal(await evaluate(`document.activeElement===document.querySelector('#history-list [data-render-key="wake:persisted-wake-0"] button')`), true, 'Closing mobile detail did not restore the original details button focus');
      await evaluate(`document.querySelector('#history-list [data-render-key="wake:persisted-wake-0"] button').click()`);
      await waitFor(() => evaluate('document.activeElement.id==="history-detail-title" && document.getElementById("history-verify").getAttribute("aria-busy")==="false"'), 'Explicit mobile detail did not focus its title');
      await evaluate('new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve)))');
      assert.equal(await evaluate('document.getElementById("history-detail-title").getAttribute("tabindex")==="-1" && document.getElementById("history-detail-title").getBoundingClientRect().top>=0 && document.getElementById("history-detail-title").getBoundingClientRect().bottom<=innerHeight && scrollY>0'), true, 'Mobile explicit detail did not scroll the title into view');
      await assertUnknownSafety(true);
      const detailScroll = await evaluate('scrollY');
      await evaluate('document.getElementById("refresh").onclick()');
      assert.deepEqual(await evaluate('[document.activeElement.id,scrollY]'), ['history-detail-title', detailScroll], 'Mobile polling moved explicit detail focus or scroll');
      await screenshot('task-history-details-' + width + 'x' + height + '.png');
      await evaluate('globalThis.taskResponse=document.querySelector("#history-detail-body [data-render-key=wake-response]");taskResponse.open=true;document.getElementById("history-detail-body").scrollTop=document.getElementById("history-detail-body").scrollHeight');
      await assertUnknownSafety(true);
      assert.equal(await evaluate('document.querySelector(".history-detail-actions").getBoundingClientRect().bottom<=innerHeight'), true, 'Mobile long response hid read-only detail actions');
      await screenshot('task-history-details-response-' + width + 'x' + height + '.png');
      await evaluate('globalThis.taskResponseSummary=taskResponse.querySelector("summary");taskResponseSummary.focus()');
    }
  }
  historyUnavailable = true;
  await clickWhenEnabled('history-refresh');
  await waitFor(() => evaluate('document.getElementById("history-message").dataset.error==="true"'), 'Unavailable task history was not reported');
  assert.match(await evaluate('document.getElementById("history-message").textContent'), /上次成功|非最新/);
  assert.equal(await evaluate('taskResponse.isConnected && taskResponse.open'), true, 'History outage discarded an already readable response');
  historyUnavailable = false;
  await clickWhenEnabled('history-refresh');
  await waitFor(() => evaluate('document.getElementById("history-message").dataset.error==="false"'), 'Task history did not recover after read-only retry');
  assert.equal(posts.length, writesBeforeHistory, 'History pagination, filtering, details or unknown verification created work');
  await cdp('Emulation.setDeviceMetricsOverride', {width: 2560, height: 1600, deviceScaleFactor: 1, mobile: false});
  const failureID = 'fixture-error-details';
  const failureJob = {...identity(), job_id: failureID, id: failureID, phase: 'failed', trigger: 'manual_batch', completed: 24, failed: 1, total: 25, created_at: observed(), updated_at: observed(),
    items: Array.from({length: 25}, (_, index) => ({account_id: 'error-fixture-account-' + index, phase: index === 24 ? 'failed' : 'completed', error_code: index === 24 ? 'quota_upstream_forbidden' : ''}))};
  jobs.set(failureID, failureJob); fixtureJobs.unshift({...failureJob, items: undefined}); revision++;
  const writesBeforeDetails = posts.length;
  await evaluate('document.getElementById("refresh").onclick()');
  await evaluate('document.querySelector("[data-page=history]").click()');
  await waitFor(() => evaluate(`!!document.querySelector('#history-list [data-render-key="query:${failureID}"] button')`), 'Error summary did not expose details');
  await evaluate(`document.querySelector('#history-list [data-render-key="query:${failureID}"] button').click()`);
  await waitFor(() => evaluate('!!document.querySelector("#history-detail-body .task-errors")'), 'Per-account errors were not loaded');
  await evaluate('globalThis.errorDetails=document.querySelector("#history-detail-body .task-errors");errorDetails.open=true;globalThis.errorItem=errorDetails.querySelector(".task-error-details");errorItem.open=true;globalThis.errorSummary=errorItem.querySelector("summary");errorSummary.focus()');
  assert.match(await evaluate('errorItem.textContent'), /quota_upstream_forbidden/);
  assert.match(await evaluate('errorItem.textContent'), /HTTP 403/);
  assert.match(await evaluate('errorItem.textContent'), /error-fixture-account-24/);
  assert.match(await evaluate('errorItem.textContent'), /fixture-error-details/);
  await evaluate('document.getElementById("refresh").onclick()');
  await waitFor(() => evaluate('errorItem.isConnected && errorItem.open && errorDetails.open && document.activeElement === errorSummary'), 'Polling reset error expansion or keyboard focus');
  for (const [width, height] of [[2560, 1600], [1280, 900], [390, 844]]) {
    await cdp('Emulation.setDeviceMetricsOverride', {width, height, deviceScaleFactor: 1, mobile: width < 600});
    assert.equal(await evaluate('document.documentElement.scrollWidth <= document.documentElement.clientWidth'), true, 'Task error details overflow at ' + width);
    await screenshot('task-error-details-' + width + '.png');
  }
  assert.equal(posts.length, writesBeforeDetails, 'Reading error details created work');
  const statusSelector = id => '[data-account-status-id=' + JSON.stringify(id) + ']';
  const statusState = () => evaluate(`(() => {
    const button=document.querySelector(${JSON.stringify(statusSelector('codex-01'))});
    return {checked:button?.getAttribute('aria-checked'),disabled:button?.disabled,phase:button?.dataset.state};
  })()`);
  async function assertAccountActionLayout(alias = '') {
    const layout = await evaluate(`Array.from(document.querySelectorAll('#accounts .account-row'), row => {
      const rectangle=node=>{const box=node.getBoundingClientRect();return {width:box.width,height:box.height,top:box.top,bottom:box.bottom}};
      const name=row.querySelector('.account-name strong'), style=getComputedStyle(name);
      return {row:rectangle(row),query:rectangle(row.querySelector('.account-query:not(.account-wake)')),
        wake:rectangle(row.querySelector('.account-wake')),status:rectangle(row.querySelector('[data-account-status-id]')),
        pending:row.querySelector('.query-pending') ? {text:row.querySelector('.query-pending').textContent,...rectangle(row.querySelector('.query-pending'))} : null,
        alias:{...rectangle(name),text:name.textContent,title:name.title,whiteSpace:style.whiteSpace,overflow:style.overflow,textOverflow:style.textOverflow,scrollWidth:name.scrollWidth,clientWidth:name.clientWidth}};
    })`);
    assert.equal(layout.length, 6, 'Desktop action layout did not exercise a complete account page');
    for (const [index, account] of layout.entries()) {
      const detail = 'row ' + index + ': ' + JSON.stringify(account);
      assert.ok(account.status.width >= 120, 'CPA switch was squeezed below a readable width: ' + detail);
      for (const action of ['query', 'wake', 'status']) {
        assert.ok(account[action].height >= 28 && account[action].height <= 40, 'Account action became stretched or too small (' + action + '): ' + detail);
      }
      assert.ok(Math.abs(account.query.top - account.wake.top) <= 1, 'Query and wake buttons no longer share their first action row: ' + detail);
      assert.ok(account.status.top >= Math.max(account.query.bottom, account.wake.bottom), 'CPA status switch must occupy a separate second action row: ' + detail);
      if (account.row.height > 140) await screenshot('account-action-row-too-tall-' + index + '.png');
      assert.ok(account.row.height <= 140, 'Account row became excessively tall: ' + detail);
    }
    if (alias) {
      const name = layout[0].alias;
      assert.equal(name.text, alias, 'Ellipsis changed the literal alias text');
      assert.equal(name.title, alias, 'Truncated alias lost its full privacy-safe title');
      assert.equal(name.whiteSpace, 'nowrap', 'Long alias wrapped into multiple lines');
      assert.equal(name.overflow, 'hidden', 'Long alias was not clipped within its account column');
      assert.equal(name.textOverflow, 'ellipsis', 'Long alias did not use an ellipsis');
      assert.ok(name.height <= 24 && name.scrollWidth > name.clientWidth, 'Long alias did not exercise single-line truncation: ' + JSON.stringify(name));
    }
    return layout;
  }
  async function clickStatus(id = 'codex-01') {
    await evaluate(`document.querySelector(${JSON.stringify(statusSelector(id))}).click()`);
    assert.equal(await evaluate('!!document.querySelector("dialog[open]")'), false, 'CPA switch opened a confirmation modal');
  }
  async function verifyStatus() {
    const count = patches.length, before = reads.filter(path => path.endsWith('/auth-files')).length;
    await clickStatus();
    await waitFor(async () => reads.filter(path => path.endsWith('/auth-files')).length > before && (await statusState()).phase === 'confirmed', 'Row switch did not GET-verify the uncertain status');
    assert.equal(patches.length, count, 'Verification resent an auth-file PATCH');
    assert.equal((await statusState()).checked, String(!authFiles[0].disabled));
  }
  await cdp('Emulation.setDeviceMetricsOverride', {width: 1586, height: 992, deviceScaleFactor: 1, mobile: false});
  await evaluate('document.querySelector("[data-page=overview]").click()');
  await waitFor(() => evaluate(`!!document.querySelector(${JSON.stringify(statusSelector('codex-01'))})`), 'Overview switch missing');
  await assertAccountActionLayout();
  const writesBeforeCPAStatus = posts.length;
  authPatchGate = gate();
  await clickStatus();
  await waitFor(() => authPatchGate.started, 'Direct switch did not PATCH CPA status');
  assert.deepEqual(patches[0].body, {name: 'real+primary.codex.json', auth_index: 'fixture-index-0', disabled: true});
  await clickStatus(); assert.equal(patches.length, 1, 'Saving allowed a duplicate PATCH');
  assert.equal((await statusState()).disabled, true);
  const beforeStatusCommitReads = reads.filter(path => path.endsWith('/auth-files')).length;
  authPatchGate.release(); authPatchGate = undefined;
  await waitFor(async () => (await statusState()).phase === 'confirmed' && (await statusState()).checked === 'false', 'Disable was not GET-verified');
  assert.ok(reads.filter(path => path.endsWith('/auth-files')).length > beforeStatusCommitReads);
  assert.equal(await evaluate('document.querySelector("#accounts .status-badge").textContent'), '手动禁用');
  assert.equal(posts.length, writesBeforeCPAStatus);
  assert.equal(await evaluate('document.getElementById("wake-manual-fields").disabled'), true);
  for (const outcome of ['disconnect', 'persist-hook-failed']) {
    authPatchGate = gate(outcome);
    const count = patches.length;
    await clickStatus(); await waitFor(() => authPatchGate.started, 'Direct PATCH missing: ' + outcome);
    authReadGate = gate('reject');
    authPatchGate.release(); authPatchGate = undefined;
    await waitFor(() => authReadGate.started, 'Uncertain PATCH skipped GET verification');
    authReadGate.release(); authReadGate = undefined;
    await waitFor(async () => (await statusState()).phase === 'unknown', 'Uncertain PATCH was reported as confirmed');
    await assertAccountActionLayout();
    await evaluate('window.dispatchEvent(new Event("focus"))'); await settleRefresh();
    assert.equal((await statusState()).phase, 'unknown', 'Polling cleared an unknown result');
    assert.equal(patches.length, count + 1, 'Unknown PATCH retried automatically');
    await verifyStatus();
    assert.equal(await evaluate('document.getElementById("wake-manual-fields").disabled'), true);
  }
  authPatchGate = gate(); await clickStatus();
  await waitFor(() => authPatchGate.started, 'Identity-race PATCH missing');
  authFilesOverride = authFiles.map(file => file.id === 'codex-01' ? {...file, id: 'replacement-account', auth_index: 'replacement-index'} : {...file});
  authPatchGate.release(); authPatchGate = undefined;
  await waitFor(async () => (await statusState()).phase === 'unknown', 'Replacement identity incorrectly confirmed');
  const identityRacePatches = patches.length;
  await clickStatus();
  await waitFor(async () => !(await statusState()).disabled, 'Identity-race read did not finish');
  assert.equal((await statusState()).phase, 'unknown'); assert.equal(patches.length, identityRacePatches);
  authFilesOverride = undefined; await verifyStatus();
  accountReadGate = {...gate(), snapshot: true}; await clickWhenEnabled('refresh');
  await waitFor(() => accountReadGate.started, 'Late account snapshot was not held');
  authPatchGate = gate(); await clickStatus();
  await waitFor(() => authPatchGate.started, 'Concurrent status PATCH missing');
  authPatchGate.release(); authPatchGate = undefined;
  await waitFor(async () => (await statusState()).phase === 'confirmed', 'Concurrent PATCH was not verified');
  const verifiedStatus = (await statusState()).checked;
  accountReadGate.release(); accountReadGate = undefined;
  await waitFor(() => evaluate('!document.getElementById("sync").disabled'), 'Old accounts GET did not finish');
  assert.equal((await statusState()).checked, verifiedStatus, 'Late account read rolled back verified status');
  for (const [label, files] of [
    ['duplicate account ID', [...structuredClone(authFiles), {...authFiles[0], name: 'duplicate.json'}]],
    ['name versus ID collision', [...structuredClone(authFiles), {...authFiles[1], id: authFiles[0].name, name: 'collision.json'}]],
    ['runtime-only', authFiles.map(file => file.id === 'codex-01' ? {...file, runtime_only: true} : {...file})],
    ['virtual child', authFiles.map(file => file.id === 'codex-01' ? {...file, plugin_virtual_child: true} : {...file})]
  ]) {
    authFilesOverride = files;
    const count = patches.length; await clickStatus();
    await waitFor(async () => (await statusState()).phase === 'error', 'Unsafe identity was not rejected: ' + label);
    assert.equal(patches.length, count, 'Unsafe identity permitted PATCH: ' + label);
    authFilesOverride = undefined;
  }
  assert.equal(posts.length, writesBeforeCPAStatus);
  console.log('Direct CPA switch regression passed: exact file binding, GET verification, duplicate lock, uncertain results, identity rejection and no confirmation modals.');
  const skipIDs = ['fixture-skipped-current', 'fixture-skipped-legacy'];
  for (const [index, jobID] of skipIDs.entries()) wakeJobs.set(jobID, {job_id: jobID, account_id: 'codex-01', phase: 'skipped', reason: 'manual',
    created_at: observed(), updated_at: observed(), error_code: index ? 'window_already_active' : '', hold_reason: index ? '' : 'window_already_active'});
  revision++;
  await evaluate('document.querySelector("[data-page=history]").click()');
  await historyFilter('history-search', 'fixture-skipped-', 'input');
  const skipSummary = await assertHistoryData();
  assert.equal(skipSummary.total, 2); assert.equal(skipSummary.summary.failed, 0); assert.equal(skipSummary.summary.unknown, 0);
  assert.equal(await evaluate('document.getElementById("history-attention").textContent'), '0', 'Skipped tasks increased the attention/error count');
  assert.doesNotMatch(await evaluate('document.getElementById("history-list").textContent'), /请求失败|账号绑定已变化|错误详情/);
  for (const jobID of skipIDs) {
    await evaluate(`document.querySelector('#history-list [data-render-key="wake:${jobID}"] button').click()`);
    await waitFor(() => evaluate('document.getElementById("history-verify").getAttribute("aria-busy")==="false"'), 'Skipped task detail did not load');
    assert.match(await evaluate('document.getElementById("history-detail-body").textContent'), /活动窗口|无需.*唤醒/);
    assert.equal(await evaluate('document.querySelectorAll("#history-detail-body .task-error-details,#history-detail-body .task-errors").length'), 0, 'Normal skip rendered an error card: ' + jobID);
    assert.equal(await evaluate('document.getElementById("history-detail-safety").hidden'), true, 'Normal skip showed an unknown/error safety warning');
  }
  await evaluate('document.querySelector("[data-page=wake]").click()'); await clickWhenEnabled('wake-history-refresh');
  for (const jobID of skipIDs) {
    assert.equal(await evaluate(`document.querySelector('#wake-history [data-render-key="wake:${jobID}"]')?.querySelectorAll('.task-error-details,.task-errors').length`), 0, 'Wake history classified a skip as an error: ' + jobID);
  }
  const preciseWakeErrors = [
    ['wake_account_not_found', /未找到此账号|账号已不存在/], ['wake_account_disabled', /已禁用此账号|账号已停用/],
    ['wake_host_unavailable', /CPA.*无法提供该账号/], ['wake_inventory_stale', /账号状态.*(?:未同步|过期)/],
    ['wake_account_ambiguous', /账号匹配不唯一/], ['wake_binding_missing', /缺少.*绑定/],
    ['wake_binding_changed', /账号绑定已变更/], ['wake_account_unavailable', /无法核实账号.*唤醒/]
  ];
  await evaluate('document.querySelector("[data-page=history]").click()');
  for (const [code, text] of preciseWakeErrors) {
    const jobID = 'fixture-precise-' + code;
    wakeJobs.set(jobID, {job_id: jobID, account_id: 'codex-01', phase: 'failed', reason: 'manual', error_code: code, created_at: observed(), updated_at: observed()}); revision++;
    await historyFilter('history-search', jobID, 'input');
    await evaluate(`document.querySelector('#history-list [data-render-key="wake:${jobID}"] button').click()`);
    await waitFor(() => evaluate('document.getElementById("history-verify").getAttribute("aria-busy")==="false"'), 'Precise wake failure detail did not load: ' + code);
    const explanation = await evaluate('document.getElementById("history-detail-body").textContent');
    assert.match(explanation, text, 'Wake failure lost its precise explanation: ' + code);
    if (code === 'wake_account_unavailable') assert.doesNotMatch(explanation, /账号凭据绑定已变化|账号绑定已变更/, 'Old generic wake failure incorrectly asserted a binding change');
    if (code === 'wake_host_unavailable') assert.doesNotMatch(explanation, /CPA.*(?:连接断开|未连接)/, 'Unavailable account incorrectly asserted host disconnection');
  }
  assert.equal(posts.length, writesBeforeCPAStatus, 'Inspecting skips or wake failures submitted work');
  await clickWhenEnabled('history-detail-close');
  await historyFilter('history-search', '', 'input');
  async function displayMode(mode) {
    await evaluate(`(() => {const modeField=document.getElementById('display-mode');modeField.value=${JSON.stringify(mode)};modeField.dispatchEvent(new Event('change'))})()`);
    assert.equal(await evaluate('document.getElementById("display-mode").value'), mode);
  }
  async function setAlias(id, alias) {
    await evaluate(`document.querySelector('[data-account-alias-id="${id}"]').click()`);
    await waitFor(() => evaluate('document.getElementById("account-alias-dialog").open'), 'Alias editor did not open');
    await evaluate(`(() => {const aliasField=document.getElementById('account-alias-value');aliasField.value=${JSON.stringify(alias)};aliasField.dispatchEvent(new Event('input'))})()`);
    await clickWhenEnabled('account-alias-save');
    await waitFor(() => evaluate('!document.getElementById("account-alias-dialog").open'), 'Alias editor did not save locally');
  }
  async function assertNoVisibleAccountID() {
    const leaked = await evaluate(`(() => {
      const visible=node=>!!node.getClientRects().length && getComputedStyle(node).visibility!=='hidden';
      const text=[];const walker=document.createTreeWalker(document.body,NodeFilter.SHOW_TEXT);
      while(walker.nextNode()){const node=walker.currentNode;if(visible(node.parentElement)&&!node.parentElement.closest('script,style,input,textarea,select'))text.push(node.textContent)}
      const attributes=Array.from(document.querySelectorAll('[title],[aria-label]')).filter(visible).map(node=>(node.title||'')+' '+(node.getAttribute('aria-label')||''));
      return text.concat(attributes).filter(value=>/codex-\\d{2}|persisted-(?:wake-)?account-\\d+|antigravity-(?:fixture|disabled)|error-fixture-account-\\d+|real\\+primary\\.codex\\.json|bravo\\.fixture@example\\.test/.test(value));
    })()`);
    assert.deepEqual(leaked, [], 'Masked/alias mode leaked visible account identities');
    assert.equal(await evaluate('Array.from(document.querySelectorAll("#generation-account-options option"),option=>option.value+" "+option.textContent+" "+option.label).some(value=>/codex-\\d{2}|antigravity-(?:fixture|disabled)/.test(value))'), false, 'Account dropdown suggestions leaked raw identities');
  }
  await evaluate('document.querySelector("[data-page=overview]").click()');
  accounts[1].id = 'bravo.fixture@example.test'; accountsRevision++; revision++;
  await waitFor(() => evaluate('document.getElementById("accounts").rows[1]?.querySelector("strong")?.textContent==="bravo.fixture@example.test"'), 'Email display fixture did not reach account rendering');
  const writesBeforeDisplay = posts.length, patchesBeforeDisplay = patches.length;
  await displayMode('masked');
  assert.equal(await evaluate('document.querySelector("#accounts .account-name strong").textContent'), 'c***1');
  assert.equal(await evaluate('document.getElementById("accounts").rows[1].querySelector("strong").textContent'), 'b***@***', 'Email account was not masked');
  for (const page of ['overview', 'accounts', 'query', 'models', 'wake', 'history', 'diagnostics']) {
    await evaluate(`document.querySelector('[data-page="${page}"]').click()`);
    await assertNoVisibleAccountID();
  }
  await evaluate('document.querySelector("[data-page=overview]").click()');
  const injectedAlias = '<img src=x onerror=globalThis.aliasExecuted=true>主账号';
  await setAlias('codex-01', injectedAlias);
  await clickWhenEnabled('next');
  await waitFor(() => evaluate('!!document.querySelector("[data-account-alias-id=codex-07]")'), 'Second page did not expose alias editing');
  await setAlias('codex-07', '值班账号');
  await clickWhenEnabled('previous');
  await waitFor(() => evaluate('!!document.querySelector("[data-account-alias-id=codex-01]")'), 'Alias editing did not return to the first page');
  await displayMode('alias');
  assert.equal(await evaluate('document.querySelector("#accounts .account-name strong").textContent'), injectedAlias);
  assert.equal(await evaluate('document.querySelectorAll("#accounts img,#account-alias-dialog img").length'), 0, 'Alias became executable HTML');
  assert.notEqual(await evaluate('globalThis.aliasExecuted'), true);
  assert.match(await evaluate('document.getElementById("tasks").textContent'), /值班账号/, 'Overview task target did not use the saved alias');
  await evaluate('globalThis.aliasButton=document.querySelector("[data-account-alias-id=codex-01]");aliasButton.focus()');
  const aliasReads = reads.filter(path => path.includes('/accounts?')).length;
  accounts[0].quota.weekly.Remaining.Value = 63; accountsRevision++; revision++;
  await waitFor(() => reads.filter(path => path.includes('/accounts?')).length > aliasReads, 'Alias focus test did not receive actual account polling');
  await waitFor(() => evaluate('document.getElementById("accounts").rows[0].cells[3].textContent.includes("63%")'), 'Alias polling did not render new quota');
  assert.equal(await evaluate('aliasButton.isConnected && document.activeElement===aliasButton'), true, 'Polling replaced the focused alias button');
  await evaluate('aliasButton.click();const field=document.getElementById("account-alias-value");field.focus();field.value="未保存别称";field.dispatchEvent(new Event("input"));field.setSelectionRange(1,3)');
  const overviewBeforeAliasDraft = reads.filter(path => path.endsWith('/overview')).length;
  await waitFor(() => reads.filter(path => path.endsWith('/overview')).length > overviewBeforeAliasDraft, 'Alias draft did not receive real polling');
  assert.deepEqual(await evaluate('[document.activeElement.id,document.getElementById("account-alias-value").value,document.getElementById("account-alias-value").selectionStart,document.getElementById("account-alias-value").selectionEnd]'), ['account-alias-value', '未保存别称', 1, 3], 'Polling disturbed alias draft or selection');
  await evaluate('document.getElementById("account-alias-cancel").click()');
  for (const page of ['overview', 'accounts', 'query', 'models', 'wake', 'history', 'diagnostics']) {
    await evaluate(`document.querySelector('[data-page="${page}"]').click()`); await assertNoVisibleAccountID();
  }
  assert.equal(posts.length, writesBeforeDisplay, 'Local display preferences posted to CPA');
  assert.equal(patches.length, patchesBeforeDisplay, 'Local display preferences patched CPA');
  const preferenceKey = 'cwr-v2-display:' + origin + '/cpa';
  assert.deepEqual(await evaluate(`JSON.parse(localStorage.getItem(${JSON.stringify(preferenceKey)}))`), {mode: 'alias', aliases: {'codex-01': injectedAlias, 'codex-07': '值班账号'}});
  await cdp('Page.reload');
  await waitFor(() => evaluate('document.getElementById("accounts")?.rows.length===6 && document.getElementById("login-section")?.hidden'), 'Display persistence reload did not connect');
  assert.equal(await evaluate('document.getElementById("display-mode").value'), 'alias');
  assert.equal(await evaluate('document.querySelector("#accounts .account-name strong").textContent'), injectedAlias);
  for (const width of [1586, 390]) {
    await cdp('Emulation.setDeviceMetricsOverride', {width, height: 992, deviceScaleFactor: 1, mobile: width < 600});
    assert.equal(await evaluate('document.documentElement.scrollWidth <= document.documentElement.clientWidth'), true, 'Alias-mode overview overflow at ' + width);
    if (width === 1586) {
      const layout = await assertAccountActionLayout(injectedAlias);
      console.log('Desktop account action layout passed: ' + JSON.stringify(layout.map(account => ({rowHeight: account.row.height, statusWidth: account.status.width, statusHeight: account.status.height, queryHeight: account.query.height, wakeHeight: account.wake.height}))));
    }
    await screenshot('account-display-alias-' + width + '.png');
  }
  assert.match(await evaluate('document.getElementById("accounts").rows[1].querySelector(".account-wake").getAttribute("aria-label")'), /b\*\*\*@\*\*\*/);
  await assertNoVisibleAccountID();
  assert.equal(posts.length, writesBeforeDisplay, 'Displaying a masked wake action sent a model request');
  assert.equal(await evaluate('document.getElementById("wake-overview-consent-row").hidden'), true, 'Reload repeated acknowledged automatic-wake notice');
  await cdp('Emulation.setDeviceMetricsOverride', {width: 1586, height: 992, deviceScaleFactor: 1, mobile: false});
  await evaluate('document.querySelector("[data-page=overview]").click();document.querySelector("#accounts .account-query").click()');
  await waitFor(() => posts.length > writesBeforeDisplay, 'Alias-mode query did not submit');
  assert.deepEqual(JSON.parse(posts.at(-1).body), {kind: 'quota_query', account_id: 'codex-01'}, 'Alias changed the request account ID');
  assert.equal(posts.at(-1).resource, 'jobs');
  await cdp('Page.navigate', {url: origin + '/other-cpa/v0/resource/plugins/codex-window-refresh/status'});
  await waitFor(() => evaluate('document.getElementById("accounts")?.rows.length===6 && document.getElementById("login-section")?.hidden'), 'Second CPA base fixture did not connect');
  assert.equal(await evaluate('document.getElementById("display-mode").value'), 'original', 'Display preferences leaked to another CPA base on the same origin');
  assert.equal(await evaluate('document.querySelector("#accounts .account-name strong").textContent'), 'codex-01', 'Alias leaked to another CPA base');
  await cdp('Page.navigate', {url: origin + base + 'status'});
  await waitFor(() => evaluate('document.getElementById("accounts")?.rows.length===6 && document.getElementById("login-section")?.hidden'), 'Original CPA base did not reconnect');
  assert.equal(await evaluate('document.getElementById("display-mode").value'), 'alias');
  await displayMode('original');
  console.log('Display/skip regression passed: precise wake explanations, informational current/legacy skips without error cards/counts, all-page alias/mask text and accessibility rendering, XSS-safe aliases, polling focus/draft preservation, reload/base-scoped local preferences and unchanged request identity.');
  const writesBeforeHTTP = posts.length;
  await cdp('Emulation.setDeviceMetricsOverride', {width: 390, height: 844, deviceScaleFactor: 1, mobile: true});
  await cdp('Page.navigate', {url: httpOrigin + base + 'status'});
  await waitFor(() => evaluate(`location.origin===${JSON.stringify(httpOrigin)} && document.getElementById("login-section")?.hidden && document.getElementById("accounts")?.rows.length===6`), 'Non-secure local HTTP fixture did not connect');
  assert.equal(await evaluate('location.protocol==="http:" && !isSecureContext && typeof navigator.clipboard==="undefined"'), true, 'HTTP copy fallback was not tested in a genuinely insecure context');
  await evaluate('document.querySelector("[data-page=history]").click()');
  await waitFor(() => evaluate('document.getElementById("history-refresh").getAttribute("aria-busy")==="false" && document.getElementById("history-count").textContent!=="— 条"'), 'HTTP task history did not load');
  await historyFilter('history-search', 'persisted-wake-0', 'input');
  await evaluate(`document.querySelector('#history-list [data-render-key="wake:persisted-wake-0"] button').click()`);
  await waitFor(() => evaluate('document.getElementById("history-verify").getAttribute("aria-busy")==="false"'), 'HTTP original wake detail did not load');
  await assertUnknownSafety();
  await clickWhenEnabled('history-copy');
  assert.deepEqual(await evaluate('(() => {const input=document.getElementById("history-copy-value");return [!document.getElementById("history-copy-fallback").hidden,input.readOnly,input.value,document.activeElement===input,input.selectionStart,input.selectionEnd]})()'),
    [true, true, 'persisted-wake-0', true, 0, 'persisted-wake-0'.length], 'Real HTTP manual copy did not expose and select the readonly original ID');
  await screenshot('task-history-http-copy-fallback-390x844.png');
  await clickWhenEnabled('history-detail-close');
  assert.equal(await evaluate('document.getElementById("history-copy-fallback").hidden && document.getElementById("history-copy-value").value==="" && document.getElementById("history-detail-safety").hidden'), true, 'Closing HTTP detail did not clear copy or warning state');
  assert.equal(posts.length, writesBeforeHTTP, 'HTTP navigation, detail or copy fallback submitted work');
  await cdp('Emulation.setDeviceMetricsOverride', {width: 1586, height: 992, deviceScaleFactor: 1, mobile: false});
  unauthorized = true;
  await waitFor(() => evaluate('!document.getElementById("login-section").hidden && document.getElementById("query").disabled'), 'Expired authentication did not stop polling');
  const readCount = reads.length;
  const requestsAfterUnauthorized = networkRequests.length;
  unauthorized = false;
  await evaluate('window.dispatchEvent(new Event("focus"));window.dispatchEvent(new Event("online"));document.dispatchEvent(new Event("visibilitychange"))');
  await new Promise(resolve => setTimeout(resolve, 1800));
  assert.equal(reads.length, readCount, 'Authentication failure kept polling');
  assert.equal(networkRequests.length, requestsAfterUnauthorized, 'Resume events restored credentials after HTTP 401');
  assert.equal(exceptions.length, 0, JSON.stringify(exceptions));
  assert.equal(pageErrors.length, 0, JSON.stringify(pageErrors));
  assert.equal(posts.filter(post => post.resource === 'settings').length, 8, 'Unexpected settings write after browser checks');
  assert.equal(posts.filter(post => post.resource === 'model-settings').length, 2, 'Unexpected model settings write count');
  assert.equal(posts.filter(post => post.resource === 'prompt-generations').length, 1, 'Unexpected generation write count');
  assert.equal(posts.filter(post => post.resource === 'wake-settings').length, 4, 'Unexpected wake settings writes');
  assert.equal(posts.filter(post => post.resource === 'wake-jobs').length, 1, 'Unexpected model wake writes');
  assert.equal(patches.length, 5, 'Unexpected auth-file status PATCH count');
  assert.equal(patches.every(patch => patch.path === '/cpa/v0/management/auth-files/status' && patch.body.name === 'real+primary.codex.json' && patch.body.auth_index === 'fixture-index-0'), true, 'Status PATCH escaped the exact auth file binding');
  assert.equal(posts.filter(post => post.resource === 'scheduling').length, 1, 'Unexpected scheduling writes');
  assert.equal(posts.length, 21, 'Unexpected total API write count');
  assert.equal(posts.every(post => ['settings', 'jobs', 'sync', 'model-settings', 'prompt-generations', 'wake-settings', 'wake-jobs', 'scheduling'].includes(post.resource)), true, 'Unexpected API write');
  assert.equal(reads.filter(path => path.endsWith('/generation-accounts')).length, 2, 'Navigation or polling unexpectedly refreshed generation accounts');
  console.log('V2 browser checks passed: desktop/mobile layout, settings/draft isolation/conflicts, query queue/finish-time contracts, filtering/paging/authentication, generation consent, direct wake without a modal, exactly one wake POST, five direct filename/index-bound CPA PATCHes with GET verification and unknown protection, global alias/mask preferences scoped to CPA base with stable request IDs and polling focus, precise wake explanations, informational legacy/current skips, persisted task history and real trigger menus, bidirectional summary/detail revision ordering, permanent unknown warning outside long scrollable responses, readonly verification without POST, clipboard fallback, mobile detail focus/scroll, unavailable-history recovery, fixture-only page traffic and screenshots.');
  console.log('Local fixture screenshots: ' + output);
  }
} finally {
  for (const held of resourceReadGates.values()) held.release();
  accountReadGate?.release();
  settingsPostGate?.release();
  settingsReadGate?.release();
  modelReadGate?.release();
  wakeReadGate?.release();
  historyDetailGate?.release();
  authReadGate?.release();
  authPatchGate?.release();
  if (socket) socket.close();
  if (browser && browser.exitCode === null && browser.signalCode === null) {
    browser.kill('SIGTERM');
    await Promise.race([new Promise(resolve => browser.once('exit', resolve)), new Promise(resolve => setTimeout(resolve, 3000))]);
    if (browser.exitCode === null && browser.signalCode === null) browser.kill('SIGKILL');
  }
  if (server) await new Promise(resolve => server.close(resolve));
  await rm(temporary, {recursive: true, force: true, maxRetries: 5, retryDelay: 200});
}
if (!autorefreshOnly && !schedulingOnly && !projectsOnly) {
  console.log('Original V2 fixture verified 21 POSTs including sticky settings and five direct CPA PATCHes; starting isolated automatic-refresh fixture after browser cleanup.');
  const child = spawn(process.execPath, [fileURLToPath(import.meta.url)], {env: {...process.env, CWR_AUTOREFRESH_ONLY: '1'}, stdio: 'inherit'});
  const result = await new Promise((resolve, reject) => { child.once('error', reject); child.once('exit', (code, signal) => resolve({code, signal})); });
  assert.equal(result.code, 0, 'Isolated automatic-refresh Chromium fixture failed: ' + JSON.stringify(result));
  console.log('V2 full and isolated automatic-refresh Chromium fixtures passed; starting isolated project-matching fixture.');
  const projectChild = spawn(process.execPath, [fileURLToPath(import.meta.url)], {env: {...process.env, CWR_PROJECTS_ONLY: '1', CWR_AUTOREFRESH_ONLY: '0', CWR_SCHEDULING_ONLY: '0', CWR_SCREENSHOT_DIR: join(output, 'project-matching')}, stdio: 'inherit'});
  const projectResult = await new Promise((resolve, reject) => { projectChild.once('error', reject); projectChild.once('exit', (code, signal) => resolve({code, signal})); });
  assert.equal(projectResult.code, 0, 'Isolated project-matching Chromium fixture failed: ' + JSON.stringify(projectResult));
  console.log('V2 full, automatic-refresh and project-matching Chromium fixtures all passed.');
}
