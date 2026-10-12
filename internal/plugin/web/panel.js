/* Public assets reuse the same-origin CPA panel's saved management key.
 * Storage is read-only; requests use fixed local endpoints and never auto-write. */
(function (root) {
  'use strict';
  const API = '/v0/management/codex-window-refresh';
  function apiBase(location) {
    if (!location) return API;
    const marker = '/v0/resource/plugins/codex-window-refresh/';
    const index = location.pathname.lastIndexOf(marker);
    if (index < 0) throw new PanelError('invalid_endpoint');
    const prefix = location.pathname.slice(0, index);
    const url = new URL(prefix + API, location.origin);
    if (url.origin !== location.origin || !url.pathname.endsWith(API)) throw new PanelError('invalid_endpoint');
    return url.pathname;
  }
  const paths = new Set(['/summary', '/accounts', '/capabilities', '/config-editor', '/config', '/reload', '/recheck', '/quota-query']);
  class PanelError extends Error {
    constructor(code, status) { super(code); this.name = 'PanelError'; this.code = code; this.status = status || 0; }
  }
  function clone(value) { return JSON.parse(JSON.stringify(value)); }
  function validObject(value) { return value !== null && typeof value === 'object' && !Array.isArray(value); }
  // CPAMC/CPAMP use cli-proxy-auth or the legacy managementKey entry, either
  // plain JSON or reversible enc::v1:: obfuscation. Read only these known keys.
  // Format reference: router-for-me/Cli-Proxy-API-Management-Center,
  // src/utils/encryption.ts and src/stores/useAuthStore.ts.
  function panelStoredValue(context, name) {
    try {
      let value = context.localStorage.getItem(name);
      if (value === null) return null;
      if (value.startsWith('enc::v1::')) {
        const bytes = Uint8Array.from(context.atob(value.slice(9)), c => c.charCodeAt(0));
        const mask = new context.TextEncoder().encode('cli-proxy-api-webui::secure-storage|' + context.location.host + '|' + context.navigator.userAgent);
        for (let i = 0; i < bytes.length; i++) bytes[i] ^= mask[i % mask.length];
        value = new context.TextDecoder('utf-8', {fatal: true}).decode(bytes);
        // Obfuscated values are JSON; damaged values must never become a key.
        return JSON.parse(value);
      }
      try { return JSON.parse(value); } catch (_) { return value; }
    } catch (_) { return null; }
  }
  function readPanelKey(context, base) {
    function candidate(key, address) {
      if (typeof key !== 'string' || !key.trim() || /[\x00-\x1f\x7f]/.test(key)) return '';
      if (address !== null && address !== undefined && address !== '') {
        if (typeof address !== 'string') return '';
        try {
          const url = new URL(address);
          const prefix = base.slice(0, -API.length).replace(/\/$/, '');
          if (url.origin !== context.location.origin || url.username || url.password || url.search || url.hash
            || url.pathname.replace(/\/$/, '') !== prefix) return '';
        } catch (_) { return ''; }
      }
      return key.trim();
    }
    const stored = panelStoredValue(context, 'cli-proxy-auth');
    const state = validObject(stored) && validObject(stored.state) ? stored.state : null;
    const key = state ? candidate(state.managementKey, state.apiBase) : '';
    if (key) return key;
    return candidate(panelStoredValue(context, 'managementKey'),
      panelStoredValue(context, 'apiBase') || panelStoredValue(context, 'apiUrl'));
  }
  function createSession(fetcher, disconnected, timers, base) {
    base = base || API;
    timers = timers || {setTimeout: (...args) => root.setTimeout(...args), clearTimeout: (...args) => root.clearTimeout(...args)};
    let key = '', generation = 0;
    const controllers = new Set();
    function disconnect() {
      key = ''; generation++;
      for (const controller of controllers) controller.abort();
      controllers.clear();
      if (disconnected) disconnected();
    }
    function setKey(value) {
      disconnect();
      if (typeof value !== 'string' || !value.trim() || /[\x00-\x1f\x7f]/.test(value)) throw new PanelError('invalid_key');
      key = value.trim();
      return generation;
    }
    async function request(path, method, body) {
      method = method || 'GET';
      if (!paths.has(path) || !['GET', 'POST'].includes(method)) throw new PanelError('invalid_endpoint');
      if (!key) throw new PanelError('disconnected');
      const current = generation, controller = new AbortController();
      controllers.add(controller);
      let timedOut = false;
      let timer;
      try {
        timer = timers.setTimeout(() => { timedOut = true; controller.abort(); }, method === 'POST' ? 120000 : 15000);
        const headers = {'Authorization': 'Bearer ' + key, 'Accept': 'application/json'};
        if (body !== undefined) headers['Content-Type'] = 'application/json';
        const result = await fetcher(base + path, {method, headers, body: body === undefined ? undefined : JSON.stringify(body), credentials: 'omit', redirect: 'error', cache: 'no-store', signal: controller.signal});
        if (generation !== current || !key) throw new PanelError('stale');
        if (result.status === 401 || result.status === 403) { disconnect(); throw new PanelError('authentication', result.status); }
        if (!result.ok) throw new PanelError(result.status === 409 ? 'conflict' : result.status === 400 ? 'invalid_config' : 'http_error', result.status);
        // Ignore arbitrary error bodies and never reflect server text into the UI.
        if (!String(result.headers.get('content-type') || '').toLowerCase().includes('application/json')) throw new PanelError('invalid_response');
        const data = await result.json();
        if (generation !== current || !key) throw new PanelError('stale');
        return data;
      } catch (error) {
        if (generation !== current && !(error instanceof PanelError && error.code === 'authentication')) throw new PanelError('stale');
        if (error instanceof PanelError) throw error;
        throw new PanelError(timedOut ? (method === 'POST' ? 'write_unconfirmed' : 'timeout') : (method === 'POST' ? 'write_unconfirmed' : 'network'));
      } finally {
        timers.clearTimeout(timer);
        controllers.delete(controller);
      }
    }
    return {setKey, disconnect, request, generation: () => generation, connected: () => !!key};
  }
  function validDraft(value) {
    return validObject(value) && typeof value.enabled === 'boolean' && typeof value.timezone === 'string'
      && validObject(value.activity) && validObject(value.execution) && validObject(value.ordinary_rules)
      && Array.isArray(value.ordinary_rules.allowed_hours) && value.ordinary_rules.allowed_hours.every(x => typeof x === 'string')
      && validObject(value.reset_rules) && validObject(value.reset_rules.five_hour) && validObject(value.reset_rules.weekly)
      && validObject(value.reset_rules.five_hour.random_delay) && validObject(value.reset_rules.weekly.random_delay)
      && (value.quota_query === undefined || (validObject(value.quota_query) && typeof value.quota_query.automatic === 'boolean' && typeof value.quota_query.manual === 'boolean' && typeof value.quota_query.interval === 'string'))
      && Array.isArray(value.daily_rules) && value.daily_rules.every(validObject) && validObject(value.account_overrides)
      && Object.values(value.account_overrides).every(validObject);
  }
  function createEditor() {
    let draft = null, revision = '', baseline = '', version = 0, raw = false;
    const editor = {
      load(envelope, force) {
        if (!validObject(envelope) || !validObject(envelope.config) || typeof envelope.revision !== 'string') throw new PanelError('invalid_response');
        if (editor.dirty() && !force) return false;
        draft = clone(envelope.config); revision = envelope.revision; baseline = JSON.stringify(draft); raw = false; version++;
        return true;
      },
      value: () => draft ? clone(draft) : null,
      revision: () => revision,
      version: () => version,
      dirty: () => raw || (draft !== null && JSON.stringify(draft) !== baseline),
      rawDirty: () => raw,
      markRaw() { raw = true; version++; },
      replace(value) { if (!validDraft(value)) throw new PanelError('invalid_config'); draft = clone(value); raw = false; version++; },
      update(path, value) {
        if (!draft || raw) throw new PanelError('unapplied_json');
        let current = draft;
        const parts = path.split('.');
        for (let i = 0; i < parts.length - 1; i++) {
          if (!Object.hasOwn(current, parts[i]) || !validObject(current[parts[i]])) throw new PanelError('invalid_config');
          current = current[parts[i]];
        }
        if (!Object.hasOwn(current, parts[parts.length - 1])) throw new PanelError('invalid_config');
        current[parts[parts.length - 1]] = clone(value); version++;
      },
      pause(id, paused) {
        if (!draft || raw) throw new PanelError('unapplied_json');
        if (!validObject(draft.account_overrides)) draft.account_overrides = {};
        const previous = Object.hasOwn(draft.account_overrides, id) ? draft.account_overrides[id] : {};
        Object.defineProperty(draft.account_overrides, id, {value: {...previous, paused}, enumerable: true, writable: true, configurable: true});
        version++;
      },
      saved(envelope, savedVersion) {
        if (!validObject(envelope) || !validObject(envelope.config) || typeof envelope.revision !== 'string') throw new PanelError('invalid_response');
        if (version === savedVersion) editor.load(envelope, true);
        else { revision = envelope.revision; baseline = JSON.stringify(envelope.config); }
      },
      clear() { draft = null; revision = ''; baseline = ''; raw = false; version++; }
    };
    return editor;
  }
  function resetTime(value) {
    if (!value || typeof value !== 'string' || value.startsWith('0001-')) return null;
    const time = Date.parse(value);
    return Number.isFinite(time) ? time : null;
  }
  function countdown(value, now) {
    const time = resetTime(value);
    if (time === null) return '重置时间未知';
    const seconds = Math.ceil((time - (now === undefined ? Date.now() : now)) / 1000);
    if (seconds <= 0) return '已到重置时间 · 等待最新证据';
    const days = Math.floor(seconds / 86400), hours = Math.floor(seconds % 86400 / 3600), minutes = Math.floor(seconds % 3600 / 60), s = seconds % 60;
    return (days ? days + ' 天 ' : '') + String(hours).padStart(2, '0') + ':' + String(minutes).padStart(2, '0') + ':' + String(s).padStart(2, '0');
  }
  const exported = {PanelError, createSession, createEditor, resetTime, countdown, apiBase, readPanelKey};
  if (typeof module !== 'undefined' && module.exports) module.exports = exported;
  if (!root.document) return;
  const document = root.document, $ = id => document.getElementById(id);
  const editor = createEditor();
  const expandedAccounts = new Set();
  let pendingQuotaQuery = null;
  let summary = null, runningConfig = null, accounts = {}, capabilities = {}, pollTimer = null, pollBusy = false, actionBusy = false, diskMatchesRunning = true;
  const session = createSession(root.fetch.bind(root), clearView, undefined, apiBase(root.location));
  function el(tag, text, className) {
    const item = document.createElement(tag);
    if (text !== undefined) item.textContent = String(text);
    if (className) item.className = className;
    return item;
  }
  function notice(text, type) { $('notice').textContent = text; $('notice').className = 'notice' + (type ? ' ' + type : ''); $('notice').hidden = !text; }
  function clearView() {
    if (pollTimer) root.clearTimeout(pollTimer);
    pollTimer = null; pollBusy = false; actionBusy = false;
    summary = null; runningConfig = null; accounts = {}; capabilities = {}; editor.clear(); diskMatchesRunning = true;
    expandedAccounts.clear();
    pendingQuotaQuery = null;
    $('workspace').hidden = true; $('login-section').hidden = false; $('management-key').value = ''; $('config-json').value = '';
    $('connection-status').textContent = '未连接'; $('connection-status').className = 'badge neutral'; $('disconnect').disabled = true;
    for (const id of ['metrics', 'schedule-info', 'hold-counts', 'account-list', 'daily-rules', 'capabilities-list']) $(id).replaceChildren();
    for (const input of document.querySelectorAll('[data-field]')) { if (input.type === 'checkbox') input.checked = false; else input.value = ''; input.disabled = input.dataset.fixed === 'true'; }
    $('account-search').value = ''; $('account-filter').value = 'all'; $('last-error').textContent = ''; $('updated-at').textContent = ''; $('json-message').textContent = '';
    $('bridge-status').textContent = ''; $('bridge-help').textContent = '';
    for (const id of ['refresh', 'connect', 'recheck', 'reload', 'draft-restore', 'json-apply', 'json-format', 'daily-add']) $(id).disabled = false;
    $('quota-query').disabled = true; $('quota-query-status').textContent = '';
    $('save-bar').hidden = true; $('mode-title').textContent = '正在读取运行模式'; $('mode-description').textContent = ''; $('mode-badge').textContent = '';
    $('config-save').disabled = true; $('account-count').textContent = '';
    $('draft-status').textContent = '已与服务器同步'; $('draft-status').className = 'badge neutral'; notice('');
  }
  function errorMessage(error) {
    const messages = {
      authentication: '认证失败或远程管理被禁止。请核对管理密钥与 CPA 的 allow-remote 设置，然后重新连接。',
      invalid_key: '请输入有效的 CPA 管理密钥。',
      conflict: '服务器配置已被其他操作修改。你的草稿已保留；请复制需要的修改，再恢复服务器规则后重新编辑。',
      invalid_config: '规则未通过服务器校验。请检查时区、时段、延迟、模型与数值范围；草稿已保留。',
      invalid_response: 'CPA 返回了无法识别的数据，请检查插件与 CPA 版本。',
      timeout: '读取状态超时，请稍后刷新视图。',
      network: '无法读取 CPA。请检查连接，稍后刷新视图。',
      write_unconfirmed: '操作结果尚未确认，服务器可能仍在处理。请等待后重新读取服务器规则核对；草稿已保留，不会自动重试。',
      unapplied_json: '请先将 JSON 应用到草稿，或恢复服务器规则，再操作其他设置。',
      http_error: 'CPA 操作未完成，请检查服务状态与日志。'
    };
    return messages[error.code] || '操作未完成，请检查规则或服务状态。';
  }
  function report(error) { if (error.code !== 'stale' && error.code !== 'disconnected') notice(errorMessage(error), 'error'); }
  function badge(text, type) { return el('span', text, 'badge' + (type ? ' ' + type : '')); }
  function date(value) {
    const time = resetTime(value);
    if (time === null) return '未知';
    try { return new Intl.DateTimeFormat('zh-CN', {timeZone: runningConfig?.timezone || 'Asia/Shanghai', year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false}).format(time); }
    catch (_) { return new Date(time).toISOString(); }
  }
  function details(container, entries) {
    container.replaceChildren();
    for (const [label, value] of entries) { const row = el('div'); row.append(el('dt', label), el('dd', value ?? '未知')); container.append(row); }
  }
  const holds = {plugin_disabled: '模型唤醒未启用', account_inventory_unavailable: '账号清单暂不可用', account_disabled: '账号已禁用或暂停', delivery_evidence_pending: '已发送，等待确认', quota_adapter_unavailable: '额度适配器不可用', foreground_busy: '账号正在处理前台请求', backoff: '失败退避中', active_five_hour: '5 小时窗口内', quota_exhausted: '额度耗尽', quota_unknown: '额度证据未知', not_due: '尚未到执行时间', outside_hours: '不在允许时段', target_satisfied: '人数比例容量已满足', credential_changed: '账号凭据已变化', weekly_reset_unconfirmed: '周重置尚未确认', weekly_reset_conflict: '周与短窗口证据冲突', no_concurrency_slot: '等待执行空位'};
  const states = {unknown: '未知', inactive: '未启动', active: '窗口内', expired: '已到期', pending: '待处理', waiting: '等待', reserved: '已预留', dispatch_intent: '准备发送', verifying: '核实窗口', awaiting_evidence: '等待证据', satisfied: '已满足', suspended: '已暂停'};
  const reasons = {five_hour_reset: '5 小时重置', weekly_reset: '周重置', daily_schedule: '每日时间规则'};
  function renderOverview() {
    if (!summary) return;
    const s = summary.scheduler || {}, cap = s.capacity || {}, cfg = runningConfig;
    const pool = Object.values(accounts || {});
    const known = pool.filter(a => Number.isFinite(a.FiveHour?.Remaining?.Value) && Number.isFinite(a.Weekly?.Remaining?.Value)).length;
    const active = pool.filter(a => a.FiveHour?.State === 'active' && (resetTime(a.FiveHour.ResetAt) || 0) > Date.now()).length;
    const cards = [['账号总数', summary.accounts ?? pool.length, 'CPA 中发现的 Codex 账号'], ['有额度数据', known, '5 小时与周额度都有数值'], ['5 小时窗口内', active, '窗口尚未到重置时间'], ['额度待确认', s.quota_unknown_accounts ?? '—', '窗口或请求权限仍未知']];
    $('mode-banner').classList.toggle('wake-enabled', summary.enabled === true);
    $('mode-title').textContent = !summary.running ? '后台未运行' : summary.enabled ? '模型唤醒已开启' : '仅查看与查询额度';
    $('mode-description').textContent = !summary.running ? '当前显示已保存的数据；请在“诊断与高级”检查后台状态。' : summary.enabled ? '后台会按已保存的规则发送模型请求。额度查询由独立开关控制。' : '不会创建新的模型唤醒请求。用户响应被动更新，额度接口按查询设置运行。';
    $('mode-badge').textContent = '已应用配置';
    $('metrics').replaceChildren();
    for (const [label, value, hint] of cards) { const card = el('div', undefined, 'metric'); card.append(el('div', label, 'metric-label'), el('div', value, 'metric-value'), el('div', hint, 'metric-detail')); $('metrics').append(card); }
    details($('schedule-info'), [['近期用户范围', s.recent_users ?? '未知'], ['普通目标 / 已就绪', (cap.target ?? '—') + ' / ' + (cap.active ?? '—')], ['预留 / 缺口', (cap.reserved ?? '—') + ' / ' + (cap.missing ?? '—')], ['规则时区', cfg?.timezone], ['普通允许时段', Array.isArray(cfg?.ordinary_rules?.allowed_hours) ? cfg.ordinary_rules.allowed_hours.join('、') || '无' : '未知'], ['周任务限制', cfg ? '不受人数与普通时段限制' : '未知'], ['有效并发', summary.effective_concurrency], ['下一次账号清单检查', date(s.next_inventory_at)], ['后台快照时间', date(s.observed_at)]]);
    $('hold-counts').replaceChildren();
    const counts = Object.entries(s.hold_counts || {}).sort((a, b) => b[1] - a[1]);
    if (!counts.length) $('hold-counts').append(el('p', '当前没有报告的阻塞原因。', 'hint'));
    for (const [reason, count] of counts) { const row = el('div', undefined, 'hold-row'); row.append(el('span', holds[reason] || reason), badge(count, 'neutral')); $('hold-counts').append(row); }
    $('last-error').textContent = summary.last_error ? '最近诊断：' + summary.last_error : '最近诊断：无错误';
    renderQuotaQuery();
  }
  function quotaJob() {
    const job = summary?.scheduler?.quota_query || {};
    if (pendingQuotaQuery) {
      if (!summary?.running || ((job.account_id || '') === pendingQuotaQuery.target &&
          (job.started_at !== pendingQuotaQuery.previousStart || ['queued', 'running'].includes(job.phase)))) pendingQuotaQuery = null;
      else return {phase: 'queued', account_id: pendingQuotaQuery.target};
    }
    return job;
  }
  function quotaQueryReason() {
    if (!summary?.running) return '后台未运行';
    if (capabilities.QuotaAdapterReady !== true) return bridgeDescription() + '，请查看诊断';
    if (runningConfig?.quota_query?.manual !== true) return '手动查询已关闭，可在“额度查询”中开启';
    return '';
  }
  function renderQuotaQuery() {
    const query = runningConfig?.quota_query || {}, job = quotaJob();
    const running = job.phase === 'queued' || job.phase === 'running';
    const phases = {queued: '等待查询', running: '正在查询', completed: '查询结束', cancelled: '已取消', unavailable: '额度查询接口未就绪'};
    const progress = job.phase ? (job.account_id ? '单个账号 · ' : '全部账号 · ') + (phases[job.phase] || job.phase) + ' · 成功 ' + (job.completed || 0) + '，失败 ' + (job.failed || 0) + '，跳过 ' + (job.skipped || 0) + ' / 共 ' + (job.total || 0) + ' 个账号' + (job.failed && job.last_error ? '；最近失败：' + quotaFailure(job) : '') : '尚未执行手动查询';
    const reason = quotaQueryReason();
    $('quota-query-status').textContent = '定期查询：' + (query.automatic ? '开启（' + query.interval + '）' : '关闭') + ' · ' + (reason || progress);
    $('quota-query').disabled = !session.connected() || actionBusy || !!reason || running;
  }
  function renderCapabilities() {
    const labels = {StableAccountIdentity: '稳定账号身份', ExactAccountExecution: '指定账号发送', QuotaAdapterReady: '额度适配器就绪', ActivityIdentityAtStart: '请求开始时识别用户范围', ActivityIdentityAtCompletion: '请求完成时识别用户范围', ForegroundGuard: '前台请求与调度互斥保护'};
    details($('capabilities-list'), Object.entries(labels).map(([key, label]) => [label, capabilities[key] === true ? (key === 'QuotaAdapterReady' ? '已就绪' : '支持') : capabilities[key] === false ? (key === 'QuotaAdapterReady' ? '未就绪' : '不支持') : '未知']));
    $('bridge-status').textContent = capabilities.QuotaAdapterReady === true ? '后台额度查询配置已就绪' : bridgeDescription();
    const env = capabilities.quota_bridge?.management_key_env || 'management_key_env 指定的变量';
    const help = {
      host_callback: '当前使用 CPA 宿主账号凭据查询，不需要配置后台管理地址或管理密钥环境变量。凭据由 CPA 维护；账号专用代理优先，否则沿用 CPA 全局代理。',
      missing_management_url: '在 CPA 主配置的 plugins.configs.codex-window-refresh 中设置 management_url 和 management_key_env，然后重启 CPA。',
      missing_management_key_env: '在 CPA 主配置的 plugins.configs.codex-window-refresh 中设置 management_key_env（环境变量名），然后重启 CPA。',
      missing_management_key: '将现有 CPA 管理密钥设置到 CPA 进程的 ' + env + ' 环境变量，然后重启 CPA。systemd 需给实际服务配置 EnvironmentFile（用户服务用 --user 管理）；Docker 需将变量传入容器。',
      invalid_management_url: '检查 CPA 主配置中的 management_url。支持 http(s) 服务基址、/v0/management、/v0/management/api-call 或完整 /v8/management/requests/api-call；修改后重启 CPA。',
      configured: '管理地址及密钥环境变量已配置。接口能力每 5 秒更新；查询失败时再检查管理密钥、地址及上游连接。'
    };
    $('bridge-help').textContent = help[capabilities.quota_bridge?.status] || '接口能力每 5 秒更新。若持续未就绪，请检查查询方式并查看 CPA 日志。';
  }
  function bridgeDescription() {
    return {host_callback: '宿主额度查询等待后台确认', missing_management_url: '未配置后台管理接口地址', missing_management_key_env: '未配置管理密钥环境变量名', missing_management_key: 'CPA 进程未读取到管理密钥环境变量', invalid_management_url: '后台管理接口地址配置无效', configured: '额度查询配置已具备，等待后台确认'}[capabilities.quota_bridge?.status] || '额度查询接口未就绪';
  }
  function quotaFailure(job) {
    return {quota_credential_expired: '账号凭据已过期，请先在 CPA 刷新或重新登录账号', quota_credential_missing: '账号缺少有效的访问凭据', quota_proxy_invalid: '账号代理配置无效', quota_host_http_failed: 'CPA HTTP 查询失败，请检查连接或使用兼容管理桥', quota_native_query_failed: '上游额度查询失败，请检查连接与账号状态'}[job.last_error] || '可手动重试';
  }
  function windowCell(title, evidence) {
    evidence = evidence || {};
    const cell = el('div', undefined, 'window-cell'), heading = el('div', undefined, 'window-title');
    heading.append(el('span', title), el('span', states[evidence.State] || evidence.State || '未知')); cell.append(heading);
    const remaining = evidence.Remaining;
    const text = remaining && Number.isFinite(remaining.Value) ? (Math.round(remaining.Value * 100) / 100) + (remaining.Unit === 'percent' ? '%' : ' ' + (remaining.Unit || '单位未知')) : '未知';
    const source = {cpa_response_headers: '正常请求响应', cpa_websocket_quota: 'WebSocket 请求响应', cpa_wham_usage: '主动查询', cpa_execution_quota: '模块唤醒响应'}[evidence.Source] || '额度观测';
    cell.append(el('div', text, 'remaining'), el('div', '剩余额度', 'window-label'));
    if (remaining && remaining.Unit === 'percent' && Number.isFinite(remaining.Value)) {
      const meter = el('progress'); meter.max = 100; meter.value = Math.max(0, Math.min(100, remaining.Value)); meter.setAttribute('aria-label', title + '剩余 ' + text); cell.append(meter);
    }
    const kind = evidence.Source === 'cpa_execution_quota' ? 'wake' : evidence.Source === 'cpa_wham_usage' ? 'query' : 'passive';
    cell.append(el('div', source, 'quota-source ' + kind), el('div', '观测 ' + date(evidence.ConfirmedAt), 'window-time'), el('div', '重置 ' + date(evidence.ResetAt), 'window-time'));
    const timer = el('div', countdown(evidence.ResetAt), 'countdown'); timer.dataset.reset = evidence.ResetAt || ''; cell.append(timer);
    return cell;
  }
  function renderAccounts() {
    const cfg = editor.value(), search = $('account-search').value.toLowerCase(), filter = $('account-filter').value;
    const all = Object.entries(accounts || {}).sort(([a], [b]) => a.localeCompare(b));
    const visible = all.filter(([id, a]) => {
      const override = cfg?.account_overrides && Object.hasOwn(cfg.account_overrides, id) ? cfg.account_overrides[id] : {};
      const paused = override?.paused === true;
      if (!id.toLowerCase().includes(search)) return false;
      if (filter === 'active') return a.FiveHour?.State === 'active' && (resetTime(a.FiveHour.ResetAt) || 0) > Date.now();
      if (filter === 'pending') return a.Task && a.Task.State !== 'satisfied';
      if (filter === 'paused') return paused;
      if (filter === 'unknown') return !a.FiveHour?.State || a.FiveHour.State === 'unknown' || a.Permission === 'unknown';
      return true;
    });
    $('account-count').textContent = '显示 ' + visible.length + ' / ' + all.length + ' 个账号';
    $('account-list').replaceChildren();
    if (!visible.length) $('account-list').append(el('div', all.length ? '没有符合筛选条件的账号。' : '暂无账号；等待调度器读取 CPA 账号清单。', 'empty'));
    for (const [id, a] of visible) {
      const paused = cfg?.account_overrides && Object.hasOwn(cfg.account_overrides, id) && cfg.account_overrides[id]?.paused === true;
      const card = el('article', undefined, 'card account-card'), header = el('div', undefined, 'account-head'), name = el('div');
      name.append(el('div', id, 'account-id'));
      const runningPaused = runningConfig?.account_overrides && Object.hasOwn(runningConfig.account_overrides, id) && runningConfig.account_overrides[id]?.paused === true;
      const pauseChanged = paused !== Boolean(runningPaused);
      const accountStatus = pauseChanged ? (paused ? '待保存：暂停唤醒' : '待保存：恢复唤醒') : paused ? '唤醒已暂停' : a.Enabled ? '账号已启用' : '账号已禁用';
      header.append(name, badge(accountStatus, paused || pauseChanged || !a.Enabled ? 'warning' : '')); card.append(header);
      const windows = el('div', undefined, 'window-grid'); windows.append(windowCell('5 小时额度', a.FiveHour), windowCell('周额度', a.Weekly)); card.append(windows);
      const queryRow = el('div', undefined, 'account-query'), job = quotaJob();
      const querying = ['queued', 'running'].includes(job.phase), selected = job.account_id === id;
      const reason = quotaQueryReason() || (!a.Enabled && !runningPaused ? '账号已禁用，无法查询' : '');
      const queryButton = el('button', querying && selected ? '正在查询…' : '查询此账号额度', 'quiet');
      queryButton.type = 'button'; queryButton.dataset.quotaAccount = id;
      queryButton.disabled = !session.connected() || actionBusy || !!reason || querying;
      queryButton.title = reason || (querying ? '请等待当前手动查询结束' : '只访问此账号的额度接口，不发送模型请求');
      queryButton.addEventListener('click', () => { if (!queryButton.disabled) void requestQuotaQuery(id); });
      let queryText = reason || '仅查询额度，不发送模型请求';
      if (selected && job.phase) queryText = ({queued: '已排队，等待查询', running: '正在读取此账号额度', cancelled: '查询已取消', unavailable: '额度查询接口未就绪'}[job.phase]) || (job.failed ? '查询失败，' + quotaFailure(job) : job.skipped ? '未查询：账号已禁用或移除' : '查询完成 · ' + date(job.finished_at));
      const queryStatus = el('span', queryText, 'hint'); queryStatus.setAttribute('role', 'status');
      queryRow.append(queryButton, queryStatus); card.append(queryRow);
      const task = a.Task, taskInfo = el('dl', undefined, 'details task-info'), disclosure = el('details', undefined, 'account-disclosure');
      disclosure.open = expandedAccounts.has(id);
      disclosure.addEventListener('toggle', () => { if (disclosure.isConnected) { if (disclosure.open) expandedAccounts.add(id); else expandedAccounts.delete(id); } });
      disclosure.append(el('summary', '唤醒任务与诊断' + (task ? ' · ' + (states[task.State] || task.State) : ' · 无任务')));
      const pendingReasons = task?.Intents?.filter(i => i.Status === 'pending').map(i => (reasons[i.Reason] || i.Reason) + (i.RuleID ? ' / ' + i.RuleID : '')) || [];
      const due = task?.Intents?.filter(i => i.Status === 'pending').map(i => resetTime(i.RandomizedDueAt)).filter(t => t !== null) || [];
      details(taskInfo, [['任务状态', task ? states[task.State] || task.State : '无任务'], ['触发原因', [...new Set(pendingReasons)].join('、') || '无'], ['阻塞原因', task?.HoldReason ? holds[task.HoldReason] || task.HoldReason : '无'], ['随机延迟结束', due.length ? date(new Date(Math.min(...due)).toISOString()) : '无'], ['下一次检查', resetTime(task?.NextCheckAt) !== null ? date(task.NextCheckAt) : '未安排'], ['下一次窗口核实', resetTime(task?.NextVerificationAt) !== null ? date(task.NextVerificationAt) : '未安排'], ['失败退避到', resetTime(a.BackoffUntil) !== null ? date(a.BackoffUntil) : '无'], ['请求权限', a.Permission === 'allowed' ? '允许' : a.Permission === 'blocked' ? '额度阻止' : '未知'], ['普通唤醒参与', a.OrdinaryParticipant ? '是' : '否'], ['周任务参与', a.WeeklyEnabled ? '是' : '否']]); disclosure.append(taskInfo);
      const footer = el('div', undefined, 'account-footer'); footer.append(el('span', '最近真实请求 ' + date(a.LastRealRequestAt), 'hint'));
      const pause = el('button', paused ? '恢复唤醒（草稿）' : '暂停唤醒（草稿）', 'quiet'); pause.type = 'button'; pause.disabled = actionBusy || editor.rawDirty();
      pause.addEventListener('click', () => { try { editor.pause(id, !paused); draftChanged(); renderAccounts(); notice('已更新账号规则草稿；保存并应用后生效。'); } catch (error) { report(error); } });
      footer.append(pause); disclosure.append(footer); card.append(disclosure); $('account-list').append(card);
    }
  }
  function updateDraftBadge() {
    const dirty = editor.dirty();
    $('draft-status').textContent = dirty ? '有未保存的草稿' : '已与服务器同步'; $('draft-status').className = 'badge' + (dirty ? ' warning' : ' neutral');
    $('save-bar').hidden = !dirty && diskMatchesRunning && !actionBusy;
    $('config-save').disabled = actionBusy || (!dirty && diskMatchesRunning) || editor.rawDirty();
    $('save-help').textContent = !diskMatchesRunning ? '服务器规则文件与当前运行策略不同。保存将应用草稿；开启模型唤醒时可能发送模型请求。' : '保存会写入服务器规则文件并立即应用。额度查询不生成内容；开启模型唤醒时可能发送模型请求。';
    $('wake-draft-hint').textContent = editor.value()?.enabled ? '草稿：模型唤醒已开启。保存后按下方规则发送模型请求。' : '草稿：模型唤醒已关闭。保存后仅查看额度，查询由独立设置控制。';
  }
  function draftChanged() { $('config-json').value = JSON.stringify(editor.value(), null, 2); $('json-message').textContent = ''; updateDraftBadge(); renderOverview(); }
  function fieldValue(cfg, path) { let v = cfg; for (const part of path.split('.')) v = v?.[part]; return v; }
  function renderForm() {
    const cfg = editor.value(); if (!cfg) return;
    for (const input of document.querySelectorAll('[data-field]')) {
      const value = fieldValue(cfg, input.dataset.field);
      if (input.type === 'checkbox') input.checked = value === true;
      else input.value = Array.isArray(value) ? value.join(', ') : value ?? '';
    }
    renderDaily(); draftChanged();
  }
  function renderDaily() {
    $('daily-rules').replaceChildren();
    const rawDaily = editor.value()?.daily_rules;
    const daily = Array.isArray(rawDaily) ? rawDaily : [];
    if (!daily.length) $('daily-rules').append(el('p', '尚未配置每日规则。可以添加早上 09:00–09:10 的随机唤醒窗口。', 'hint'));
    daily.forEach((rule, index) => {
      const row = el('div', undefined, 'daily-row');
      function input(labelText, key, type) {
        const label = el('label', labelText), field = el('input'); field.type = type || 'text'; field.value = rule[key] ?? ''; if (type === 'number') { field.min = '1'; field.step = '1'; }
        field.addEventListener('change', () => {
          if (editor.rawDirty()) { report(new PanelError('unapplied_json')); renderDaily(); return; }
          const value = editor.value(), parsed = type === 'number' ? Number(field.value) : field.value;
          if (type === 'number' && (!field.value.trim() || !Number.isFinite(parsed))) { notice('每日最低就绪数需要填写有效整数。', 'error'); return; }
          value.daily_rules[index][key] = parsed; editor.replace(value); draftChanged();
        }); label.append(field); row.append(label);
      }
      input('规则 ID', 'id'); input('随机时间窗口', 'random_time_range'); input('最低就绪账号数', 'minimum_ready_accounts', 'number');
      const remove = el('button', '移除', 'quiet danger'); remove.type = 'button'; remove.addEventListener('click', () => { if (editor.rawDirty()) { report(new PanelError('unapplied_json')); return; } const cfg = editor.value(); cfg.daily_rules.splice(index, 1); editor.replace(cfg); renderDaily(); draftChanged(); }); row.append(remove);
      const switches = el('div', undefined, 'daily-switches');
      for (const [key, text] of [['enabled', '启用规则'], ['catch_up', '错过窗口后补发']]) { const label = el('label', undefined, 'check-field'), field = el('input'); field.type = 'checkbox'; field.checked = rule[key] === true; field.addEventListener('change', () => { if (editor.rawDirty()) { report(new PanelError('unapplied_json')); renderDaily(); return; } const cfg = editor.value(); cfg.daily_rules[index][key] = field.checked; editor.replace(cfg); draftChanged(); }); label.append(field, document.createTextNode(text)); switches.append(label); }
      row.append(switches); $('daily-rules').append(row);
    });
  }
  function busy(value) {
    actionBusy = value;
    for (const id of ['recheck', 'reload', 'draft-restore', 'json-apply', 'json-format', 'daily-add']) $(id).disabled = value;
    for (const field of document.querySelectorAll('#workspace [data-field], #config-json, #daily-rules button')) field.disabled = value || field.dataset.fixed === 'true';
    updateDraftBadge(); renderAccounts(); renderQuotaQuery();
  }
  async function readSnapshots() {
    const generation = session.generation();
    const [nextSummary, nextAccounts, nextCapabilities] = await Promise.all([session.request('/summary'), session.request('/accounts'), session.request('/capabilities')]);
    if (generation !== session.generation()) return;
    if (![nextSummary, nextAccounts, nextCapabilities].every(validObject)) throw new PanelError('invalid_response');
    summary = nextSummary; accounts = nextAccounts; capabilities = nextCapabilities; renderOverview(); renderAccounts(); renderCapabilities(); $('updated-at').textContent = '读取于 ' + new Date().toLocaleTimeString('zh-CN', {hour12: false});
  }
  function schedulePoll() {
    if (pollTimer) root.clearTimeout(pollTimer);
    pollTimer = null;
    if (session.connected() && !document.hidden) pollTimer = root.setTimeout(() => refreshSnapshots(false), 5000);
  }
  async function refreshSnapshots(manual) {
    if (!session.connected() || pollBusy || (document.hidden && !manual)) return;
    const generation = session.generation(); pollBusy = true; $('refresh').disabled = true;
    try { if (manual) await readRunningConfig(); await readSnapshots(); if (manual && generation === session.generation()) notice('状态快照已更新。'); }
    catch (error) { report(error); }
    finally { if (generation === session.generation()) { pollBusy = false; $('refresh').disabled = false; schedulePoll(); } }
  }
  async function readRunningConfig() {
    const generation = session.generation(), config = await session.request('/config');
    if (generation !== session.generation()) return;
    if (!validObject(config)) throw new PanelError('invalid_response');
    runningConfig = config;
  }
  async function loadEditor() {
    const generation = session.generation(), envelope = await session.request('/config-editor');
    if (generation !== session.generation()) return;
    editor.load(envelope, true); diskMatchesRunning = envelope.disk_matches_running !== false; renderForm(); renderAccounts();
  }
  async function connectPanel(key, automatic) {
    $('management-key').value = '';
    let generation;
    try { generation = session.setKey(key); } catch (error) { report(error); return; }
    $('connect').disabled = true;
    $('management-key').disabled = true;
    if (automatic) {
      $('login-section').hidden = true;
      $('connection-status').textContent = '正在连接';
    }
    try {
      // CPA counts failed authentication per request. Probe once before parallel reads.
      const nextSummary = await session.request('/summary');
      const [nextAccounts, caps, envelope, runtimeConfig] = await Promise.all([session.request('/accounts'), session.request('/capabilities'), session.request('/config-editor'), session.request('/config')]);
      if (generation !== session.generation()) return;
      if (![nextSummary, nextAccounts, caps, runtimeConfig].every(validObject)) throw new PanelError('invalid_response');
      editor.load(envelope, true); diskMatchesRunning = envelope.disk_matches_running !== false;
      summary = nextSummary; accounts = nextAccounts; capabilities = caps; runningConfig = runtimeConfig;
      $('workspace').hidden = false; $('login-section').hidden = true; $('connection-status').textContent = '已连接'; $('connection-status').className = 'badge'; $('disconnect').disabled = false;
      renderForm(); renderOverview(); renderAccounts(); renderCapabilities(); $('updated-at').textContent = '读取于 ' + new Date().toLocaleTimeString('zh-CN', {hour12: false});
      if (!diskMatchesRunning) notice('服务器规则文件尚未应用。这里显示的是文件草稿；运行概览中的启用状态来自当前调度器。', 'warning');
      schedulePoll();
    } catch (error) {
      if (generation === session.generation()) session.disconnect();
      report(error);
    } finally {
      if (generation === session.generation() || !session.connected()) {
        $('connect').disabled = false; $('management-key').disabled = false;
      }
    }
  }
  $('login-form').addEventListener('submit', event => {
    event.preventDefault();
    void connectPanel($('management-key').value, false);
  });
  $('disconnect').addEventListener('click', () => { if (editor.dirty() && !root.confirm('断开将清除未保存的规则草稿。继续断开？')) return; session.disconnect(); });
  function activateTab(name, focus) {
    const button = Array.from(document.querySelectorAll('[data-tab]')).find(tab => tab.dataset.tab === name);
    if (!button) return;
    document.querySelectorAll('[data-tab]').forEach(tab => { const selected = tab === button; tab.classList.toggle('active', selected); tab.setAttribute('aria-selected', String(selected)); tab.tabIndex = selected ? 0 : -1; });
    for (const view of document.querySelectorAll('.view')) view.hidden = view.id !== 'tab-' + name;
    if (focus) button.focus();
  }
  document.querySelectorAll('[data-tab]').forEach(button => {
    button.addEventListener('click', () => activateTab(button.dataset.tab));
    button.addEventListener('keydown', event => {
      const tabs = Array.from(document.querySelectorAll('[data-tab]')), index = tabs.indexOf(button);
      let next;
      if (event.key === 'ArrowRight') next = (index + 1) % tabs.length;
      else if (event.key === 'ArrowLeft') next = (index + tabs.length - 1) % tabs.length;
      else if (event.key === 'Home') next = 0;
      else if (event.key === 'End') next = tabs.length - 1;
      else return;
      event.preventDefault(); activateTab(tabs[next].dataset.tab, true);
    });
  });
  document.querySelectorAll('[data-open-tab]').forEach(button => button.addEventListener('click', () => activateTab(button.dataset.openTab, true)));
  for (const input of document.querySelectorAll('[data-field]')) input.addEventListener('change', () => {
    try {
      if (editor.rawDirty()) throw new PanelError('unapplied_json');
      let value = input.type === 'checkbox' ? input.checked : input.type === 'number' ? Number(input.value) : input.value;
      if (input.type === 'number' && (!input.value.trim() || !Number.isFinite(value))) throw new PanelError('invalid_config');
      if (input.dataset.field === 'ordinary_rules.allowed_hours') value = input.value.split(/[,，\n]/).map(x => x.trim()).filter(Boolean);
      editor.update(input.dataset.field, value); draftChanged(); renderAccounts();
    } catch (error) { report(error); const cfg = editor.value(); if (cfg) { const old = fieldValue(cfg, input.dataset.field); if (input.type === 'checkbox') input.checked = old === true; else input.value = Array.isArray(old) ? old.join(', ') : old ?? ''; } }
  });
  $('daily-add').addEventListener('click', () => {
    if (editor.rawDirty()) { report(new PanelError('unapplied_json')); return; }
    const cfg = editor.value(); cfg.daily_rules = cfg.daily_rules || []; const ids = new Set(cfg.daily_rules.map(r => r.id)); let n = 1; while (ids.has('morning-' + n)) n++;
    cfg.daily_rules.push({id: 'morning-' + n, enabled: true, random_time_range: '09:00-09:10', minimum_ready_accounts: 1, catch_up: true}); editor.replace(cfg); renderDaily(); draftChanged();
  });
  $('config-json').addEventListener('input', () => { editor.markRaw(); $('json-message').textContent = 'JSON 尚未应用到草稿。'; updateDraftBadge(); renderAccounts(); });
  $('json-apply').addEventListener('click', () => { try { const cfg = JSON.parse($('config-json').value); editor.replace(cfg); renderForm(); renderAccounts(); notice('JSON 已应用到本地草稿，尚未保存到服务器。'); } catch (_) { notice('JSON 无法解析或缺少完整规则结构。请保留完整字段并修正后再应用。', 'error'); } });
  $('json-format').addEventListener('click', () => { try { const value = JSON.parse($('config-json').value); if (!validObject(value)) throw new Error(); $('config-json').value = JSON.stringify(value, null, 2); } catch (_) { notice('JSON 无法解析或顶层不是对象。', 'error'); } });
  $('config-save').addEventListener('click', async () => {
    if (!session.connected() || actionBusy || editor.rawDirty() || (!editor.dirty() && diskMatchesRunning)) return;
    const config = editor.value();
    if (!root.confirm(config.enabled ? '保存并应用这些规则？模型唤醒已开启，调度器可能发送真实模型请求。' : '保存并应用规则？模型唤醒保持关闭，额度查询按独立设置运行；已发出的请求仍可能完成。')) return;
    const generation = session.generation(), version = editor.version(); busy(true);
    try { const envelope = await session.request('/config', 'POST', {config, revision: editor.revision()}); if (generation !== session.generation()) return; editor.saved(envelope, version); diskMatchesRunning = envelope.disk_matches_running !== false; renderForm(); renderAccounts(); notice('规则已保存并应用。'); await readRunningConfig(); await readSnapshots(); }
    catch (error) { report(error); }
    finally { if (generation === session.generation()) busy(false); }
  });
  $('draft-restore').addEventListener('click', async () => {
    if (actionBusy || !session.connected() || (editor.dirty() && !root.confirm('恢复服务器规则会丢弃当前未保存的草稿。继续？'))) return;
    const generation = session.generation(); busy(true);
    try { await loadEditor(); if (generation !== session.generation()) return; notice(diskMatchesRunning ? '已重新读取服务器规则。' : '已读取服务器规则文件；文件与当前运行策略不同，保存后才会应用。', diskMatchesRunning ? '' : 'warning'); }
    catch (error) { report(error); }
    finally { if (generation === session.generation()) busy(false); }
  });
  for (const [id, path, text] of [['recheck', '/recheck', '通知调度器重新检查现有规则？这不会强制唤醒，但启用的规则可能随后发送真实请求。'], ['reload', '/reload', '重新读取并应用服务器规则文件？启用的规则可能随后发送真实请求。']]) $(id).addEventListener('click', async () => {
    if (actionBusy || !session.connected()) return;
    if (id === 'reload' && editor.dirty() && !root.confirm('重新读取规则会丢弃当前未保存的草稿。继续？')) return;
    if (!root.confirm(text)) return;
    const generation = session.generation(); busy(true);
    try { await session.request(path, 'POST'); if (generation !== session.generation()) return; if (id === 'reload') { await loadEditor(); await readRunningConfig(); } await readSnapshots(); notice(id === 'reload' ? '规则文件已重新读取并应用。' : '已通知调度器重新检查，后续执行由规则决定。'); }
    catch (error) { report(error); }
    finally { if (generation === session.generation()) busy(false); }
  });
  $('refresh').addEventListener('click', () => refreshSnapshots(true));
  async function requestQuotaQuery(accountID) {
    if (actionBusy || !session.connected()) return;
    const generation = session.generation();
    pendingQuotaQuery = {target: accountID || '', previousStart: summary?.scheduler?.quota_query?.started_at};
    busy(true); renderQuotaQuery();
    try {
      const result = await session.request('/quota-query', 'POST', accountID ? {account_id: accountID} : undefined);
      if (generation !== session.generation()) return;
      notice(result.status === 'already_queued' ? '已有相同的额度查询任务，正在等待结果。' : accountID ? '已安排此账号的额度查询，不发送模型请求。' : '已安排全部账号的额度查询，不发送模型请求。');
      await readSnapshots();
    } catch (error) {
      if (generation === session.generation()) pendingQuotaQuery = null;
      if (['conflict', 'invalid_config', 'http_error'].includes(error.code)) notice('未能安排额度查询。请检查手动查询开关、账号和后台状态；已有查询任务时请等待结束后重试。', 'error');
      else report(error);
    }
    finally { if (generation === session.generation()) { busy(false); renderQuotaQuery(); } }
  }
  $('quota-query').addEventListener('click', () => { if (!$('quota-query').disabled) void requestQuotaQuery(); });
  $('account-search').addEventListener('input', renderAccounts); $('account-filter').addEventListener('change', renderAccounts);
  document.addEventListener('visibilitychange', () => { if (document.hidden) { if (pollTimer) root.clearTimeout(pollTimer); pollTimer = null; } else if (session.connected()) refreshSnapshots(false); });
  root.addEventListener('beforeunload', event => { if (editor.dirty()) { event.preventDefault(); event.returnValue = ''; } });
  root.addEventListener('pagehide', () => session.disconnect());
  root.setInterval(() => { if (!session.connected() || document.hidden) return; for (const item of document.querySelectorAll('[data-reset]')) item.textContent = countdown(item.dataset.reset); }, 1000);
  (function autoConnect() {
    const key = readPanelKey(root, apiBase(root.location));
    if (key) void connectPanel(key, true);
  })();
})(typeof globalThis !== 'undefined' ? globalThis : this);
