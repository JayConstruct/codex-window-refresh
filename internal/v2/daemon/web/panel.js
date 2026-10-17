'use strict';
(() => {
  const API = '/v0/management/codex-window-refresh/v2';
  const RESOURCE = '/v0/resource/plugins/codex-window-refresh/';
  const PAGE_SIZE = 6;
  const pages = {
    overview: ['运行总览', '掌握账号额度与实时查询任务'],
    accounts: ['账号额度', '查看五小时与周额度，按账号查询最新结果'],
    query: ['额度查询', '管理查询规则与随机延迟，不发送模型请求'],
    wake: ['模型唤醒', '实时重置唤醒与启动分散执行的规则预览'],
    models: ['模型与提示词', '管理唤醒模型、提示词与短句池'],
    history: ['计划与历史', '查看后台持久保存的查询任务与执行结果'],
    diagnostics: ['诊断与设置', '检查 CPA 连接、查询能力与后台异常']
  };
  const codes = {
    unauthorized: '管理认证失败，请重新连接', forbidden: '管理访问被拒绝',
    storage_unavailable: '后台存储暂不可用', automatic_query_failed: '后台自动查询调度异常', automatic_query_disabled: '自动查询规则已关闭，尚未开始的任务已取消', bridge_unavailable: 'CPA 接入层未连接',
    bridge_busy: 'CPA 接入层繁忙', bridge_instance_changed: 'CPA 接入实例已更换',
    bridge_transport_failed: 'CPA 本地连接中断', bridge_protocol_incompatible: 'CPA 接入协议不兼容',
    bridge_response_invalid: 'CPA 接入响应无效', backend_unavailable: '独立后台暂不可用',
    host_callback_failed: 'CPA 宿主回调失败', inventory_sync_failed: 'CPA 账号同步失败',
    account_identity_ambiguous: '账号身份重复', account_identity_unavailable: '无法核实账号身份',
    account_binding_changed: '账号凭据绑定已变化', account_not_found: '账号不存在',
    quota_credential_missing: '账号缺少可用凭据', quota_proxy_invalid: '账号代理配置无效',
    quota_transport_failed: '上游额度连接失败', quota_authentication_failed: '账号凭据已过期或认证失败',
    quota_upstream_forbidden: '上游拒绝额度访问', quota_upstream_rate_limited: '上游额度查询限流',
    quota_upstream_http_failed: '上游额度接口返回异常', quota_body_failed: '无法读取额度响应',
    quota_response_too_large: '额度响应超过大小限制', quota_response_unrecognized: '无法识别额度响应',
    quota_query_timeout: '额度查询超时', quota_query_failed: '额度查询失败',
    quota_clock_changed: '系统时钟变化，请重新查询', query_interrupted: '查询被中断',
    queue_full: '后台任务队列已满', job_expired: '任务记录已超过保留期限',
    not_found: '记录不存在', view_changed: '账号状态已变化，正在重新加载',
    network: '连接中断，请检查 CPA 地址和网络', timeout: '读取超时，正在重试',
    invalid_response: '后台响应格式无效', invalid_key: '请输入有效的 CPA 管理密钥',
    invalid_endpoint: '请从 CPA 的插件页面打开', sync_unavailable: '当前后台不支持手动同步', inventory_sync_unavailable: '当前后台不支持手动同步',
    write_unconfirmed: '任务是否接受尚未确认，请保留本页后重试',
    invalid_account_id: '请输入账号 ID', idempotency_conflict: '任务提交标识冲突',
    settings_conflict: '后台设置已变化，请重新加载并丢弃草稿后再编辑',
    invalid_query_settings: '查询设置无效，请检查字段', settings_unconfirmed: '保存结果尚未确认，请重新加载设置核实，不要直接重复提交'
  };
  codes.secure_random_unavailable = '浏览器无法生成安全的任务标识，请更换浏览器或使用 HTTPS';
  function requestID(crypto) {
    if (typeof crypto?.randomUUID === 'function') {
      try { return crypto.randomUUID(); } catch {}
    }
    if (typeof crypto?.getRandomValues !== 'function') throw Object.assign(new Error('secure_random_unavailable'), {code: 'secure_random_unavailable'});
    const bytes = new Uint8Array(16);
    try { crypto.getRandomValues(bytes); }
    catch { throw Object.assign(new Error('secure_random_unavailable'), {code: 'secure_random_unavailable'}); }
    return Array.from(bytes, value => value.toString(16).padStart(2, '0')).join('');
  }
  class ViewTracker {
    constructor() { this.sequence = 0; this.minimum = 0; this.instance = ''; this.instanceAt = 0; this.latest = new Map(); this.versions = new Map(); this.retired = new Set(); }
    begin(resource) { const sequence = ++this.sequence; this.latest.set(resource, sequence); return sequence; }
    barrier() { this.minimum = ++this.sequence; }
    accept(resource, sequence, data) {
      if (sequence !== this.latest.get(resource) || sequence < this.minimum) return false;
      if (typeof data?.instance_id !== 'string' || !data.instance_id || !Number.isSafeInteger(data.view_revision) || data.view_revision < 0) return false;
      if (data.instance_id !== this.instance) {
        if (this.retired.has(data.instance_id) || sequence < this.instanceAt) return false;
        if (this.instance) this.retired.add(this.instance);
        if (this.retired.size > 64) this.retired.delete(this.retired.values().next().value);
        this.instance = data.instance_id; this.instanceAt = sequence; this.versions.clear();
      }
      if (data.view_revision < (this.versions.get(resource) ?? -1)) return false;
      this.versions.set(resource, data.view_revision); return true;
    }
  }
  function apiBase(location) {
    if (!location) return API;
    const index = location.pathname.lastIndexOf(RESOURCE);
    if (index < 0) throw Object.assign(new Error('invalid_endpoint'), {code: 'invalid_endpoint'});
    const url = new URL(location.pathname.slice(0, index) + API, location.origin);
    if (url.origin !== location.origin || !url.pathname.endsWith(API)) throw Object.assign(new Error('invalid_endpoint'), {code: 'invalid_endpoint'});
    return url.pathname;
  }
  function storedValue(context, name) {
    try {
      let raw = context.localStorage.getItem(name);
      if (raw === null) return null;
      if (raw.startsWith('enc::v1::')) {
        const bytes = Uint8Array.from(context.atob(raw.slice(9)), character => character.charCodeAt(0));
        const mask = new context.TextEncoder().encode('cli-proxy-api-webui::secure-storage|' + context.location.host + '|' + context.navigator.userAgent);
        for (let index = 0; index < bytes.length; index++) bytes[index] ^= mask[index % mask.length];
        return JSON.parse(new context.TextDecoder('utf-8', {fatal: true}).decode(bytes));
      }
      try { return JSON.parse(raw); } catch { return raw; }
    } catch { return null; }
  }
  function readPanelKey(context, base) {
    const candidate = (key, address) => {
      if (typeof key !== 'string' || !key.trim() || /[\x00-\x1f\x7f]/.test(key)) return '';
      if (address) {
        if (typeof address !== 'string') return '';
        try {
          const url = new URL(address), prefix = base.slice(0, -API.length);
          if (url.origin !== context.location.origin || url.username || url.password || url.search || url.hash || url.pathname.replace(/\/$/, '') !== prefix) return '';
        } catch { return ''; }
      }
      return key.trim();
    };
    const saved = storedValue(context, 'cli-proxy-auth');
    const current = candidate(saved?.state?.managementKey, saved?.state?.apiBase);
    if (current) return current;
    return candidate(storedValue(context, 'managementKey'), storedValue(context, 'apiBase') || storedValue(context, 'apiUrl'));
  }
  function timestamp(raw) {
    if (typeof raw !== 'string' || raw.startsWith('0001-')) return NaN;
    return Date.parse(raw);
  }
  function age(raw, now = Date.now()) {
    const at = timestamp(raw);
    if (!Number.isFinite(at)) return '尚未观测';
    const seconds = Math.max(0, Math.floor((now - at) / 1000));
    if (seconds < 10) return '刚刚';
    if (seconds < 60) return seconds + ' 秒前';
    if (seconds < 3600) return Math.floor(seconds / 60) + ' 分钟前';
    if (seconds < 86400) return Math.floor(seconds / 3600) + ' 小时前';
    return Math.floor(seconds / 86400) + ' 天前';
  }
  function resetText(raw, now = Date.now()) {
    const at = timestamp(raw);
    if (!Number.isFinite(at)) return '重置时间未知';
    if (at <= now) return '已到重置时间 · 待核实';
    const minutes = Math.max(1, Math.ceil((at - now) / 60000));
    if (minutes >= 1440) return '约 ' + Math.floor(minutes / 1440) + ' 天 ' + Math.floor(minutes % 1440 / 60) + ' 小时后重置';
    if (minutes >= 60) return '约 ' + Math.floor(minutes / 60) + ' 小时 ' + minutes % 60 + ' 分钟后重置';
    return '约 ' + minutes + ' 分钟后重置';
  }
  function sourceText(source) {
    const sources = {cpa_wham_usage: '主动查询', cpa_response_headers: '正常业务响应', cpa_websocket_quota: '正常业务推送', cpa_model_response: '模型响应'};
    return Object.hasOwn(sources, source) ? sources[source] : '额度观测';
  }
  function windowView(quota, name, now = Date.now()) {
    const window = quota?.[name], remaining = window?.Remaining, value = remaining?.Value;
    const percent = remaining?.Unit === 'percent' && typeof value === 'number' && Number.isFinite(value) && value >= 0 && value <= 100 ? value : null;
    const source = quota?.fields?.[name]?.source || window?.Source || quota?.source;
    return {percent, amount: percent === null && remaining?.Unit !== 'percent' && typeof value === 'number' && Number.isFinite(value) && value >= 0 ? value + ' ' + (remaining.Unit || '') : '', reset: resetText(window?.ResetAt, now), source: sourceText(source), observedAt: quota?.fields?.[name]?.observed_at || window?.ConfirmedAt || quota?.observed_at};
  }
  function accountStatus(account) {
    if (account.status === 'disabled') return {label: '已禁用', kind: 'disabled'};
    if (account.status === 'removed') return {label: '已移除', kind: 'disabled'};
    if (account.status === 'binding_unavailable') return {label: '身份待核实', kind: 'unavailable'};
    if (account.status === 'unavailable') return {label: '暂不可用', kind: 'unavailable'};
    if (account.quota?.permission === 'blocked') return {label: '额度受限', kind: 'attention'};
    return {label: '正常', kind: 'available'};
  }
  const phaseNames = {queued: '等待查询', blocked: '等待 CPA', running: '查询中', completed: '查询完成', failed: '查询失败', cancelled: '已取消'};
  const triggerNames = {manual_single: '手动单账号', manual_batch: '手动批量', periodic: '定期查询', five_hour_expiry: '5 小时到期', weekly_expiry: '周限额到期', initial_sync: '启动同步'};
  const holdNames = {excluded_hours: '处于排除时段', automatic_query_disabled: '自动查询规则已关闭', inventory_unavailable: '等待 CPA 账号同步', bridge_unavailable: '等待 CPA 连接'};
  function queueText(query, now = Date.now()) {
    const trigger = triggerNames[query.trigger], parts = [query.phase === 'running' ? '查询中' : '等待查询'];
    if (trigger) parts.push(trigger);
    if (query.phase !== 'running') {
      if (query.hold_reason) parts.push(holdNames[query.hold_reason] || '后台暂缓执行');
      const at = timestamp(query.scheduled_at);
      if (Number.isFinite(at)) {
        const seconds = Math.ceil((at - now) / 1000);
        parts.push(seconds > 0 ? (seconds >= 60 ? Math.floor(seconds / 60) + ' 分 ' + seconds % 60 + ' 秒' : seconds + ' 秒') + '后计划查询' : '已到计划时间，等待后台执行');
      }
    }
    return parts.join(' · ');
  }
  function displayedJobPhase(job) {
    if (!['running', 'queued', 'blocked'].includes(job.phase)) return job.phase;
    const querying = Array.isArray(job.items) ? job.items.some(item => item.phase === 'running') : job.querying > 0;
    if (querying) return 'running';
    return jobWaiting(job) > 0 || timestamp(job.scheduled_at) > Date.now() || job.hold_reason ? 'queued' : job.phase;
  }
  function jobWaiting(job) {
    return Array.isArray(job.items) ? job.items.filter(item => item.phase === 'queued').length : job.waiting;
  }
  function configDraft(config) {
    return {automatic: config.automatic, fiveHour: config.on_five_hour_expiry, weekly: config.on_weekly_expiry,
      interval: String(config.interval_seconds), timezone: config.timezone, delayMin: String(config.random_delay.min_seconds), delayMax: String(config.random_delay.max_seconds), excluded: config.excluded_hours.join(', ')};
  }
  function validateQueryDraft(draft) {
    const errors = {}, integer = value => typeof value === 'string' && /^\d+$/.test(value.trim()) ? Number(value) : NaN;
    const interval = integer(draft.interval), minimum = integer(draft.delayMin), maximum = integer(draft.delayMax), timezone = draft.timezone.trim();
    if (!Number.isSafeInteger(interval) || interval < 30 || interval > 3600) errors.interval = '查询间隔须为 30–3600 的整数秒';
    if (!Number.isSafeInteger(minimum) || minimum < 0 || minimum > 86400) errors.delayMin = '最小延迟须为 0–86400 的整数秒';
    if (!Number.isSafeInteger(maximum) || maximum < 0 || maximum > 86400 || maximum < minimum) errors.delayMax = '最大延迟须为 0–86400 的整数秒，且不小于最小延迟';
    try {
      if (!timezone || timezone === 'Local' || timezone.length > 128 || /^[+-]/.test(timezone)) throw new Error();
      new Intl.DateTimeFormat('en', {timeZone: timezone}).format();
    } catch { errors.timezone = '请输入有效的 IANA 时区，例如 Asia/Shanghai'; }
    const excluded = draft.excluded.trim() ? draft.excluded.split(',').map(span => span.trim()) : [];
    const covered = new Set();
    if (excluded.length > 24) errors.excluded = '排除时段最多 24 段';
    for (const span of excluded) {
      const match = /^([01]\d|2[0-3]):([0-5]\d)-([01]\d|2[0-3]|24):([0-5]\d)$/.exec(span);
      if (!match || match[3] === '24' && match[4] !== '00') { errors.excluded = '排除时段格式须为 HH:mm-HH:mm，以英文逗号分隔'; break; }
      const first = Number(match[1]) * 60 + Number(match[2]), last = Number(match[3]) * 60 + Number(match[4]);
      if (first === last) { errors.excluded = '排除时段的开始与结束不能相同'; break; }
      for (let minute = 0; minute < 1440; minute++) {
        if (first < last ? minute >= first && minute < last : minute >= first || minute < last) {
          if (covered.has(minute)) errors.excluded = '排除时段不可重叠';
          covered.add(minute);
        }
      }
    }
    return {errors, config: {timezone, automatic: draft.automatic, interval_seconds: interval,
      on_five_hour_expiry: draft.fiveHour, on_weekly_expiry: draft.weekly,
      random_delay: {min_seconds: minimum, max_seconds: maximum}, excluded_hours: excluded}};
  }
  function jobCounts(job) {
    if (Array.isArray(job.items)) return {total: job.items.length, completed: job.items.filter(item => item.phase === 'completed').length, failed: job.items.filter(item => item.phase === 'failed').length};
    return {total: job.total ?? job.total_items ?? 0, completed: job.completed ?? job.completed_items ?? 0, failed: job.failed ?? job.failed_items ?? 0};
  }
  function message(code) { return codes[code] || '请求失败，请查看诊断信息'; }
  if (typeof module !== 'undefined' && module.exports) module.exports = {ViewTracker, apiBase, readPanelKey, age, resetText, windowView, accountStatus, jobCounts, requestID, validateQueryDraft, configDraft, queueText};
  if (typeof document === 'undefined') return;

  const context = globalThis, $ = id => document.getElementById(id);
  let base;
  try { base = apiBase(context.location); } catch (error) { $('error').hidden = false; $('error').textContent = message(error.code); return; }
  let key = '', generation = 0, views = new ViewTracker(), pollTimer;
  let connected = false, connecting = false, refreshing = false;
  let overviewData = null, accountsData = [], accountsRevision = 0, accountsViewRevision = -1, cursor = '', nextCursor = '', previousCursors = [], pageStart = 0, filteredTotal = null;
  let filterTimer, filterSequence = 0, updatingAccounts = false, syncing = false;
  let savedSettings = null, settingsBase = null, settingsLoading = false, settingsSaving = false, settingsConflict = false, activePage = 'overview';
  const settingsFields = {automatic: 'settings-automatic', fiveHour: 'settings-five-hour', weekly: 'settings-weekly', interval: 'settings-interval', timezone: 'settings-timezone', delayMin: 'settings-delay-min', delayMax: 'settings-delay-max', excluded: 'settings-excluded'};
  const pending = new Map(), controllers = new Set(), queryBusy = new Set(), unresolved = new Map();
  const setText = (id, text) => { if ($(id)) $(id).textContent = text; };
  function element(tag, className, text) {
    const item = document.createElement(tag);
    if (className) item.className = className;
    if (text !== undefined) item.textContent = text;
    return item;
  }
  function report(error) {
    $('error').hidden = false;
    $('error').textContent = message(error.code || error.message) + '。已显示的数据可能已过期。';
  }
  function notice(text) { if ($('notice')) { $('notice').hidden = !text; $('notice').textContent = text; } }
  function clearError() { $('error').hidden = true; $('error').textContent = ''; }
  function settingsSupported() { return overviewData?.capabilities?.query_settings === true; }
  function readDraft() {
    return Object.fromEntries(Object.entries(settingsFields).map(([name, id]) => [name, ['automatic', 'fiveHour', 'weekly'].includes(name) ? $(id).checked === true : $(id).value]));
  }
  function draftDirty() { return !!settingsBase && JSON.stringify(readDraft()) !== JSON.stringify(configDraft(settingsBase.config)); }
  function fillDraft(data) {
    const draft = configDraft(data.config);
    for (const [name, id] of Object.entries(settingsFields)) {
      if (typeof draft[name] === 'boolean') $(id).checked = draft[name]; else $(id).value = draft[name];
    }
    settingsBase = data;
  }
  function applySettings(data, discard = false) {
    const config = data?.config;
    if (!Number.isSafeInteger(data?.revision) || data.revision < 0 || typeof data.instance_id !== 'string' || !config || typeof config.timezone !== 'string' ||
      !config.timezone.trim() || config.timezone.length > 128 || /[\x00-\x1f\x7f]/.test(config.timezone) ||
      !['automatic', 'on_five_hour_expiry', 'on_weekly_expiry'].every(name => typeof config[name] === 'boolean') ||
      ![config.interval_seconds, config.random_delay?.min_seconds, config.random_delay?.max_seconds].every(Number.isSafeInteger) ||
      !Array.isArray(config.excluded_hours) || config.excluded_hours.some(span => typeof span !== 'string') ||
      Object.keys(validateQueryDraft(configDraft(config)).errors).some(name => name !== 'timezone')) throw Object.assign(new Error('invalid_response'), {code: 'invalid_response'});
    if (savedSettings?.instance_id === data.instance_id && savedSettings.revision > data.revision) return false;
    const dirty = draftDirty();
    savedSettings = data;
    if (discard || !dirty) { fillDraft(data); settingsConflict = false; }
    else if (settingsBase.instance_id !== data.instance_id || settingsBase.revision !== data.revision) settingsConflict = true;
    renderSettings(); return true;
  }
  function renderSettings() {
    const supported = settingsSupported(), dirty = draftDirty(), draft = readDraft();
    const errors = settingsBase ? validateQueryDraft(draft).errors : {};
    $('settings-fields').disabled = !connected || !supported || !settingsBase || settingsSaving || settingsLoading;
    $('settings-save').disabled = !connected || !supported || !dirty || !!Object.keys(errors).length || settingsConflict || settingsSaving || settingsLoading;
    $('settings-reload').disabled = !connected || !supported || settingsSaving || settingsLoading;
    for (const [name, id] of Object.entries(settingsFields)) $(id).setAttribute('aria-invalid', errors[name] ? 'true' : 'false');
    setText('settings-validation', Object.values(errors).join('；')); $('settings-validation').hidden = !Object.keys(errors).length;
    $('settings-conflict').hidden = !settingsConflict;
    setText('settings-status', !connected ? '等待连接' : !supported ? '尚未接入' : settingsLoading ? '读取中' : settingsSaving ? '保存中' : settingsConflict ? '设置冲突' : dirty ? '未保存草稿' : savedSettings ? '版本 ' + savedSettings.revision : '尚未读取');
    const diff = $('settings-diff'); diff.replaceChildren();
    if (settingsBase) {
      const original = configDraft(settingsBase.config), labels = {automatic: '定期自动查询', fiveHour: '5 小时到期查询', weekly: '周限额到期查询', interval: '查询间隔（秒）', timezone: '时区', delayMin: '最小延迟（秒）', delayMax: '最大延迟（秒）', excluded: '排除时段'};
      const display = value => typeof value === 'boolean' ? value ? '开启' : '关闭' : value || '无';
      for (const name of Object.keys(settingsFields)) {
        if (draft[name] === original[name]) continue;
        const row = element('div', 'settings-diff-row');
        row.append(element('strong', '', labels[name]), element('span', '', display(original[name]) + ' → ' + display(draft[name]))); diff.append(row);
      }
      if (!dirty) diff.append(element('p', '', '暂无修改。只有点击“保存并应用”才会写入后台。'));
    } else diff.append(element('p', '', connected && !supported ? '当前后台未提供查询设置能力，不能保存规则。' : '连接后读取已保存设置'));
    for (const [id, name] of [['five-hour-query', 'on_five_hour_expiry'], ['weekly-query', 'on_weekly_expiry']]) {
      $(id).checked = savedSettings?.config?.[name] === true;
      $(id).disabled = !connected || !supported || settingsSaving || settingsLoading;
    }
    $('auto-wake').checked = false; $('auto-wake').disabled = true;
    setText('rules-status', connected && supported ? '查询可配置' : '尚未接入');
    setText('settings-save', settingsSaving ? '保存中…' : '保存并应用');
  }
  async function reloadSettings(discard = false) {
    if (!connected || !settingsSupported() || settingsLoading || settingsSaving) return false;
    const current = generation, sequence = views.begin('settings'); settingsLoading = true; renderSettings();
    try {
      const data = await api('/settings');
      if (current !== generation || !views.accept('settings', sequence, data)) return false;
      if (!applySettings(data, discard)) return false;
      setText('settings-message', discard ? '已重新加载后台设置并丢弃草稿，尚未写入任何更改。' : '已读取后台设置；修改后请预览差异并保存。');
      return true;
    } catch (error) { if (current === generation) setText('settings-message', message(error.code)); return false; }
    finally { if (current === generation) { settingsLoading = false; renderSettings(); } }
  }
  async function saveSettings() {
    if (!connected || !settingsSupported() || settingsSaving || settingsLoading || settingsConflict || !draftDirty()) return;
    if (settingsBase.instance_id !== views.instance) { settingsConflict = true; renderSettings(); return; }
    const validated = validateQueryDraft(readDraft());
    if (Object.keys(validated.errors).length) { renderSettings(); return; }
    const current = generation, sequence = views.begin('settings');
    settingsSaving = true; renderSettings(); setText('settings-message', '正在保存设置…');
    try {
      const data = await api('/settings', {method: 'POST', body: JSON.stringify({revision: settingsBase.revision, config: validated.config})});
      if (current !== generation) return;
      if (!views.accept('settings', sequence, data)) throw Object.assign(new Error('settings_unconfirmed'), {code: 'settings_unconfirmed'});
      if (!applySettings(data, true)) throw Object.assign(new Error('settings_conflict'), {code: 'settings_conflict'});
      views.barrier(); setText('settings-message', '已保存并应用。后台将按此规则持续执行额度查询，不发送模型请求。');
    } catch (error) {
      if (current !== generation) return;
      if (['settings_conflict', 'settings_unconfirmed', 'network', 'timeout', 'invalid_response'].includes(error.code)) settingsConflict = true;
      setText('settings-message', message(['network', 'timeout', 'invalid_response'].includes(error.code) ? 'settings_unconfirmed' : error.code));
    } finally { if (current === generation) { settingsSaving = false; renderSettings(); } }
  }
  async function stageRule(name, checked) {
    if (!connected || !settingsSupported() || settingsSaving || settingsLoading) { renderSettings(); return; }
    navigate('query', false);
    if ((!settingsBase || settingsBase.instance_id !== views.instance) && !await reloadSettings()) return;
    $(settingsFields[name]).checked = checked;
    setText('settings-message', '已修改草稿，请核对差异后点击“保存并应用”。后台设置尚未改变。'); renderSettings();
  }
  function setConnection(text) {
    setText('connection', text);
    setText('sidebar-connection', connected ? 'CPA 已连接' : connecting ? '正在连接 CPA' : 'CPA 未连接');
    if ($('sidebar-connection')) $('sidebar-connection').dataset.connected = String(connected);
  }
  function controls() {
    for (const id of ['query', 'query-single', 'refresh', 'sync']) if ($(id)) $(id).disabled = !connected || syncing && id === 'sync' || queryBusy.has('batch') && id === 'query';
    $('connect').disabled = connecting; $('key').disabled = connecting;
    setText('query', unresolved.has('batch') ? '重试批量查询' : '批量查询');
    if ($('disconnect')) $('disconnect').hidden = !connected;
    if ($('previous')) $('previous').disabled = !connected || updatingAccounts || !previousCursors.length;
    if ($('next')) $('next').disabled = !connected || updatingAccounts || !nextCursor;
    if ($('more')) { $('more').hidden = true; $('more').disabled = !connected; }
    for (const button of document.querySelectorAll('[data-action="batch"]')) button.disabled = !connected || queryBusy.has('batch');
    renderSettings();
  }
  function stop() {
    generation++; connected = false; connecting = false; key = '';
    clearTimeout(pollTimer); clearTimeout(filterTimer);
    for (const controller of controllers) controller.abort();
    controllers.clear(); queryBusy.clear(); pending.clear();
    refreshing = false; updatingAccounts = false; syncing = false; settingsLoading = false; settingsSaving = false;
    $('login-section').hidden = false;
    setText('auto-status', '尚未连接');
    $('auto-status').dataset.state = 'offline';
    setText('metric-status', '未连接'); setText('metric-status-detail', '网页已断开 · 后台不受影响'); controls();
    for (const button of document.querySelectorAll('.account-query')) button.disabled = true;
    for (const button of document.querySelectorAll('.task-open')) button.disabled = true;
  }
  async function api(path, options = {}) {
    if (!key) throw Object.assign(new Error('unauthorized'), {code: 'unauthorized'});
    const current = generation, controller = new AbortController();
    controllers.add(controller);
    let timedOut = false;
    const deadline = setTimeout(() => { timedOut = true; controller.abort(); }, 10000);
    try {
      const response = await fetch(base + path, {...options, headers: {'Authorization': 'Bearer ' + key, 'Accept': 'application/json', ...(options.body ? {'Content-Type': 'application/json'} : {}), ...(options.headers || {})}, credentials: 'omit', redirect: 'error', cache: 'no-store', signal: controller.signal});
      if (current !== generation) throw Object.assign(new Error('stale'), {code: 'stale'});
      if (response.status === 401 || response.status === 403) {
        stop(); setConnection('认证失效，请重新连接');
        throw Object.assign(new Error('unauthorized'), {code: 'unauthorized', status: response.status});
      }
      const data = await response.json();
      if (current !== generation) throw Object.assign(new Error('stale'), {code: 'stale'});
      if (!response.ok) throw Object.assign(new Error('request_failed'), {code: typeof data?.error === 'string' && Object.hasOwn(codes, data.error) ? data.error : 'network', status: response.status});
      if (data === null || typeof data !== 'object' || Array.isArray(data)) throw Object.assign(new Error('invalid_response'), {code: 'invalid_response'});
      return data;
    } catch (error) {
      if (error.code) throw error;
      throw Object.assign(new Error('request_failed'), {code: timedOut ? 'timeout' : 'network'});
    } finally { clearTimeout(deadline); controllers.delete(controller); }
  }
  function badge(label, kind) {
    const span = element('span', 'status-badge ' + kind);
    span.append(element('span', 'status-dot'), document.createTextNode(label)); return span;
  }
  function renderQuota(quota, name) {
    const view = windowView(quota, name), cell = element('div', 'quota-cell');
    if (view.percent !== null) {
      const line = element('div', 'quota-line'), progress = element('progress', 'quota-progress' + (view.percent < 20 ? ' low' : ''));
      progress.max = 100; progress.value = view.percent;
      progress.setAttribute('aria-label', (name === 'five_hour' ? '五小时' : '周') + '剩余 ' + view.percent + '%');
      line.append(progress, element('span', 'quota-percent', Number(view.percent.toFixed(1)) + '%')); cell.append(line);
    } else cell.append(element('span', 'quota-amount', view.amount || '—'));
    if (quota?.[name]) {
      const reset = element('small', 'quota-reset', view.reset), source = element('small', 'quota-source sr-only', view.source + ' · ' + age(view.observedAt));
      cell.title = view.source + ' · ' + age(view.observedAt);
      reset.dataset.resetAt = quota[name].ResetAt || '';
      source.dataset.observedAt = view.observedAt || ''; source.dataset.quotaSource = view.source;
      cell.append(reset, source);
    } else cell.append(element('small', 'quota-reset', '尚无额度数据'));
    return cell;
  }
  function renderAccounts() {
    $('accounts').replaceChildren();
    for (const account of accountsData) {
      const row = element('tr', 'account-row'), name = element('td', 'account-name');
      name.append(element('strong', '', account.id));
      const status = element('td'), value = accountStatus(account);
      status.append(badge(value.label, value.kind));
      const five = element('td'), weekly = element('td'), observed = element('td'), action = element('td');
      five.append(renderQuota(account.quota, 'five_hour')); weekly.append(renderQuota(account.quota, 'weekly'));
      const observedAge = element('span', 'observed-age', age(account.observed_at)); observedAge.dataset.observedAt = account.observed_at || '';
      observed.append(observedAge, element('small', 'observed-source', account.observed_at ? sourceText(account.quota?.source) : '等待首次查询'));
      const accountKey = 'account:' + account.id;
      const button = element('button', 'account-query', queryBusy.has(accountKey) ? '提交中…' : unresolved.has(accountKey) ? '重试查询' : '立即查询');
      button.type = 'button'; button.disabled = !connected || queryBusy.has(accountKey);
      button.setAttribute('aria-label', '立即查询账号 ' + account.id); button.onclick = () => submitQuery(account.id);
      action.append(button);
      if (['queued', 'running'].includes(account.pending_query?.phase)) action.append(queueAnnotation(account.pending_query, 'query-pending'));
      row.append(name, status, five, weekly, observed, action); $('accounts').append(row);
    }
    if (!accountsData.length) {
      const row = element('tr'), cell = element('td', 'empty-state', connected ? ($('accounts-search').value || $('accounts-filter').value !== 'all' ? '没有符合条件的账号' : '尚无账号，后台将自动同步 CPA') : '连接 CPA 后显示真实账号额度');
      cell.colSpan = 6; row.append(cell); $('accounts').append(row);
    }
    const total = overviewData?.dashboard?.total ?? accountsData.length;
    setText('accounts-count', connected ? total + ' 个账号' : '等待连接');
    setText('accounts-page-info', accountsData.length ? (pageStart + 1) + '–' + (pageStart + accountsData.length) + ' / ' + (filteredTotal ?? overviewData?.dashboard?.total ?? '—') : '0 / ' + (connected ? filteredTotal ?? total : '—'));
    controls();
  }
  function updateTimes() {
    for (const node of document.querySelectorAll('[data-reset-at]')) node.textContent = resetText(node.dataset.resetAt);
    for (const node of document.querySelectorAll('[data-observed-at]')) node.textContent = (node.dataset.quotaSource ? node.dataset.quotaSource + ' · ' : '') + age(node.dataset.observedAt);
    for (const node of document.querySelectorAll('[data-queue-phase]')) node.textContent = queueText({phase: node.dataset.queuePhase, scheduled_at: node.dataset.scheduledAt, trigger: node.dataset.trigger, hold_reason: node.dataset.holdReason});
  }
  function queueAnnotation(query, className = 'queue-time') {
    const node = element('small', className, queueText(query));
    node.dataset.queuePhase = query.phase; node.dataset.scheduledAt = query.scheduled_at || '';
    node.dataset.trigger = query.trigger || ''; node.dataset.holdReason = query.hold_reason || '';
    const at = timestamp(query.scheduled_at);
    if (Number.isFinite(at)) node.title = '后台计划时间：' + new Date(at).toLocaleString('zh-CN', {hour12: false});
    return node;
  }
  function taskElement(job, detailed = false) {
    const item = element('article', 'task-item'), heading = element('div', 'task-heading');
    const phase = displayedJobPhase(job);
    heading.append(element('strong', '', job.account_id || job.target || '全部账号'), badge(phaseNames[phase] || '状态未知', phase === 'failed' ? 'attention' : ['queued', 'blocked'].includes(phase) ? 'unavailable' : 'available'));
    const counts = jobCounts(job);
    const waiting = jobWaiting(job);
    const description = (triggerNames[job.trigger] || (job.account_id || job.target ? '单账号额度查询' : '批量额度查询')) + (detailed ? ' · 成功 ' + counts.completed + ' / 失败 ' + counts.failed + ' / 共 ' + counts.total : ' · ' + counts.completed + ' / ' + counts.total + (counts.failed ? ' · 失败 ' + counts.failed : '')) + (Number.isInteger(waiting) ? ' · 等待 ' + waiting : '');
    const time = element('small', 'task-time', age(job.updated_at || job.created_at)); time.dataset.observedAt = job.updated_at || job.created_at || '';
    item.append(heading, element('p', 'task-description', description), time);
    if (['queued', 'blocked', 'running'].includes(job.phase) && (job.scheduled_at || job.hold_reason)) item.append(queueAnnotation({...job, phase: waiting > 0 ? 'queued' : phase}));
    if (job.error_code) item.append(element('small', 'task-description error-text', message(job.error_code)));
    if (detailed && Array.isArray(job.items)) {
      const details = element('details', 'task-results');
      details.append(element('summary', '', '逐账号结果 · ' + job.items.length + ' 个账号'));
      for (const result of job.items.slice(0, 20)) {
        const row = element('p', 'task-description', (result.account_id || '账号') + ' · ' + (phaseNames[result.phase] || '状态未知') + (result.error_code ? ' · ' + message(result.error_code) : ''));
        if (['queued', 'running'].includes(result.phase)) row.append(queueAnnotation(result));
        else if (triggerNames[result.trigger]) row.append(element('small', 'task-description', ' · ' + triggerNames[result.trigger]));
        details.append(row);
      }
      if (job.items.length > 20) details.append(element('small', 'muted', '仅展示前 20 个账号结果'));
      item.append(details);
    } else if (detailed && (job.job_id || job.id)) {
      const button = element('button', 'task-open', '查看逐账号结果'); button.type = 'button'; button.disabled = !connected;
      button.onclick = () => openJob(job.job_id || job.id); item.append(button);
    }
    return item;
  }
  async function openJob(id) {
    if (!connected) return;
    const current = generation, resource = 'job:' + id, sequence = views.begin(resource);
    try {
      const job = await api('/jobs?job_id=' + encodeURIComponent(id));
      if (current !== generation || !views.accept(resource, sequence, job)) return;
      pending.set(id, job); navigate('history');
    } catch (error) { if (current === generation || error.code === 'unauthorized') report(error); }
  }
  function recentJobs() {
    const jobs = new Map([...(overviewData?.dashboard?.recent_jobs || []), ...(overviewData?.dashboard?.active_jobs || [])].map(job => [job.job_id || job.id, job]));
    for (const [id, job] of pending) jobs.set(id, {...jobs.get(id), ...job});
    return [...jobs.values()].sort((first, second) => (timestamp(second.created_at) || 0) - (timestamp(first.created_at) || 0));
  }
  function renderTasks() {
    const jobs = recentJobs(), active = jobs.filter(job => !['completed', 'failed', 'cancelled'].includes(job.phase));
    $('tasks').replaceChildren(...active.slice(0, 3).map(job => taskElement(job)));
    if (!active.length) $('tasks').append(element('div', 'empty-state', connected ? '暂无执行中的任务' : '连接后查看后台任务'));
    setText('tasks-count', Math.max(active.length, overviewData?.dashboard?.tasks?.active || 0) + ' 项');
    $('history-list').replaceChildren(...jobs.map(job => taskElement(job, true)));
    if (!jobs.length) $('history-list').append(element('div', 'empty-state', connected ? '暂无查询历史，任务将在后台持久保存' : '连接后查看已保存任务'));
    const latest = active[0];
    setText('job', latest ? (phaseNames[displayedJobPhase(latest)] || '查询任务') + ' · ' + (latest.account_id || latest.target || '全部账号') : '查询额度不发送模型请求');
  }
  function renderEvents() {
    const events = overviewData?.dashboard?.recent_events || [];
    $('recent-events').replaceChildren();
    for (const event of events.slice(0, 3)) {
      const item = element('div', 'event-item'), at = timestamp(event.at || event.created_at);
      const time = element('time', 'event-time', Number.isFinite(at) ? new Date(at).toLocaleTimeString('zh-CN', {hour12: false}) : '—');
      let detail;
      if (event.kind === 'inventory_sync' || event.kind === 'inventory') detail = event.phase === 'unavailable' ? 'CPA 账号同步失败，后台正在重试' : 'CPA 同步完成，账号状态已更新';
      else if (event.phase) detail = (event.account_id || '批量查询') + ' · ' + (phaseNames[event.phase] || '查询任务');
      else detail = event.detail || event.message || '后台状态已更新';
      item.append(time, element('span', 'event-dot'), element('span', 'event-description', detail)); $('recent-events').append(item);
    }
    if (!events.length) $('recent-events').append(element('div', 'empty-state', connected ? '暂无最近事件' : '连接后显示真实事件'));
  }
  function renderOverview() {
    const data = overviewData, stats = data?.dashboard, ready = data?.inventory?.status === 'ready' && data?.activity?.connected !== false;
    setText('metric-status', ready ? '运行中' : connected ? '等待 CPA' : '未连接');
    setText('metric-status-detail', ready ? '后台独立运行' : connected ? '保留已保存数据' : '请连接管理接口');
    setText('metric-total', stats ? String(stats.total) : '—');
    setText('metric-total-detail', stats ? stats.available + ' 正常 · ' + stats.disabled + ' 禁用 · ' + stats.unavailable + ' 暂不可用' : '等待同步账号');
    setText('metric-capacity', stats ? stats.capacity + ' / ' + stats.available : '— / —');
    setText('metric-capacity-detail', stats ? '已核实可用窗口 · ' + (stats.unknown_quota || 0) + ' 待核实' : '等待额度观测');
    setText('metric-errors', stats ? String(stats.errors + (data.last_error ? 1 : 0)) : '—');
    setText('metric-errors-detail', stats?.errors || data?.last_error ? '查看诊断详情' : stats ? '当前无已报告异常' : '等待后台数据');
    setText('last-updated', data?.inventory?.last_success_at ? age(data.inventory.last_success_at) + '同步 CPA' : '尚未同步 CPA');
    if (data) {
      setText('inventory', ready ? '页面关闭后，后台仍持续同步 CPA 并保存正常业务中的额度信息。' : 'CPA 账号同步尚未就绪；后台会自动重试，保留已保存结果。');
      const automatic = data.capabilities?.automatic_quota_query === true, randomized = data.capabilities?.randomized_quota_query === true;
      setText('feature-note', (automatic ? '到期查询可配置，修改开关仅更新草稿，保存后生效。' : '自动查询尚未接入。') + '模型唤醒尚未接入。');
      setText('account-query-note', randomized ? '单账号立即查询 · 批量按账号随机延迟，后台持久排队' : '单账号立即查询 · 批量查询由后台执行');
      setText('batch-query-note', randomized ? '每个账号由后台分别设置随机延迟，关闭页面后继续执行。' : '执行方式以当前后台能力及任务反馈为准。');
      setText('diagnostic-query-note', (automatic ? '自动额度查询已接入。' : '自动额度查询尚未接入。') + (randomized ? '批量及自动查询按账号随机延迟。' : '') + '模型唤醒及提示词配置尚未接入。');
    }
    renderSettings(); renderTasks(); renderEvents(); renderDiagnostics();
  }
  function renderDiagnostics() {
    const data = overviewData, inventory = data?.inventory;
    const rows = [
      ['后台版本', data?.version || '尚未连接'], ['后台实例', data?.instance_id || '—'],
      ['CPA 账号同步', inventory?.status === 'ready' ? '已连接' : '等待同步'],
      ['CPA 接入连接', data?.activity?.connected ? '已连接' : '等待连接'],
      ['最近成功同步', inventory?.last_success_at ? new Date(inventory.last_success_at).toLocaleString('zh-CN', {hour12: false}) : '—'],
      ['额度查询并发', data?.query_concurrency ? String(data.query_concurrency) : '—'],
      ['正常业务额度观测', data?.capabilities?.passive_quota ? '支持' : '尚未连接'],
      ['自动额度查询', data?.capabilities?.automatic_quota_query ? '支持，按已保存规则执行' : '尚未接入'],
      ['按账号随机查询延迟', data?.capabilities?.randomized_quota_query ? '支持，由后台持久保存' : '尚未接入'],
      ['模型执行', '尚未接入'], ['后台异常', data?.last_error ? message(data.last_error) : '无已报告异常'],
      ['账号同步异常', inventory?.error_code ? message(inventory.error_code) : '无已报告异常']
    ];
    $('diagnostics-content').replaceChildren(...rows.map(([label, value]) => {
      const row = element('div', 'diagnostic-row'); row.append(element('span', '', label), element('strong', '', value)); return row;
    }));
  }
  async function accounts(reset = false, expectedRevision = accountsRevision) {
    const current = generation, sequence = views.begin('accounts'), filtersAt = filterSequence;
    const search = $('accounts-search').value.trim(), status = $('accounts-filter').value || 'all';
    let path = '/accounts?limit=' + PAGE_SIZE;
    if (search) path += '&search=' + encodeURIComponent(search);
    if (status !== 'all') path += '&status=' + encodeURIComponent(status);
    const after = reset ? '' : cursor;
    if (after) path += '&after=' + encodeURIComponent(after) + '&revision=' + expectedRevision;
    updatingAccounts = true; controls();
    try {
      const data = await api(path);
      if (current !== generation || filtersAt !== filterSequence) return;
      if (!Number.isSafeInteger(data.accounts_revision) || !Array.isArray(data.accounts) || data.accounts.some(account => typeof account.id !== 'string' || typeof account.status !== 'string')) throw Object.assign(new Error('invalid_response'), {code: 'invalid_response'});
      if (!views.accept('accounts', sequence, data)) return;
      if (reset) { cursor = ''; previousCursors = []; pageStart = 0; }
      if (Number.isInteger(data.offset)) pageStart = data.offset;
      accountsData = data.accounts; accountsRevision = data.accounts_revision; accountsViewRevision = data.view_revision; nextCursor = data.next_after || '';
      filteredTotal = Number.isInteger(data.filter_total) ? data.filter_total : Number.isInteger(data.total) ? data.total : null; renderAccounts();
    } catch (error) {
      if (current !== generation || filtersAt !== filterSequence || error.code === 'stale') return;
      if (after && error.code === 'view_changed') return accounts(true);
      throw error;
    } finally { if (current === generation && filtersAt === filterSequence) { updatingAccounts = false; controls(); } }
  }
  async function overview() {
    const current = generation, sequence = views.begin('overview'), previous = views.instance;
    const data = await api('/overview');
    if (current !== generation || !views.accept('overview', sequence, data)) return null;
    if (!data.capabilities?.quota_query && !data.read_only) throw Object.assign(new Error('invalid_response'), {code: 'invalid_response'});
    if (previous && previous !== data.instance_id) { cursor = ''; nextCursor = ''; previousCursors = []; pageStart = 0; accountsRevision = -1; accountsViewRevision = -1; pending.clear(); }
    overviewData = data;
    if (savedSettings && savedSettings.instance_id !== data.instance_id) {
      if (draftDirty()) settingsConflict = true;
      else { savedSettings = null; settingsBase = null; }
    }
    if (data.query_settings && settingsSupported()) applySettings({...data.query_settings, instance_id: data.instance_id, view_revision: data.view_revision});
    renderOverview(); return data;
  }
  async function pollJobs() {
    for (const [id, previous] of [...pending]) {
      if (['completed', 'failed', 'cancelled'].includes(previous.phase)) continue;
      const current = generation, resource = 'job:' + id, sequence = views.begin(resource);
      try {
        const job = await api('/jobs?job_id=' + encodeURIComponent(id));
        if (current !== generation || !views.accept(resource, sequence, job)) continue;
        pending.set(id, job);
        if (job.phase === 'failed') notice('查询任务已结束：' + message(job.error_code || job.items?.find(item => item.error_code)?.error_code || 'quota_query_failed'));
      } catch (error) {
        if (current !== generation || error.code === 'stale') return;
        if (error.status === 404 || error.status === 410) pending.delete(id); else throw error;
      }
    }
    renderTasks();
  }
  async function refresh(forceAccounts = false) {
    if (!connected || refreshing) return;
    const current = generation; refreshing = true;
    try {
      const data = await overview();
      if (current !== generation || !data) return;
      if (forceAccounts || data.accounts_revision !== accountsRevision || data.view_revision !== accountsViewRevision) {
        await accounts(forceAccounts || !cursor, data.accounts_revision);
      }
      await pollJobs();
      if (current !== generation) return;
      updateTimes();
      setText('auto-status', '数据自动更新'); $('auto-status').dataset.state = 'online'; setConnection('独立后台已连接 · 页面每 5 秒更新'); clearError();
    } catch (error) {
      if (error.code === 'stale') return;
      if (current === generation) {
        setText('auto-status', '连接中断 · 自动重试'); $('auto-status').dataset.state = 'offline'; setConnection('后台连接中断，正在重试');
        setText('metric-status', '连接中断'); setText('metric-status-detail', '保留最后已知状态');
      }
      if (current === generation || error.code === 'unauthorized') report(error);
    } finally { if (current === generation) refreshing = false; }
  }
  function monitor(current) {
    clearTimeout(pollTimer);
    if (current !== generation || !connected) return;
    const active = recentJobs().some(job => !['completed', 'failed', 'cancelled'].includes(job.phase));
    pollTimer = setTimeout(async () => {
      if (current !== generation || !connected) return;
      await refresh();
      if (current === generation && connected) monitor(current);
    }, active ? 1500 : 5000);
  }
  async function connect(value) {
    stop(); views = new ViewTracker(); overviewData = null; accountsRevision = -1; accountsViewRevision = -1;
    if (typeof value !== 'string' || !value.trim() || /[\x00-\x1f\x7f]/.test(value)) { report({code: 'invalid_key'}); return; }
    key = value.trim(); $('key').value = ''; connecting = true; setConnection('正在连接 CPA'); controls();
    const current = generation;
    try {
      const data = await overview();
      if (current !== generation || !data) return;
      connected = true; connecting = false; $('login-section').hidden = true;
      setConnection('独立后台已连接 · 页面每 5 秒更新'); setText('auto-status', '数据自动更新'); $('auto-status').dataset.state = 'online'; controls();
      await accounts(true);
      if (current !== generation) return;
      if (activePage === 'query' && settingsSupported() && !savedSettings) await reloadSettings();
      clearError(); monitor(current);
    } catch (error) {
      if (current !== generation && error.code !== 'unauthorized') return;
      if (current === generation && connected) { report(error); monitor(current); }
      else { stop(); setConnection('连接失败，请重新连接'); report(error); }
    } finally { if (current === generation) { connecting = false; controls(); } }
  }
  async function submitQuery(target = '') {
    const busyKey = target ? 'account:' + target : 'batch';
    if (!connected || queryBusy.has(busyKey)) return;
    queryBusy.add(busyKey); controls(); renderAccounts();
    const current = generation;
    try {
      let intent = unresolved.get(busyKey);
      if (!intent) {
        intent = {idempotency: requestID(context.crypto), body: JSON.stringify({kind: 'quota_query', account_id: target})};
        unresolved.set(busyKey, intent);
      }
      const options = {method: 'POST', headers: {'Idempotency-Key': intent.idempotency}, body: intent.body};
      let result;
      try { result = await api('/jobs', options); }
      catch (error) {
        if (current !== generation || !['network', 'timeout'].includes(error.code)) throw error;
        result = await api('/jobs', options);
      }
      if (current !== generation) return;
      if (typeof result.job_id !== 'string' || result.instance_id !== views.instance) throw Object.assign(new Error('write_unconfirmed'), {code: 'write_unconfirmed'});
      unresolved.delete(busyKey);
      views.barrier(); pending.set(result.job_id, {job_id: result.job_id, account_id: target, phase: 'queued', created_at: new Date().toISOString()});
      while (pending.size > 30) pending.delete(pending.keys().next().value);
      notice(target ? target + ' 的查询任务已提交，后台将立即处理，不添加随机延迟。' : overviewData?.capabilities?.randomized_quota_query ? '批量查询已提交，每个账号由后台随机延迟并持久排队，关闭页面不会取消。' : '批量查询已提交，由后台持续执行，关闭页面不会取消。');
      clearError(); renderTasks(); await refresh(); monitor(current);
    } catch (error) {
      if (current === generation || error.code === 'unauthorized') {
        if (['network', 'timeout', 'write_unconfirmed'].includes(error.code)) {
          report({code: 'write_unconfirmed'}); notice('再次点击查询会重试同一项任务，不会创建另一项查询任务。');
        } else report(error);
      }
    }
    finally { if (current === generation) { queryBusy.delete(busyKey); controls(); renderAccounts(); } }
  }
  async function syncInventory() {
    if (!connected || syncing) return;
    const current = generation; syncing = true; controls();
    try {
      await api('/sync', {method: 'POST'});
      if (current !== generation) return;
      views.barrier(); notice('已请求同步 CPA 账号清单与状态，不查询上游额度、不发送模型请求。');
      await refresh(); monitor(current);
    } catch (error) { if (current === generation || error.code === 'unauthorized') report(error); }
    finally { if (current === generation) { syncing = false; controls(); } }
  }
  function navigate(name, loadSettings = true) {
    if (!Object.hasOwn(pages, name)) return;
    activePage = name;
    for (const view of document.querySelectorAll('[data-view]')) view.hidden = view.dataset.view !== name;
    for (const button of document.querySelectorAll('[data-page]')) {
      const selected = button.dataset.page === name; button.classList.toggle('active', selected);
      if (selected) button.setAttribute('aria-current', 'page'); else button.removeAttribute('aria-current');
    }
    const slot = name === 'accounts' ? $('accounts-page-slot') : $('overview-account-slot');
    if (slot && $('account-card')) slot.append($('account-card'));
    setText('page-title', pages[name][0]); setText('page-description', pages[name][1]); setText('breadcrumb', pages[name][0]);
    document.title = '窗口助手 · ' + pages[name][0];
    if (connected) { renderTasks(); renderDiagnostics(); }
    if (name === 'query' && loadSettings && connected && settingsSupported() && (!savedSettings || savedSettings.instance_id !== views.instance)) void reloadSettings();
  }
  function changeFilter() {
    clearTimeout(filterTimer); filterSequence++;
    if (!connected) return;
    const current = generation;
    filterTimer = setTimeout(() => { if (current === generation && connected) void accounts(true).catch(report); }, 200);
  }
  $('connect').onclick = () => connect($('key').value);
  $('key').addEventListener('keydown', event => { if (event.key === 'Enter') void connect($('key').value); });
  if ($('disconnect')) $('disconnect').onclick = () => { stop(); setConnection('已断开，后台仍持续运行'); setText('metric-status', '未连接'); };
  $('query').onclick = () => submitQuery();
  if ($('query-single')) $('query-single').onclick = () => {
    const target = $('account').value.trim();
    if (!target || target.length > 256 || /[\x00\r\n]/.test(target)) { report({code: 'invalid_account_id'}); return; }
    return submitQuery(target);
  };
  $('refresh').onclick = () => refresh(true); $('sync').onclick = syncInventory;
  $('query-settings-form').onsubmit = event => { event.preventDefault(); return saveSettings(); };
  $('settings-reload').onclick = () => reloadSettings(true);
  for (const id of Object.values(settingsFields)) $(id).addEventListener('input', () => { renderSettings(); setText('settings-message', '草稿尚未保存，请核对差异后保存。'); });
  $('five-hour-query').onchange = () => stageRule('fiveHour', $('five-hour-query').checked);
  $('weekly-query').onchange = () => stageRule('weekly', $('weekly-query').checked);
  $('accounts-search').addEventListener('input', changeFilter); $('accounts-filter').addEventListener('change', changeFilter);
  $('next').onclick = async () => {
    if (!connected || updatingAccounts || !nextCursor) return;
    previousCursors.push(cursor); cursor = nextCursor; pageStart += accountsData.length;
    try { await accounts(); } catch (error) { cursor = previousCursors.pop() || ''; pageStart = Math.max(0, pageStart - PAGE_SIZE); report(error); }
  };
  $('previous').onclick = async () => {
    if (!connected || updatingAccounts || !previousCursors.length) return;
    const previous = cursor; cursor = previousCursors.pop(); pageStart = Math.max(0, pageStart - PAGE_SIZE);
    try { await accounts(); } catch (error) { previousCursors.push(cursor); cursor = previous; pageStart += PAGE_SIZE; report(error); }
  };
  for (const button of document.querySelectorAll('[data-page]')) button.addEventListener('click', () => navigate(button.dataset.page));
  for (const button of document.querySelectorAll('[data-action="batch"]')) button.addEventListener('click', () => submitQuery());
  context.addEventListener?.('pagehide', stop);
  context.addEventListener?.('online', () => { if (connected) { void refresh(); monitor(generation); } });
  context.addEventListener?.('pageshow', event => { if (event.persisted && !connected) { const saved = readPanelKey(context, base); if (saved) void connect(saved); } });
  navigate('overview'); controls(); renderAccounts(); renderTasks(); renderEvents(); renderDiagnostics();
  const saved = readPanelKey(context, base);
  if (saved) void connect(saved);
})();
