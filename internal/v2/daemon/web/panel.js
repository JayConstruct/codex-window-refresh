'use strict';
(() => {
  const API = '/v0/management/codex-window-refresh/v2';
  const RESOURCE = '/v0/resource/plugins/codex-window-refresh/';
  const PAGE_SIZE = 6;
  const pages = {
    overview: ['运行总览', '掌握账号额度与实时查询任务'],
    accounts: ['账号额度', '查看五小时与周额度，按账号查询最新结果'],
    query: ['额度查询', '管理查询规则与随机延迟，不发送模型请求'],
    wake: ['模型唤醒', '配置后台唤醒规则、手动执行并核实任务结果'],
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
    const observedAt = quota?.fields?.[name]?.observed_at || window?.ConfirmedAt || quota?.observed_at;
    const seconds = Number.isSafeInteger(window?.ResetAfterSeconds) ? window.ResetAfterSeconds : Math.floor(timestamp(window?.ResetAt) / 1000) - Math.floor(timestamp(window?.ConfirmedAt || observedAt) / 1000);
    const fullWindow = seconds === {five_hour: 18000, weekly: 604800}[name];
    return {percent, amount: percent === null && remaining?.Unit !== 'percent' && typeof value === 'number' && Number.isFinite(value) && value >= 0 ? value + ' ' + (remaining.Unit || '') : '', reset: fullWindow ? '' : resetText(window?.ResetAt, now), source: sourceText(source), observedAt};
  }
  function accountStatus(account) {
    if (account.status === 'disabled') return {label: '已禁用', kind: 'disabled'};
    if (account.status === 'removed') return {label: '已移除', kind: 'disabled'};
    if (account.status === 'binding_unavailable') return {label: '身份待核实', kind: 'unavailable'};
    if (account.status === 'unavailable') return {label: '暂不可用', kind: 'unavailable'};
    if (account.status !== 'available') return {label: '状态未知', kind: 'unavailable'};
    const empty = ['five_hour', 'weekly'].some(name => {
      const remaining = account.quota?.[name]?.Remaining;
      return remaining?.Unit === 'percent' && remaining.Value === 0;
    });
    if (account.display_status === 'quota_exhausted' || empty) return {label: '额度耗尽', kind: 'attention'};
    if (account.display_status === 'quota_restricted' || account.quota?.permission === 'blocked') return {label: '额度受限', kind: 'attention'};
    return {label: '正常', kind: 'available'};
  }
  const phaseNames = {queued: '等待查询', blocked: '等待 CPA', running: '查询中', completed: '查询完成', failed: '查询失败', cancelled: '已取消'};
  const triggerNames = {manual_single: '手动单账号', manual_batch: '手动批量', periodic: '定期查询', five_hour_expiry: '5 小时到期', weekly_expiry: '周限额到期', initial_sync: '启动同步', wake_preflight: '唤醒前额度核对', wake_verification: '唤醒后窗口验证'};
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
  const wakeDefaults = {enabled: false, on_five_hour_reset: false, on_weekly_reset: false, timezone: 'Asia/Shanghai', random_delay: {min_seconds: 5, max_seconds: 60}, allowed_hours: ['08:00-23:00'], accounts_per_user: 1, max_accounts: 10000, daily_enabled: false, daily_window: '09:00-09:10', daily_target: 1};
  function wakeConfigDraft(config) {
    return {enabled: config.enabled, fiveHour: config.on_five_hour_reset, weekly: config.on_weekly_reset, timezone: config.timezone,
      delayMin: String(config.random_delay.min_seconds), delayMax: String(config.random_delay.max_seconds), allowed: config.allowed_hours.join(', '),
      perUser: String(config.accounts_per_user), maximum: String(config.max_accounts), daily: config.daily_enabled, dailyWindow: config.daily_window, dailyTarget: String(config.daily_target)};
  }
  function validateWakeDraft(draft) {
    const query = validateQueryDraft({automatic: false, fiveHour: false, weekly: false, interval: '300', timezone: draft.timezone, delayMin: draft.delayMin, delayMax: draft.delayMax, excluded: draft.allowed});
    const errors = {};
    for (const name of ['timezone', 'delayMin', 'delayMax']) if (query.errors[name]) errors[name] = query.errors[name];
    if (query.errors.excluded) errors.allowed = query.errors.excluded.replaceAll('排除', '允许');
    const integer = (value, name, title, minimum = 1, maximum = 10000) => {
      const result = typeof value === 'string' && /^\d+$/.test(value.trim()) ? Number(value) : NaN;
      if (!Number.isSafeInteger(result) || result < minimum || result > maximum) errors[name] = title + '须为 ' + minimum + '–' + maximum + ' 的整数';
      return result;
    };
    const accountsPerUser = typeof draft.perUser === 'string' && draft.perUser.trim() ? Number(draft.perUser) : NaN;
    if (!Number.isFinite(accountsPerUser) || accountsPerUser < 0 || accountsPerUser > 100) errors.perUser = '每用户账号数须为 0–100 的有限数值，可含小数';
    const maximum = integer(draft.maximum, 'maximum', '最大账号数'), dailyTarget = integer(draft.dailyTarget, 'dailyTarget', '每日目标容量', 0, maximum);
    const daily = validateQueryDraft({automatic: false, fiveHour: false, weekly: false, interval: '300', timezone: 'UTC', delayMin: '0', delayMax: '0', excluded: draft.dailyWindow});
    const dailySpan = daily.config.excluded_hours[0] || '';
    if (!dailySpan || daily.errors.excluded || daily.config.excluded_hours.length !== 1 || dailySpan.slice(0, 5) >= dailySpan.slice(6)) errors.dailyWindow = '每日准备窗口须为同日的 HH:mm-HH:mm，可结束于 24:00，不可跨夜';
    for (const name of ['enabled', 'fiveHour', 'weekly', 'daily']) if (typeof draft[name] !== 'boolean') errors[name] = '规则开关必须为布尔值';
    if (draft.enabled && !draft.fiveHour && !draft.weekly && !draft.daily) errors.enabled = '启用自动唤醒前，请至少开启五小时、周重置或每日准备中的一项规则';
    return {errors, config: {enabled: draft.enabled, on_five_hour_reset: draft.fiveHour, on_weekly_reset: draft.weekly, timezone: query.config.timezone,
      random_delay: query.config.random_delay, allowed_hours: query.config.excluded_hours, accounts_per_user: accountsPerUser, max_accounts: maximum,
      daily_enabled: draft.daily, daily_window: draft.dailyWindow.trim(), daily_target: dailyTarget}};
  }
  function jobCounts(job) {
    if (Array.isArray(job.items)) return {total: job.items.length, completed: job.items.filter(item => item.phase === 'completed').length, failed: job.items.filter(item => item.phase === 'failed').length};
    return {total: job.total ?? job.total_items ?? 0, completed: job.completed ?? job.completed_items ?? 0, failed: job.failed ?? job.failed_items ?? 0};
  }
  const modelDefaults = {default_model: '', default_prompt: '你好，请用一句简短的中文问候回复。', five_hour_model: '', five_hour_prompt: '', weekly_model: '', weekly_prompt: '', generation_prompt: '生成简短、自然、适合日常问候的中文短句，避免重复。', prompt_mode: 'fixed', phrases: []};
  const codexPromptPreset = '请用一句话解释 Git 中 commit 和 push 的区别，不运行命令、不读取文件。';
  function resolveModelAccount(data, id, provider) {
    const invalid = () => { throw Object.assign(new Error('model_catalog_account_invalid'), {code: 'model_catalog_account_invalid'}); };
    if (typeof id !== 'string' || !id.trim() || new TextEncoder().encode(id).length > 256 || /[\p{Cc}]/u.test(id) || !['codex', 'antigravity'].includes(provider) || !data || typeof data !== 'object' || Array.isArray(data) || !Array.isArray(data.files) || data.files.length > 10000 || data.files.some(account => !account || typeof account !== 'object' || Array.isArray(account))) return invalid();
    const matches = data.files.filter(account => account?.id === id);
    if (matches.length !== 1 || data.files.some(account => account?.id !== id && account?.name === id)) return invalid();
    const account = matches[0];
    if (account.provider !== provider || ['disabled', 'runtime_only'].some(name => account[name] !== undefined && account[name] !== false) || typeof account.auth_index !== 'string' || !account.auth_index.trim() || /[\p{Cc}]/u.test(account.auth_index)) return invalid();
    return {id, provider, index: account.auth_index};
  }
  function parseModelPhrases(text) {
    return text.split(/\r\n|\n|\r/).map(phrase => phrase.trim()).filter(Boolean);
  }
  function exportModelPhrases(phrases) { return phrases.length ? phrases.join('\n') + '\n' : ''; }
  function deduplicateModelPhrases(phrases) { return [...new Set(phrases)]; }
  function modelConfigDraft(config) { return {...config, generation_prompt: config.generation_prompt ?? '', phrases: config.phrases.join('\n')}; }
  function validateModelDraft(draft) {
    const errors = {}, config = {}, bytes = value => new TextEncoder().encode(value).length;
    for (const name of ['default_model', 'five_hour_model', 'weekly_model']) {
      config[name] = draft[name];
      if (typeof draft[name] !== 'string' || bytes(draft[name]) > 200 || /[\s\p{Cc}]/u.test(draft[name])) errors[name] = '模型标识最多 200 字节，不能包含空白或控制字符';
    }
    for (const name of ['default_prompt', 'five_hour_prompt', 'weekly_prompt', 'generation_prompt']) {
      config[name] = name === 'generation_prompt' && draft[name] === undefined ? '' : draft[name];
      if (typeof config[name] !== 'string' || bytes(config[name]) > 8192 || /[\x00-\x08\x0b\x0c\x0e-\x1f\x7f-\x9f]/u.test(config[name]) || name === 'default_prompt' && !config[name].trim()) errors[name] = '提示词最多 8192 字节，仅允许换行、回车和制表控制字符，默认提示词不能为空';
    }
    config.prompt_mode = draft.prompt_mode;
    if (!['fixed', 'random'].includes(draft.prompt_mode)) errors.prompt_mode = '请选择固定提示词或随机短句';
    config.phrases = typeof draft.phrases === 'string' ? parseModelPhrases(draft.phrases) : [];
    if (typeof draft.phrases !== 'string' || config.phrases.length > 200 || config.phrases.some(phrase => bytes(phrase) > 512 || /[\p{Cc}\u2028\u2029]/u.test(phrase))) errors.phrases = '短句最多 200 条，每条为不超过 512 字节的单行文本，不能包含控制字符';
    else if (new Set(config.phrases).size !== config.phrases.length) errors.phrases = '存在重复短句，请先检查或确认去重';
    else if (draft.prompt_mode === 'random' && !config.phrases.length) errors.phrases = '随机模式至少需要一条短句';
    if (Object.values(config).reduce((total, value) => total + (Array.isArray(value) ? value.reduce((sum, phrase) => sum + bytes(phrase), 0) : typeof value === 'string' ? bytes(value) : 0), 0) > 65536) errors.total = '配置文本总量不能超过 65536 字节';
    return {errors, config};
  }
  function modelPromptPreview(config, rule = 'default', crypto) {
    const prefix = {five_hour: 'five_hour', weekly: 'weekly'}[rule];
    const modelOverride = prefix && config[prefix + '_model'], promptOverride = prefix && config[prefix + '_prompt'];
    const preview = {model: modelOverride || config.default_model, prompt: promptOverride || config.default_prompt,
      modelInherited: !!prefix && !modelOverride, promptInherited: !!prefix && !promptOverride, mode: config.prompt_mode};
    if (config.prompt_mode !== 'random') return preview;
    if (!config.phrases.length) throw new Error('随机模式至少需要一条短句');
    if (typeof crypto?.getRandomValues !== 'function') throw new Error('浏览器缺少安全随机能力，无法预览随机短句');
    const bytes = new Uint8Array(4), limit = Math.floor(4294967296 / config.phrases.length) * config.phrases.length;
    for (let attempt = 0; attempt < 16; attempt++) {
      crypto.getRandomValues(bytes);
      const value = bytes[0] * 16777216 + bytes[1] * 65536 + bytes[2] * 256 + bytes[3];
      if (value < limit) return {...preview, prompt: config.phrases[value % config.phrases.length], promptInherited: false};
    }
    throw new Error('无法生成随机预览，请稍后重试');
  }
  Object.assign(codes, {invalid_wake_request: '手动唤醒参数无效，请检查 Codex 账号 ID', wake_model_missing: '请先独立保存实际可用的唤醒模型，再确认执行'});
  Object.assign(codes, {wake_settings_unavailable: '后台未提供唤醒设置能力', wake_unavailable: '后台未提供模型唤醒能力', invalid_wake_settings: '唤醒设置无效，请检查字段', wake_settings_conflict: '唤醒设置已在后台变化，请重新读取核实', invalid_wake_job: '手动唤醒参数无效', wake_account_unavailable: '唤醒账号不可用或绑定已变化', wake_model_unavailable: '请先保存实际可用的唤醒模型', wake_outcome_unknown: '唤醒结果未知，可能已消耗额度，禁止重发', wake_interrupted: '唤醒被中断，请核实原任务，禁止重发', wake_request_rejected: '模型请求未被接受', wake_quota_unknown: '额度证据未知，跳过模型请求', wake_active_window: '账号已有活动窗口，无需重复唤醒'});
  Object.assign(codes, {model_catalog_account_invalid: '账号身份、提供方或文件别名无法唯一核实，请刷新账号目录后重试', model_catalog_unavailable: 'CPA 未提供账号模型目录，可继续手动填写模型', model_catalog_changed: '账号目录或绑定已变化，请重新读取模型'});
  codes.invalid_model_settings = '模型与提示词设置无效，请检查字段';
  codes.generation_accounts_unavailable = '生成账号目录暂不可用，请稍后刷新核实';
  codes.generation_prompt_unavailable = '当前后台不支持自定义生成提示词，请升级后台后再试';
  codes.model_settings_conflict = '后台模型配置已变化，请重新读取并丢弃草稿后再编辑';
  Object.assign(codes, {prompt_generation_unavailable: '后台未提供候选生成功能', invalid_prompt_generation: '生成参数无效，请检查账号、模型和数量', generation_account_unavailable: '生成账号不可用或凭据绑定已变化，请检查账号状态', generation_response_invalid: '模型返回的候选短句无效', generation_request_rejected: '模型请求被拒绝', generation_interrupted: '生成被中断，可能已消耗额度', generation_outcome_unknown: '生成结果未知，可能已消耗额度', idempotency_key_required: '缺少有效的请求标识', job_id_required: '缺少有效的生成任务标识'});
  function message(code) { return typeof code === 'string' && Object.hasOwn(codes, code) ? codes[code] : '请求失败，请查看诊断信息'; }
  function taskDiagnostic(code) {
    const known = typeof code === 'string' && Object.hasOwn(codes, code);
    const safe = known ? code : 'task_error_unknown';
    let stage = '后台任务', advice = '查看 CPA 和插件后台同一时间的本地日志；核实原因后再手动操作。页面未保存上游原始响应。';
    if (/^bridge_|^host_callback_|^backend_/.test(safe)) { stage = 'CPA 本地连接'; advice = '检查 CPA 与独立后台服务、私有 socket 和接入层版本，不必通过模型请求测试连接。'; }
    else if (/^account_|^generation_account|^wake_account/.test(safe)) { stage = '账号身份与状态核实'; advice = '刷新 CPA 账号目录，核对账号是否删除、禁用、暂不可用或凭据绑定已变化；不会自动更换账号。'; }
    else if (safe === 'quota_authentication_failed' || safe === 'quota_credential_missing') { stage = '额度凭据认证'; advice = '在 CPA 中核实并更新该账号凭据。认证失败不等同于额度耗尽，也不会解除禁用。'; }
    else if (safe === 'quota_upstream_rate_limited') { stage = '额度接口 · HTTP 429'; advice = '等待上游限流解除，降低查询频率或增加随机延迟；不要连续提交相同批量任务。'; }
    else if (safe === 'quota_upstream_forbidden') { stage = '额度接口 · HTTP 403'; advice = '核对账号访问权限、区域及该账号代理配置；不能仅凭拒绝访问推断额度耗尽。'; }
    else if (/^quota_(transport|proxy|query_timeout)/.test(safe)) { stage = '额度请求连接'; advice = '检查该账号代理、网络、DNS 与 TLS 以及查询超时；不会回退其他账号或发送模型请求。'; }
    else if (/^quota_/.test(safe)) { stage = '额度接口与响应处理'; advice = '检查上游额度服务及本地日志。异常或无法识别的响应不会覆盖最后一次有效额度。'; }
    else if (known && /unknown|unconfirmed|interrupted/.test(safe)) { stage = '执行结果核实'; advice = '结果可能已产生或消耗额度；仅核实原任务，保留原请求标识，不要重发或创建替代模型任务。'; }
    else if (/^generation_|^wake_|^model_/.test(safe)) { stage = '模型任务与安全准入'; advice = '核对实际账号、已保存模型和提示词及任务状态。结果未知时仅核实原任务，不自动重发。'; }
    else if (safe === 'storage_unavailable') { stage = '后台持久化'; advice = '检查磁盘空间、目录权限和后台日志；不要删除数据库或发送防重记录来恢复执行。'; }
    return {code: safe, title: known ? message(code) : '未识别的任务错误', stage, advice};
  }
  function createToastCenter(document, clock) {
    const region = document.getElementById('toast-region'), entries = new Map();
    function dismiss(id, forget = false) {
      const entry = entries.get(id);
      if (!entry) return;
      clock.clearTimeout(entry.timer); entry.timer = null;
      entry.card.hidden = true; entry.content.hidden = true;
      entry.hovered = false; entry.focused = false;
      if (forget) entry.last = '';
    }
    function schedule(entry) {
      clock.clearTimeout(entry.timer); entry.timer = null;
      if (entry.card.hidden || entry.hovered || entry.focused || ['error', 'warning'].includes(entry.tone)) return;
      entry.timer = clock.setTimeout(() => dismiss(entry.id), entry.tone === 'success' ? 4500 : 6000);
    }
    function show(id, text, tone = 'info') {
      if (!text) { dismiss(id, true); return; }
      let entry = entries.get(id);
      if (!entry) {
        const content = document.getElementById(id), card = document.createElement('div'), close = document.createElement('button');
        if (!content || !region) return;
        card.className = 'toast'; card.dataset.channel = id;
        content.className = 'toast-text';
        close.type = 'button'; close.className = 'toast-close'; close.textContent = '×';
        close.setAttribute('aria-label', '关闭提示');
        entry = {id, card, content, close, last: '', tone, timer: null, hovered: false, focused: false};
        close.onclick = () => dismiss(id);
        card.addEventListener('mouseenter', () => { entry.hovered = true; schedule(entry); });
        card.addEventListener('mouseleave', () => { entry.hovered = false; schedule(entry); });
        card.addEventListener('focusin', () => { entry.focused = true; schedule(entry); });
        card.addEventListener('focusout', event => { entry.focused = card.contains(event.relatedTarget); schedule(entry); });
        card.append(content, close); region.append(card); entries.set(id, entry);
      }
      const signature = tone + ':' + text;
      if (entry.last === signature) return;
      entry.last = signature; entry.tone = tone;
      entry.card.dataset.tone = tone;
      entry.content.setAttribute('role', tone === 'error' ? 'alert' : 'status');
      entry.content.setAttribute('aria-atomic', 'true');
      entry.card.hidden = false; entry.content.hidden = false; entry.content.textContent = text;
      if (!entry.focused && !entry.hovered) region.append(entry.card);
      entries.delete(id); entries.set(id, entry);
      const visible = [...entries.values()].filter(item => !item.card.hidden);
      while (visible.length > 4) {
        const index = visible.findIndex(item => item !== entry && !item.focused && !item.hovered);
        if (index < 0) break;
        dismiss(visible.splice(index, 1)[0].id);
      }
      schedule(entry);
    }
    function clear() { for (const id of entries.keys()) dismiss(id, true); }
    return {show, dismiss, clear};
  }
  if (typeof module !== 'undefined' && module.exports) module.exports = {ViewTracker, apiBase, readPanelKey, age, resetText, windowView, accountStatus, jobCounts, requestID, validateQueryDraft, configDraft, queueText};
  if (typeof module !== 'undefined' && module.exports) Object.assign(module.exports, {modelDefaults, parseModelPhrases, exportModelPhrases, deduplicateModelPhrases, modelConfigDraft, validateModelDraft, modelPromptPreview});
  if (typeof module !== 'undefined' && module.exports) Object.assign(module.exports, {wakeDefaults, wakeConfigDraft, validateWakeDraft});
  if (typeof module !== 'undefined' && module.exports) Object.assign(module.exports, {resolveModelAccount, codexPromptPreset});
  if (typeof module !== 'undefined' && module.exports) Object.assign(module.exports, {createToastCenter});
  if (typeof module !== 'undefined' && module.exports) Object.assign(module.exports, {taskDiagnostic});
  if (typeof document === 'undefined') return;

  const context = globalThis, $ = id => document.getElementById(id);
  const toasts = createToastCenter(document, context);
  for (const id of ['error', 'notice', 'rules-message', 'wake-overview-message', 'settings-message', 'wake-settings-message', 'wake-manual-message', 'wake-history-message', 'model-message', 'generation-message']) $(id).hidden = true;
  let base;
  try { base = apiBase(context.location); } catch (error) { toasts.show('error', message(error.code), 'error'); return; }
  let key = '', generation = 0, views = new ViewTracker(), pollTimer;
  let connected = false, connecting = false, refreshing = false;
  let overviewData = null, accountsData = [], accountsRevision = 0, accountsViewRevision = -1, cursor = '', nextCursor = '', previousCursors = [], pageStart = 0, filteredTotal = null;
  let filterTimer, filterSequence = 0, updatingAccounts = false, syncing = false;
  let savedSettings = null, settingsBase = null, settingsLoading = false, settingsSaving = false, settingsConflict = false, activePage = 'overview';
  let settingsNeedsVerification = false, savingRule = null;
  let modelBase = null, savedModelSettings = null, modelLoading = false, modelPolling = false, modelSaving = false, modelConflict = false, modelNeedsVerification = false, modelConfirmation = '';
  const modelCatalogs = {wake: {sequence: 0, loading: false}, generation: {sequence: 0, loading: false}};
  let modelCatalogInstance = '';
  let generationIntent = null, generationTask = null, generationSubmitting = false, generationReading = false, generationUnconfirmed = false, generationUnknown = false;
  let generationCatalog = null, generationCatalogLoading = false, generationCatalogError = '', generationCatalogAttempt = '', generationCatalogSequence = 0, generationAccountIdentity = null;
  let savedWakeSettings = null, wakeBase = null, wakeLoading = false, wakePolling = false, wakeSaving = false, wakeConflict = false, wakeNeedsVerification = false, wakeSettingsAttempt = '', wakeSavingEnabled = null;
  let wakeIntent = null, wakeTask = null, wakeSubmitting = false, wakeReading = false, wakeUnconfirmed = false, wakeUnknown = false, wakeJobsLoading = false, wakeJobs = [];
  const wakeFields = {enabled: 'wake-enabled', fiveHour: 'wake-five-hour', weekly: 'wake-weekly', timezone: 'wake-timezone', delayMin: 'wake-delay-min', delayMax: 'wake-delay-max', allowed: 'wake-allowed', perUser: 'wake-per-user', maximum: 'wake-max-accounts', daily: 'wake-daily', dailyWindow: 'wake-daily-window', dailyTarget: 'wake-daily-target'};
  const wakePhases = {queued: '等待执行', checking: '查询并核实额度', prepared: '已准备', sending: '正在发送模型请求', verifying: '核实执行结果', completed: '已完成', skipped: '已跳过', failed: '执行失败', unknown: '结果未知', cancelled: '已取消'};
  const wakeReasons = {manual: '手动唤醒', manual_single: '手动唤醒', five_hour_reset: '五小时重置', weekly_reset: '周额度重置', startup: '启动分散唤醒', daily: '每日容量准备', daily_capacity: '每日容量准备'};
  const generationSelected = new Set();
  const modelFields = {default_model: 'model-default', default_prompt: 'model-default-prompt', five_hour_model: 'model-five-hour', five_hour_prompt: 'model-five-hour-prompt', weekly_model: 'model-weekly', weekly_prompt: 'model-weekly-prompt', generation_prompt: 'generation-prompt', prompt_mode: 'model-prompt-mode', phrases: 'model-phrases'};
  const settingsFields = {automatic: 'settings-automatic', fiveHour: 'settings-five-hour', weekly: 'settings-weekly', interval: 'settings-interval', timezone: 'settings-timezone', delayMin: 'settings-delay-min', delayMax: 'settings-delay-max', excluded: 'settings-excluded'};
  const pending = new Map(), controllers = new Set(), queryBusy = new Set(), unresolved = new Map();
  const errorLimits = new Map();
  const setText = (id, text) => { if ($(id) && $(id).textContent !== String(text)) $(id).textContent = text; };
  function updateChildren(parent, children) {
    const previous = Array.from(parent.childNodes);
    const keyed = new Map(previous.filter(node => node.dataset?.renderKey).map(node => [node.dataset.renderKey, node]));
    const used = new Set();
    const compatible = (before, after) => before && before.nodeType === after.nodeType && before.tagName === after.tagName;
    const desired = children.map((next, index) => {
      const key = next.dataset?.renderKey;
      const current = key ? keyed.get(key) : previous[index];
      if (!compatible(current, next) || used.has(current) || !key && current.dataset?.renderKey) return next;
      used.add(current);
      if (current.nodeType === 3) {
        if (current.textContent !== next.textContent) current.textContent = next.textContent;
        return current;
      }
      for (const name of current.getAttributeNames()) if (!next.hasAttribute(name) && !(name === 'open' && current.tagName === 'DETAILS')) current.removeAttribute(name);
      for (const name of next.getAttributeNames()) if (current.getAttribute(name) !== next.getAttribute(name)) current.setAttribute(name, next.getAttribute(name));
      for (const name of ['className', 'title', 'disabled', 'checked', 'value', 'max', 'type', 'colSpan']) {
        if (name in next && current[name] !== next[name]) current[name] = next[name];
      }
      for (const name of Object.keys(current.dataset)) if (!Object.hasOwn(next.dataset, name)) delete current.dataset[name];
      Object.assign(current.dataset, next.dataset);
      current.onclick = next.onclick; current.onchange = next.onchange;
      if (next.childNodes.length) updateChildren(current, Array.from(next.childNodes));
      else if (current.textContent !== next.textContent) current.textContent = next.textContent;
      return current;
    });
    for (let index = 0; index < desired.length; index++) {
      if (parent.childNodes[index] !== desired[index]) parent.insertBefore(desired[index], parent.childNodes[index] || null);
    }
    for (const node of Array.from(parent.childNodes)) if (!desired.includes(node)) parent.removeChild(node);
  }
  const sidebarPreference = 'cwr-v2-sidebar:' + base;
  function renderSidebar(collapsed, persist = false) {
    $('app-sidebar').dataset.collapsed = String(collapsed);
    document.documentElement?.classList.toggle('sidebar-collapsed', collapsed);
    const label = collapsed ? '展开侧栏' : '收起侧栏';
    $('sidebar-toggle').setAttribute('aria-expanded', String(!collapsed));
    $('sidebar-toggle').setAttribute('aria-label', label); $('sidebar-toggle').setAttribute('title', label);
    setText('sidebar-toggle-label', label);
    if (persist) { try { context.localStorage.setItem(sidebarPreference, String(collapsed)); } catch {} }
  }
  function element(tag, className, text) {
    const item = document.createElement(tag);
    if (className) item.className = className;
    if (text !== undefined) item.textContent = text;
    return item;
  }
  function taskErrorDetails(code, job, account = '', kind = 'query') {
    const diagnostic = taskDiagnostic(code), details = element('details', 'task-error-details');
    details.dataset.renderKey = 'error:' + account + ':' + diagnostic.code;
    details.append(element('summary', '', '错误详情 · ' + diagnostic.title));
    const metadata = element('dl', 'task-error-metadata');
    const phases = kind === 'wake' ? wakePhases : kind === 'generation' ? {queued: '等待生成', running: '正在生成', completed: '生成完成', failed: '生成失败', unknown: '结果未知'} : phaseNames;
    for (const [label, value] of [['任务 ID', job.job_id || job.id || '未知'], ['账号', account || job.account_id || job.target || '批量任务'], ['任务阶段', phases[job.phase] || '未知'], ['错误代码', diagnostic.code], ['失败环节', diagnostic.stage], ['记录时间', job.updated_at || job.created_at || '未提供']]) {
      metadata.append(element('dt', '', label), element('dd', '', value));
    }
    details.append(metadata, element('p', 'task-error-advice', diagnostic.advice), element('p', 'muted', '仅展示脱敏分类与排查建议，不包含凭据、代理密钥、请求提示词或上游原始响应。'));
    return details;
  }
  function report(error) {
    feedback('error', message(error.code || error.message) + '。已显示的数据可能已过期。', 'error');
  }
  function feedback(id, text, tone = 'info') { toasts.show(id, text, tone); }
  function rememberFeedback(id, text) { toasts.dismiss(id, true); setText(id, text); $(id).hidden = true; }
  function notice(text, tone = 'success') { feedback('notice', text, tone); }
  function clearError() { toasts.dismiss('error', true); $('error').hidden = true; $('error').textContent = ''; }
  function ruleMessage(text, tone = 'info') { feedback('rules-message', text, tone); }
  function settingsSupported() { return overviewData?.capabilities?.query_settings === true; }
  function readDraft() {
    return Object.fromEntries(Object.entries(settingsFields).map(([name, id]) => [name, ['automatic', 'fiveHour', 'weekly'].includes(name) ? $(id).checked === true : $(id).value]));
  }
  function draftDirty() { return !!settingsBase && JSON.stringify(readDraft()) !== JSON.stringify(configDraft(settingsBase.config)); }
  function fillDraft(data) {
    const draft = configDraft(data.config);
    for (const [name, id] of Object.entries(settingsFields)) {
      if (typeof draft[name] === 'boolean') $(id).checked = draft[name]; else if ($(id).value !== draft[name]) $(id).value = draft[name];
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
    $('settings-save').disabled = !connected || !supported || !dirty || !!Object.keys(errors).length || settingsConflict || settingsNeedsVerification || settingsSaving || settingsLoading;
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
      $(id).checked = savingRule?.field === name ? savingRule.checked : savedSettings?.config?.[name] === true;
      $(id).disabled = !connected || !supported || !savedSettings || settingsNeedsVerification || settingsSaving || settingsLoading;
      $(id).setAttribute('aria-busy', savingRule?.field === name ? 'true' : 'false');
    }
    renderWakeSettings();
    $('rules-reload').hidden = !connected || !supported || !settingsNeedsVerification && !!savedSettings;
    $('rules-reload').disabled = !connected || settingsSaving || settingsLoading;
    setText('rules-status', !connected || !supported ? '尚未接入' : settingsSaving ? '保存中…' : settingsLoading ? '读取中…' : settingsNeedsVerification ? '需要核实' : '即改即存');
    setText('settings-save', settingsSaving ? '保存中…' : '保存并应用');
  }
  async function reloadSettings(discard = false) {
    if (!connected || !settingsSupported() || settingsLoading || settingsSaving) return false;
    const current = generation, sequence = views.begin('settings'), verifying = settingsNeedsVerification; settingsLoading = true; renderSettings();
    try {
      const data = await api('/settings');
      if (current !== generation || !views.accept('settings', sequence, data)) return false;
      if (!applySettings(data, discard)) return false;
      settingsNeedsVerification = false;
      if (verifying) ruleMessage('已重新读取后台设置，请核对开关状态。未重复提交任何更改。', 'success');
      const text = discard ? '已重新加载后台设置并丢弃草稿，尚未写入任何更改。' : '已读取后台设置；修改后请预览差异并保存。';
      if (discard && !verifying) feedback('settings-message', text, 'success'); else rememberFeedback('settings-message', text);
      return true;
    } catch (error) { if (current === generation) { rememberFeedback('settings-message', message(error.code)); ruleMessage('读取设置失败：' + message(error.code), 'error'); } return false; }
    finally { if (current === generation) { settingsLoading = false; renderSettings(); } }
  }
  async function saveSettings() {
    if (!connected || !settingsSupported() || settingsSaving || settingsLoading || settingsConflict || settingsNeedsVerification || !draftDirty()) return;
    if (settingsBase.instance_id !== views.instance) { settingsConflict = true; renderSettings(); return; }
    const validated = validateQueryDraft(readDraft());
    if (Object.keys(validated.errors).length) { renderSettings(); return; }
    const current = generation, sequence = views.begin('settings');
    settingsSaving = true; renderSettings(); feedback('settings-message', '正在保存设置…');
    try {
      const data = await api('/settings', {method: 'POST', body: JSON.stringify({revision: settingsBase.revision, config: validated.config})});
      if (current !== generation) return;
      if (!views.accept('settings', sequence, data)) throw Object.assign(new Error('settings_unconfirmed'), {code: 'settings_unconfirmed'});
      if (!applySettings(data, true)) throw Object.assign(new Error('settings_conflict'), {code: 'settings_conflict'});
      views.barrier(); feedback('settings-message', '已保存并应用。后台将按此规则持续执行额度查询，不发送模型请求。', 'success');
    } catch (error) {
      if (current !== generation) return;
      if (['settings_conflict', 'settings_unconfirmed', 'network', 'timeout', 'invalid_response'].includes(error.code)) {
        settingsConflict = true; settingsNeedsVerification = true;
        rememberFeedback('rules-message', '查询设置保存失败或结果未确认，请先重新读取后台设置。');
      }
      feedback('settings-message', message(['network', 'timeout', 'invalid_response'].includes(error.code) ? 'settings_unconfirmed' : error.code), 'error');
    } finally { if (current === generation) { settingsSaving = false; renderSettings(); } }
  }
  async function toggleRule(name, checked) {
    const field = {fiveHour: 'on_five_hour_expiry', weekly: 'on_weekly_expiry'}[name];
    if (!field || !connected || !settingsSupported() || !savedSettings || settingsSaving || settingsLoading || settingsNeedsVerification) { renderSettings(); return; }
    const basis = savedSettings;
    if (basis.instance_id !== views.instance) { settingsNeedsVerification = true; ruleMessage('后台已重启，请重新读取设置后再调整开关。'); renderSettings(); return; }
    if (basis.config[field] === checked) { renderSettings(); return; }
    const current = generation, sequence = views.begin('settings'), draft = readDraft();
    const rebase = !settingsConflict && settingsBase?.instance_id === basis.instance_id && settingsBase.revision === basis.revision;
    const original = settingsBase ? configDraft(settingsBase.config) : null;
    const config = {...basis.config, [field]: checked}, label = name === 'fiveHour' ? '5 小时到期查询' : '周限额到期查询';
    settingsSaving = true; savingRule = {field, checked}; renderSettings(); ruleMessage('正在保存' + label + '…');
    try {
      const data = await api('/settings', {method: 'POST', body: JSON.stringify({revision: basis.revision, config})});
      if (current !== generation) return;
      if (data.instance_id !== basis.instance_id || data.config?.[field] !== checked || data.revision <= basis.revision || !views.accept('settings', sequence, data)) throw Object.assign(new Error('settings_unconfirmed'), {code: 'settings_unconfirmed'});
      if (!applySettings(data, rebase)) throw Object.assign(new Error('settings_conflict'), {code: 'settings_conflict'});
      if (rebase) {
        for (const [name, id] of Object.entries(settingsFields)) {
          if (draft[name] === original[name]) continue;
          if (typeof draft[name] === 'boolean') $(id).checked = draft[name]; else $(id).value = draft[name];
        }
      }
      views.barrier(); ruleMessage(label + '已' + (checked ? '开启' : '关闭') + '并保存。', 'success');
      rememberFeedback('settings-message', draftDirty() ? '总览开关已保存；编辑器中的其他修改仍为草稿，尚未应用。' : '总览开关已保存并应用。');
    } catch (error) {
      if (current !== generation) return;
      const uncertain = ['settings_conflict', 'settings_unconfirmed', 'network', 'timeout', 'invalid_response'].includes(error.code);
      if (uncertain) settingsNeedsVerification = true;
      ruleMessage(error.code === 'settings_conflict' ? '后台设置已变化，本次开关未保存。请重新读取后再调整。' : uncertain ? '保存结果尚未确认，已恢复最后已知状态。请重新读取核实，不要重复提交。' : '开关未保存：' + message(error.code), 'error');
    } finally { if (current === generation) { settingsSaving = false; savingRule = null; renderSettings(); } }
  }
  function wakeSettingsSupported() { return overviewData?.capabilities?.wake_settings === true; }
  function wakeExecutionSupported() { return overviewData?.capabilities?.model_execution === true; }
  function automaticWakeSupported() { return wakeSettingsSupported() && wakeExecutionSupported() && overviewData?.capabilities?.automatic_wake === true; }
  function readWakeDraft() { return Object.fromEntries(Object.entries(wakeFields).map(([name, id]) => [name, ['enabled', 'fiveHour', 'weekly', 'daily'].includes(name) ? $(id).checked : $(id).value])); }
  function wakeDirty() {
    if (!wakeBase) return false;
    const original = wakeConfigDraft(wakeBase.config), draft = readWakeDraft();
    return Object.keys(wakeFields).some(name => original[name] !== draft[name]);
  }
  function fillWakeDraft(data) {
    const draft = wakeConfigDraft(data.config);
    for (const [name, id] of Object.entries(wakeFields)) {
      if (typeof draft[name] === 'boolean') $(id).checked = draft[name]; else if ($(id).value !== draft[name]) $(id).value = draft[name];
    }
    wakeBase = data;
  }
  function applyWakeSettings(data, discard = false) {
    const config = data?.config;
    if (!Number.isSafeInteger(data?.revision) || data.revision < 0 || !config || typeof config.timezone !== 'string' || typeof config.daily_window !== 'string' ||
      !Array.isArray(config.allowed_hours) || config.allowed_hours.some(span => typeof span !== 'string') || !config.random_delay ||
      ['enabled', 'on_five_hour_reset', 'on_weekly_reset', 'daily_enabled'].some(name => typeof config[name] !== 'boolean') ||
      ['accounts_per_user', 'max_accounts', 'daily_target'].some(name => typeof config[name] !== 'number') ||
      ['min_seconds', 'max_seconds'].some(name => typeof config.random_delay[name] !== 'number') || Object.keys(validateWakeDraft(wakeConfigDraft(config)).errors).length) throw Object.assign(new Error('invalid_response'), {code: 'invalid_response'});
    if (savedWakeSettings?.instance_id === data.instance_id && savedWakeSettings.revision > data.revision) return false;
    const dirty = wakeDirty();
    if (savedWakeSettings?.instance_id !== data.instance_id || savedWakeSettings.revision !== data.revision) { $('wake-overview-consent').checked = false; $('wake-settings-consent').checked = false; }
    savedWakeSettings = data;
    if (discard || !dirty) { fillWakeDraft(data); wakeConflict = false; if (discard) $('wake-settings-consent').checked = false; }
    else if (wakeBase.instance_id !== data.instance_id || wakeBase.revision !== data.revision) wakeConflict = true;
    renderWakeSettings(); return true;
  }
  function wakeOverviewMessage(text, tone = 'info') { feedback('wake-overview-message', text, tone); }
  function renderWakeSettings() {
    const supported = wakeSettingsSupported(), dirty = wakeDirty(), draft = readWakeDraft(), errors = wakeBase ? validateWakeDraft(draft).errors : {};
    const locked = !connected || !supported || !wakeBase || wakeLoading || wakeSaving;
    $('wake-settings-fields').disabled = locked;
    $('wake-enabled').disabled = locked || !automaticWakeSupported();
    $('wake-settings-consent').disabled = locked || !automaticWakeSupported();
    $('wake-settings-save').disabled = locked || !dirty || !!Object.keys(errors).length || wakeConflict || wakeNeedsVerification || draft.enabled && (!automaticWakeSupported() || !$('wake-settings-consent').checked);
    $('wake-settings-reload').disabled = !connected || !supported || wakeLoading || wakeSaving;
    setText('wake-settings-status', !connected ? '等待连接' : !supported ? '尚未接入' : wakeSaving ? '保存中' : wakeLoading ? '读取中' : wakeNeedsVerification ? '需要核实' : wakeConflict ? '设置冲突' : dirty ? '未保存草稿' : savedWakeSettings ? '版本 ' + savedWakeSettings.revision : '尚未读取');
    setText('wake-settings-validation', Object.values(errors).join('；')); $('wake-settings-validation').hidden = !Object.keys(errors).length;
    $('wake-settings-conflict').hidden = !wakeConflict && !wakeNeedsVerification;
    for (const [name, id] of Object.entries(wakeFields)) $(id).setAttribute('aria-invalid', errors[name] ? 'true' : 'false');
    const diff = $('wake-settings-diff'); diff.replaceChildren();
    if (wakeBase) {
      const original = wakeConfigDraft(wakeBase.config), labels = {enabled: '自动唤醒', fiveHour: '五小时重置', weekly: '周重置', timezone: '时区', delayMin: '最小启动延迟', delayMax: '最大启动延迟', allowed: '普通允许时段', perUser: '每用户账号数', maximum: '账号上限', daily: '每日准备', dailyWindow: '每日准备窗口', dailyTarget: '每日目标容量'};
      const display = value => typeof value === 'boolean' ? value ? '开启' : '关闭' : value || '全天';
      for (const name of Object.keys(wakeFields)) if (original[name] !== draft[name]) {
        const row = element('div', 'settings-diff-row'); row.append(element('strong', '', labels[name]), element('span', '', display(original[name]) + ' → ' + display(draft[name]))); diff.append(row);
      }
      if (!dirty) diff.append(element('p', '', '暂无修改。只有明确保存才会更新后台规则。'));
    } else diff.append(element('p', '', supported ? '连接后读取真实后台规则。' : '当前后台未提供唤醒规则，不会发送模型请求。'));
    const current = savedWakeSettings?.instance_id === views.instance;
    $('auto-wake').checked = wakeSavingEnabled === null ? current && savedWakeSettings.config.enabled === true : wakeSavingEnabled;
    $('auto-wake').disabled = !connected || !automaticWakeSupported() || !current || wakeLoading || wakeSaving || wakeNeedsVerification;
    $('auto-wake').setAttribute('aria-busy', wakeSavingEnabled === null ? 'false' : 'true');
    $('wake-overview-consent-row').hidden = !automaticWakeSupported();
    $('wake-overview-consent').disabled = !connected || !automaticWakeSupported() || !current || wakeLoading || wakeSaving || wakeNeedsVerification;
    $('wake-overview-reload').hidden = !connected || !supported || !wakeNeedsVerification && !!current;
    $('wake-overview-reload').disabled = !connected || !supported || wakeLoading || wakeSaving;
    if (wakeSaving) setText('wake-settings-save', '保存中…'); else setText('wake-settings-save', '保存唤醒规则');
    renderManualWake();
  }
  async function reloadWakeSettings(discard = false, explicit = false) {
    if (!connected || !wakeSettingsSupported() || wakeLoading || wakePolling || wakeSaving || wakeNeedsVerification && !explicit) return false;
    const current = generation, sequence = views.begin('wake-settings');
    wakeLoading = discard || explicit || !wakeBase; wakePolling = !wakeLoading;
    wakeSettingsAttempt = views.instance; renderWakeSettings();
    try {
      const data = await api('/wake-settings');
      if (current !== generation || !views.accept('wake-settings', sequence, data) || !applyWakeSettings(data, discard)) return false;
      if (explicit) {
        wakeNeedsVerification = false;
        if (discard) { toasts.dismiss('wake-overview-message', true); feedback('wake-settings-message', '已重新读取并丢弃唤醒草稿，没有重发模型请求。', 'success'); }
        else wakeOverviewMessage('已读取真实后台唤醒设置；未重发保存或模型请求。', 'success');
      }
      return true;
    } catch (error) {
      if (current === generation) { rememberFeedback('wake-settings-message', '读取唤醒设置失败：' + message(error.code)); wakeOverviewMessage('读取唤醒设置失败：' + message(error.code), 'error'); }
      return false;
    } finally { if (current === generation) { wakeLoading = false; wakePolling = false; renderWakeSettings(); } }
  }
  function ensureWakeSettings() { if (wakeSettingsSupported() && wakeSettingsAttempt !== views.instance) void reloadWakeSettings(); }
  async function saveWakeSettings(direct = false, checked = false) {
    if (!connected || !wakeSettingsSupported() || wakeLoading || wakeSaving || wakeNeedsVerification) { renderWakeSettings(); return; }
    const basis = direct ? savedWakeSettings : wakeBase;
    if (!basis || !direct && (wakeConflict || !wakeDirty())) { renderWakeSettings(); return; }
    const validated = validateWakeDraft(direct ? wakeConfigDraft({...basis.config, enabled: checked}) : readWakeDraft());
    if (Object.keys(validated.errors).length) {
      if (direct) wakeOverviewMessage(Object.values(validated.errors).join('；') + '。请先编辑唤醒规则并明确保存。', 'error');
      renderWakeSettings(); return;
    }
    if (validated.config.enabled && (!automaticWakeSupported() || !$(direct ? 'wake-overview-consent' : 'wake-settings-consent').checked)) {
      if (direct) wakeOverviewMessage('开启自动唤醒前，请先明确确认后台持续消耗模型额度。', 'warning');
      else feedback('wake-settings-message', '请明确确认开启后后台可能持续消耗模型额度，再保存。', 'warning');
      renderWakeSettings(); return;
    }
    if (basis.instance_id !== views.instance) { wakeNeedsVerification = true; wakeOverviewMessage('后台实例已变化，请重新读取唤醒设置后再保存。', 'warning'); renderWakeSettings(); return; }
    if (direct && basis.config.enabled === checked) { renderWakeSettings(); return; }
    const current = generation, sequence = views.begin('wake-settings'), draft = readWakeDraft(), original = wakeBase ? wakeConfigDraft(wakeBase.config) : null;
    const rebase = direct && !wakeConflict && wakeBase?.instance_id === basis.instance_id && wakeBase.revision === basis.revision;
    wakeSaving = true; wakeSavingEnabled = direct ? checked : null; renderWakeSettings();
    const saving = direct ? '正在保存自动唤醒开关…' : '正在保存后台唤醒规则…';
    if (direct) wakeOverviewMessage(saving); else feedback('wake-settings-message', saving);
    try {
      const data = await api('/wake-settings', {method: 'POST', body: JSON.stringify({revision: basis.revision, config: validated.config})});
      if (current !== generation) return;
      if (data.instance_id !== basis.instance_id || views.instance !== basis.instance_id || !Number.isSafeInteger(data.revision) || data.revision < basis.revision || !data.config || !data.config.random_delay || !Array.isArray(data.config.allowed_hours) ||
        !views.accept('wake-settings', sequence, data) || JSON.stringify(wakeConfigDraft(data.config)) !== JSON.stringify(wakeConfigDraft(validated.config))) throw Object.assign(new Error('settings_unconfirmed'), {code: 'settings_unconfirmed'});
      if (!applyWakeSettings(data, !direct || rebase)) throw Object.assign(new Error('wake_settings_conflict'), {code: 'wake_settings_conflict'});
      if (rebase) for (const [name, id] of Object.entries(wakeFields)) if (draft[name] !== original[name]) {
        if (typeof draft[name] === 'boolean') $(id).checked = draft[name]; else $(id).value = draft[name];
      }
      views.barrier(); $('wake-settings-consent').checked = false; $('wake-overview-consent').checked = false;
      const text = wakeDirty() ? '总览开关已保存；编辑器中的其他修改仍是未保存草稿。' : '唤醒规则已保存；后台按已保存模型、提示词和规则执行。';
      if (direct) { rememberFeedback('wake-settings-message', text); wakeOverviewMessage('自动唤醒已' + (checked ? '开启' : '关闭') + '并保存；其他页面草稿未提交。', 'success'); }
      else feedback('wake-settings-message', text, 'success');
    } catch (error) {
      if (current !== generation) return;
      if (['wake_settings_conflict', 'settings_unconfirmed', 'network', 'timeout', 'invalid_response'].includes(error.code) || error.status >= 500) { wakeNeedsVerification = true; wakeConflict = true; }
      const text = wakeNeedsVerification ? '唤醒设置已冲突或保存结果尚未确认。保留草稿与最后已知开关，请显式重新读取核实，不会自动重试。' : '唤醒设置未保存：' + message(error.code);
      if (direct) { rememberFeedback('wake-settings-message', text); wakeOverviewMessage(text, 'error'); }
      else feedback('wake-settings-message', text, 'error');
    } finally { if (current === generation) { wakeSaving = false; wakeSavingEnabled = null; renderWakeSettings(); } }
  }
  function wakeActive() { return wakeTask && !['completed', 'skipped', 'failed', 'unknown', 'cancelled'].includes(wakeTask.phase); }
  function wakeHoldText(reason) {
    return {quota_pending: '等待额度查询', quota_unknown: '额度证据未知，暂不发送', quota_blocked: '额度受限', active_window: '已有活动窗口，防止重复唤醒', weekly_reset_conflict: '周重置状态冲突', activity_unknown: '业务活动证据未知', account_busy: '账号有其他执行任务', capacity_satisfied: '目标容量已满足', outside_allowed_hours: '尚未进入允许时段', model_busy: '模型执行资源繁忙', model_missing: '尚未保存可用唤醒模型', verification_pending: '等待结果核实'}[reason] || (reason ? '后台暂缓执行，请查看诊断' : '');
  }
  function renderWakeHistory() {
    const merged = new Map(wakeJobs.map(job => [job.job_id, job])); if (wakeTask) merged.set(wakeTask.job_id, wakeTask);
    const jobs = [...merged.values()].sort((first, second) => (timestamp(second.created_at) || 0) - (timestamp(first.created_at) || 0));
    const items = jobs.slice(0, 20).map(job => {
      const item = element('article', 'task-item'), heading = element('div', 'task-heading');
      item.dataset.renderKey = 'wake:' + job.job_id;
      heading.append(element('strong', '', job.account_id || '指定账号'), badge(wakePhases[job.phase] || '状态未知', ['failed', 'unknown'].includes(job.phase) ? 'attention' : 'available'));
      item.append(heading, element('p', 'task-description', wakeReasons[job.reason] || '后台唤醒任务'));
      if (job.scheduled_at) item.append(element('small', 'task-description', '后台计划：' + (Number.isFinite(timestamp(job.scheduled_at)) ? new Date(job.scheduled_at).toLocaleString('zh-CN', {hour12: false}) : '待核实')));
      if (job.hold_reason) item.append(element('p', 'task-description', wakeHoldText(job.hold_reason)));
      if (['failed', 'unknown'].includes(job.phase) || job.error_code && ['skipped', 'cancelled'].includes(job.phase)) item.append(taskErrorDetails(job.error_code || (job.phase === 'unknown' ? 'wake_outcome_unknown' : 'wake_request_rejected'), job, '', 'wake'));
      if (job.phase === 'unknown') item.append(element('p', 'error-text', '可能已消耗额度；禁止重发，仅可读取原任务。'));
      const time = element('small', 'task-time', age(job.updated_at || job.created_at)); time.dataset.observedAt = job.updated_at || job.created_at || ''; item.append(time);
      const button = element('button', 'text-button', '读取任务结果'); button.dataset.renderKey = 'read'; button.type = 'button'; button.disabled = !connected || !wakeExecutionSupported(); button.onclick = () => readWakeJob(job.job_id); item.append(button); return item;
    });
    updateChildren($('wake-history'), items.length ? items : [element('p', 'muted', wakeExecutionSupported() ? '暂无唤醒任务；读取页面不会创建任务。' : '当前后台尚未接入模型唤醒。')]);
  }
  function renderManualWake() {
    const supported = wakeExecutionSupported(), locked = !connected || !supported || wakeSubmitting || wakeActive() || wakeUnconfirmed || wakeUnknown;
    $('wake-manual-fields').disabled = locked;
    $('wake-manual-submit').disabled = locked || wakeReading || !$('wake-manual-consent').checked;
    $('wake-manual-verify').hidden = !wakeUnconfirmed && !wakeUnknown;
    $('wake-manual-verify').disabled = !connected || !supported || wakeSubmitting || wakeReading || wakeUnknown && !wakeTask?.job_id;
    $('wake-history-refresh').disabled = !connected || !supported || wakeJobsLoading;
    setText('wake-manual-status', !connected ? '等待连接' : !supported ? '尚未接入' : wakeSubmitting ? '提交中' : wakeUnconfirmed ? '需要核实' : wakeUnknown ? '结果未知' : wakePhases[wakeTask?.phase] || '按需唤醒');
    if (!supported) setText('wake-manual-message', '当前后台未提供真实模型唤醒能力，不会提交模型请求。');
    renderWakeHistory();
  }
  function validateWakeJob(data, expectedID = '') {
    if (!data || typeof data.job_id !== 'string' || !data.job_id || expectedID && data.job_id !== expectedID || !Object.hasOwn(wakePhases, data.phase) ||
      ['account_id', 'reason', 'error_code', 'hold_reason', 'scheduled_at', 'created_at', 'updated_at'].some(name => data[name] !== undefined && typeof data[name] !== 'string')) throw Object.assign(new Error('invalid_response'), {code: 'invalid_response'});
  }
  function acceptWakeTask(data) {
    validateWakeJob(data);
    if (data.instance_id !== views.instance) throw Object.assign(new Error('invalid_response'), {code: 'invalid_response'});
    if (wakeTask?.job_id === data.job_id && wakeTask.instance_id === data.instance_id && Number.isSafeInteger(wakeTask.view_revision) && Number.isSafeInteger(data.view_revision) && data.view_revision < wakeTask.view_revision) return;
    wakeTask = data; wakeUnconfirmed = false; wakeUnknown = data.phase === 'unknown';
    if (['completed', 'skipped', 'failed', 'cancelled'].includes(data.phase)) $('wake-manual-consent').checked = false;
    feedback('wake-manual-message', wakeUnknown ? '唤醒结果未知，可能已消耗额度。不会重发；只能核实已有任务。' : data.phase === 'completed' ? '唤醒任务已完成；请以后台核实的额度和窗口状态为准。' : data.phase === 'skipped' ? '后台已跳过此次唤醒：' + (wakeHoldText(data.hold_reason) || '不满足安全执行条件') : data.phase === 'failed' ? '唤醒失败：' + message(data.error_code) : data.phase === 'cancelled' ? '唤醒任务已取消，未开始的工作不会继续发送。' : '任务已接受：' + wakePhases[data.phase] + '。页面只读取进度，不会再次提交模型请求。', wakeUnknown ? 'warning' : data.phase === 'failed' ? 'error' : data.phase === 'completed' ? 'success' : 'info');
  }
  async function readWakeJob(id = wakeTask?.job_id) {
    if (!connected || !wakeExecutionSupported() || !id || wakeReading || wakeSubmitting) return;
    const current = generation, sequence = views.begin('wake-job:' + id); wakeReading = true; renderManualWake();
    try {
      const data = await api('/wake-jobs?job_id=' + encodeURIComponent(id));
      if (current !== generation || !views.accept('wake-job:' + id, sequence, data)) return;
      validateWakeJob(data, id);
      wakeJobs = [data, ...wakeJobs.filter(job => job.job_id !== id)].slice(0, 20);
      if (wakeTask?.job_id === id) acceptWakeTask(data);
      else if (data.phase === 'unknown') feedback('wake-history-message', '该任务结果未知，可能已消耗额度。读取不会重发模型请求。', 'warning');
    } catch (error) {
      if (current !== generation) return;
      if (wakeTask?.job_id === id && [404, 410].includes(error.status)) { wakeUnknown = true; wakeTask = {...wakeTask, phase: 'unknown'}; feedback('wake-manual-message', '无法核实原唤醒任务，可能已消耗额度。禁止重发或新建替代请求。', 'warning'); }
      else feedback('wake-history-message', '读取唤醒任务失败：' + message(error.code) + '；只重试读取，不重发模型请求。', 'error');
    } finally { if (current === generation) { wakeReading = false; renderManualWake(); } }
  }
  async function loadWakeJobs(explicit = false) {
    if (!connected || !wakeExecutionSupported() || wakeJobsLoading) return;
    const current = generation, sequence = views.begin('wake-jobs'); wakeJobsLoading = true; renderManualWake();
    try {
      const data = await api('/wake-jobs?limit=20');
      if (current !== generation || !views.accept('wake-jobs', sequence, data)) return;
      if (!Array.isArray(data.jobs) || data.jobs.length > 20) throw Object.assign(new Error('invalid_response'), {code: 'invalid_response'});
      const ids = new Set(); for (const job of data.jobs) { validateWakeJob(job); if (ids.has(job.job_id)) throw Object.assign(new Error('invalid_response'), {code: 'invalid_response'}); ids.add(job.job_id); }
      wakeJobs = data.jobs;
      const own = wakeJobs.find(job => job.job_id === wakeTask?.job_id); if (own) acceptWakeTask({...own, instance_id: data.instance_id, view_revision: data.view_revision});
      if (explicit) feedback('wake-history-message', '显示最近 ' + data.jobs.length + ' 条持久任务。刷新只读取状态，不发送模型请求。', 'success');
    } catch (error) { if (current === generation) feedback('wake-history-message', '读取唤醒历史失败：' + message(error.code), 'error'); }
    finally { if (current === generation) { wakeJobsLoading = false; renderManualWake(); } }
  }
  async function submitManualWake(verify = false) {
    if (!connected || !wakeExecutionSupported() || wakeSubmitting || wakeReading) return;
    if (verify && wakeTask?.job_id) return readWakeJob();
    if (wakeUnknown || (verify ? !wakeUnconfirmed || !wakeIntent : wakeUnconfirmed || wakeActive() || !$('wake-manual-consent').checked)) return;
    if (!verify) {
      const account = $('wake-account').value.trim();
      if (!account || new TextEncoder().encode(account).length > 256 || /[\p{Cc}]/u.test(account)) { feedback('wake-manual-message', '请输入完整有效的 Codex 账号 ID，并确认可能消耗模型额度。', 'error'); return; }
      try { wakeIntent = {key: requestID(context.crypto), instance: views.instance, body: JSON.stringify({account_id: account})}; }
      catch (error) { feedback('wake-manual-message', message(error.code), 'error'); return; }
      wakeTask = null;
    }
    if (wakeIntent.instance !== views.instance) { wakeUnknown = true; feedback('wake-manual-message', '后台实例已更换，原唤醒可能已消耗额度。禁止向新实例重发请求。', 'warning'); renderManualWake(); return; }
    const current = generation; wakeSubmitting = true; renderManualWake();
    feedback('wake-manual-message', verify ? '使用原标识与原账号核实同一唤醒任务，不创建替代请求…' : '提交一次明确确认的手动唤醒，后台将先查询额度并检查安全条件…');
    try {
      const data = await api('/wake-jobs', {method: 'POST', headers: {'Idempotency-Key': wakeIntent.key}, body: wakeIntent.body});
      if (current !== generation) return;
      acceptWakeTask({...data, phase: data.phase || 'queued', account_id: data.account_id || JSON.parse(wakeIntent.body).account_id, reason: data.reason || 'manual'});
      views.barrier();
    } catch (error) {
      if (current !== generation) return;
      if (verify || ['network', 'timeout', 'invalid_response', 'write_unconfirmed', 'idempotency_conflict'].includes(error.code) || error.status >= 500 && error.code !== 'wake_unavailable') { wakeUnconfirmed = true; feedback('wake-manual-message', '提交结果尚未确认，可能已消耗额度。不会自动重试；请显式核实同一任务，保留原标识与原账号。', 'warning'); }
      else { wakeUnconfirmed = false; $('wake-manual-consent').checked = false; feedback('wake-manual-message', '唤醒请求未接受：' + message(error.code), 'error'); }
    } finally { if (current === generation) { wakeSubmitting = false; renderManualWake(); } }
  }
  function modelSupported() { return overviewData?.capabilities?.model_settings === true; }
  function editableGenerationPrompt() { return overviewData?.capabilities?.editable_generation_prompt === true; }
  function currentModelFields() { return Object.keys(modelFields).filter(name => name !== 'generation_prompt' || editableGenerationPrompt()); }
  function readModelDraft() { return Object.fromEntries(Object.entries(modelFields).map(([name, id]) => [name, name === 'generation_prompt' && !editableGenerationPrompt() ? '' : $(id).value])); }
  function modelDirty() {
    if (!modelBase) return false;
    const original = modelConfigDraft(modelBase.config), draft = readModelDraft();
    return currentModelFields().some(name => draft[name] !== original[name]);
  }
  function fillModelDraft(data) {
    const draft = modelConfigDraft(data.config);
    if ($('generation-prompt').value !== draft.generation_prompt) $('generation-consent').checked = false;
    for (const [name, id] of Object.entries(modelFields)) if ($(id).value !== draft[name]) $(id).value = draft[name];
    modelBase = data;
  }
  function applyModelSettings(data, discard = false) {
    const config = data?.config;
    if (!Number.isSafeInteger(data?.revision) || data.revision < 0 || typeof data.instance_id !== 'string' || !config ||
      Object.keys(modelFields).filter(name => name !== 'phrases' && (name !== 'generation_prompt' || config[name] !== undefined)).some(name => typeof config[name] !== 'string') ||
      !Array.isArray(config.phrases) || config.phrases.some(phrase => typeof phrase !== 'string' || !phrase.trim() || phrase !== phrase.trim() || /[\r\n\u2028\u2029]/u.test(phrase)) ||
      Object.keys(validateModelDraft(modelConfigDraft(config)).errors).length) throw Object.assign(new Error('invalid_response'), {code: 'invalid_response'});
    if (savedModelSettings?.instance_id === data.instance_id && savedModelSettings.revision > data.revision) return false;
    const dirty = modelDirty();
    const sameBase = modelBase?.instance_id === data.instance_id && modelBase.revision === data.revision;
    savedModelSettings = data;
    if (discard || !dirty) { fillModelDraft(data); modelConflict = false; if (discard || !sameBase) modelConfirmation = ''; }
    else if (modelBase.instance_id !== data.instance_id || modelBase.revision !== data.revision) modelConflict = true;
    renderModelSettings(); return true;
  }
  function renderModelSettings() {
    const supported = modelSupported(), dirty = modelDirty(), draft = readModelDraft(), errors = modelBase ? validateModelDraft(draft).errors : {};
    const locked = !connected || !supported || !modelBase || modelSaving || modelLoading;
    $('model-fields').disabled = locked;
    $('model-save').disabled = locked || !dirty || !!Object.keys(errors).length || modelConflict || modelNeedsVerification;
    $('model-reload').disabled = !connected || !supported || modelSaving || modelLoading;
    for (const [name, id] of Object.entries(modelFields)) $(id).setAttribute('aria-invalid', errors[name] ? 'true' : 'false');
    setText('model-validation', Object.values(errors).join('；')); $('model-validation').hidden = !Object.keys(errors).length;
    $('model-conflict').hidden = !modelConflict && !modelNeedsVerification;
    setText('model-status', !connected ? '等待连接' : !supported ? '尚未接入' : modelSaving ? '保存中' : modelLoading ? '读取中' : modelNeedsVerification ? '需要核实' : modelConflict ? '设置冲突' : dirty ? '未保存草稿' : savedModelSettings ? '版本 ' + savedModelSettings.revision : '尚未读取');
    setText('model-save', modelSaving ? '保存中…' : '保存配置');
    setText('model-phrase-count', parseModelPhrases(draft.phrases).length + ' / 200 条');
    $('model-preview').disabled = locked || !!Object.keys(errors).length;
    for (const id of ['model-import', 'model-export', 'model-deduplicate', 'model-clear', 'model-confirm-action', 'model-cancel-action']) $(id).disabled = locked;
    $('model-confirmation').hidden = !modelConfirmation;
    setText('model-confirmation-text', modelConfirmation === 'clear' ? '确认清空短句池草稿？保存后才影响后台。' : '确认去除重复短句并保留首次出现的顺序？保存后才影响后台。');
    const diff = $('model-diff'); diff.replaceChildren();
    if (modelBase) {
      const original = modelConfigDraft(modelBase.config), labels = {default_model: '默认模型', default_prompt: '默认提示词', five_hour_model: '五小时模型覆盖', five_hour_prompt: '五小时提示词覆盖', weekly_model: '周模型覆盖', weekly_prompt: '周提示词覆盖', generation_prompt: '候选生成提示词', prompt_mode: '提示词策略', phrases: '短句池'};
      const display = (name, value) => name === 'prompt_mode' ? value === 'random' ? '随机短句' : '固定提示词' : value || (name === 'generation_prompt' ? '内置生成提示词' : name === 'default_model' ? '未指定模型' : name === 'phrases' ? '空' : '继承默认');
      for (const name of currentModelFields()) {
        if (draft[name] === original[name]) continue;
        const row = element('div', 'settings-diff-row model-diff-row');
        row.append(element('strong', '', labels[name]), element('span', '', display(name, original[name]) + ' → ' + display(name, draft[name]))); diff.append(row);
      }
      if (!dirty) diff.append(element('p', '', '暂无修改。编辑、导入和本地预览均不写入后台，也不发送模型请求。'));
    } else diff.append(element('p', '', connected && !supported ? '当前后台未提供模型配置能力。' : '进入本页后读取已保存配置。'));
    renderGeneration();
  }
  async function reloadModelSettings(discard = false, explicit = false) {
    if (!connected || !modelSupported() || modelLoading || modelPolling || modelSaving || modelNeedsVerification && !explicit) return false;
    const current = generation, sequence = views.begin('model-settings');
    modelLoading = discard || explicit || !modelBase; modelPolling = !modelLoading; renderModelSettings();
    try {
      const data = await api('/model-settings');
      if (current !== generation || !views.accept('model-settings', sequence, data)) return false;
      if (!applyModelSettings(data, discard)) return false;
      if (explicit) modelNeedsVerification = false;
      if (explicit) feedback('model-message', discard ? '已重新读取后台配置并丢弃草稿，未重复提交任何更改。' : '已读取配置；修改后请检查差异并明确保存。', 'success');
      return true;
    } catch (error) { if (current === generation) feedback('model-message', '读取配置失败：' + message(error.code), 'error'); return false; }
    finally { if (current === generation) { modelLoading = false; modelPolling = false; renderModelSettings(); } }
  }
  async function saveModelSettings() {
    if (!connected || !modelSupported() || modelSaving || modelLoading || modelConflict || modelNeedsVerification || !modelDirty()) return;
    const basis = modelBase, validated = validateModelDraft(readModelDraft());
    if (!editableGenerationPrompt()) delete validated.config.generation_prompt;
    if (Object.keys(validated.errors).length) { renderModelSettings(); return; }
    if (basis.instance_id !== views.instance) { modelConflict = true; feedback('model-message', '后台已重启，请重新读取配置后再保存。', 'warning'); renderModelSettings(); return; }
    const current = generation, sequence = views.begin('model-settings'); modelSaving = true; renderModelSettings(); feedback('model-message', '正在保存模型与提示词配置…');
    try {
      const data = await api('/model-settings', {method: 'POST', body: JSON.stringify({revision: basis.revision, config: validated.config})});
      if (current !== generation) return;
      if (data.instance_id !== basis.instance_id || views.instance !== basis.instance_id || data.revision < basis.revision ||
        !views.accept('model-settings', sequence, data) || currentModelFields().some(name => JSON.stringify(name === 'generation_prompt' ? data.config?.[name] ?? '' : data.config?.[name]) !== JSON.stringify(validated.config[name]))) throw Object.assign(new Error('settings_unconfirmed'), {code: 'settings_unconfirmed'});
      if (!applyModelSettings(data, true)) throw Object.assign(new Error('settings_conflict'), {code: 'settings_conflict'});
      views.barrier(); feedback('model-message', '配置已保存，本次未发送模型请求。候选生成与唤醒按已保存规则独立执行。', 'success');
    } catch (error) {
      if (current !== generation) return;
      if (['model_settings_conflict', 'settings_conflict', 'settings_unconfirmed', 'network', 'timeout', 'invalid_response'].includes(error.code)) { modelConflict = true; modelNeedsVerification = true; }
      feedback('model-message', message(['network', 'timeout', 'invalid_response'].includes(error.code) ? 'settings_unconfirmed' : error.code), 'error');
    } finally { if (current === generation) { modelSaving = false; renderModelSettings(); } }
  }
  function modelDraftChanged() {
    modelConfirmation = ''; renderModelSettings(); rememberFeedback('model-message', '草稿尚未保存。请检查差异后保存，本地预览不调用模型。');
    setText('model-preview-result', '草稿已修改，请重新生成本地预览。');
  }
  function previewModel() {
    if (!connected || !modelSupported() || !modelBase || modelSaving || modelLoading) return;
    const validated = validateModelDraft(readModelDraft());
    if (Object.keys(validated.errors).length) { renderModelSettings(); return; }
    try {
      const preview = modelPromptPreview(validated.config, $('model-preview-rule').value, context.crypto);
      setText('model-preview-result', '实际模型：' + (preview.model || '未指定（执行前须独立保存可用模型）') + (preview.modelInherited ? ' · 继承默认模型' : '') + '\n提示词来源：' + (preview.mode === 'random' ? '本地随机短句' : preview.promptInherited ? '继承默认提示词' : '固定提示词') + '\n实际提示词：\n' + preview.prompt + '\n\n仅为当前草稿的本地预览，未发送模型请求。');
    } catch (error) { feedback('model-message', error.message || '本地预览失败', 'error'); }
  }
  function generationSupported() { return overviewData?.capabilities?.prompt_generation === true; }
  function validGenerationPrompt(prompt) { return new TextEncoder().encode(prompt).length <= 8192 && !/[\x00-\x08\x0b\x0c\x0e-\x1f\x7f-\x9f]/u.test(prompt); }
  function generationCatalogSupported() { return overviewData?.capabilities?.generation_accounts === true; }
  function generationProviderName(provider) { return provider === 'antigravity' ? '反重力（Antigravity）' : 'Codex'; }
  function generationProviderSupported(provider) {
    const providers = overviewData?.capabilities?.prompt_generation_providers;
    return Array.isArray(providers) ? providers.includes(provider) : provider === 'codex';
  }
  function generationAccountState() {
    const id = $('generation-account').value.trim();
    if (!generationCatalogSupported()) return {allowed: generationProviderSupported('codex'), provider: 'codex', text: generationProviderSupported('codex') ? '旧版后台未提供账号目录，仅支持手动输入 Codex 账号 ID。' : '当前后台未提供账号目录，且未声明支持手动 Codex 账号生成。'};
    if (generationCatalogLoading) return {allowed: false, text: '正在读取本地 CPA 账号目录，请稍候。'};
    if (!generationCatalog || generationCatalog.instance !== views.instance || generationCatalogError) return {allowed: false, text: '账号目录尚未核实，请刷新生成账号。'};
    if (!id) return {allowed: false, text: '请选择 Codex 或反重力生成账号，不会自动选择。'};
    const account = generationCatalog.accounts.find(item => item.id === id);
    if (!account) return {allowed: false, text: '所选账号已移除或不在目录中，请核实；不会自动切换。'};
    if (generationAccountIdentity?.id === id && generationAccountIdentity.provider !== account.provider) return {allowed: false, text: '所选账号提供方已变化，请重新明确选择并检查模型；不会自动切换。'};
    const provider = account.provider, label = generationProviderName(provider);
    if (!generationProviderSupported(provider)) return {allowed: false, provider, text: label + ' · 当前后台尚不支持该提供方生成。'};
    const statuses = {disabled: '已禁用', unavailable: '暂不可用', binding_unavailable: '无法核实凭据绑定'};
    return {allowed: account.status === 'available', provider, text: label + ' · ' + (statuses[account.status] || '可用，请填写该提供方实际可用的模型') + '；不会自动回退到其他账号。'};
  }
  async function reloadGenerationAccounts(explicit = false) {
    if (!connected || !generationCatalogSupported() || generationCatalogLoading) return;
    clearModelCatalog('generation');
    const current = generation, instance = views.instance, sequence = ++generationCatalogSequence;
    generationCatalogLoading = true; generationCatalogAttempt = instance; generationCatalogError = ''; renderGeneration();
    try {
      const data = await api('/generation-accounts');
      if (current !== generation || sequence !== generationCatalogSequence || instance !== views.instance) return;
      const seen = new Set();
      if (data?.instance_id !== instance || !Array.isArray(data.accounts) || data.accounts.some(account => {
        if (typeof account?.id !== 'string' || !account.id.trim() || new TextEncoder().encode(account.id).length > 256 || /[\p{Cc}]/u.test(account.id) || seen.has(account.id) || !['codex', 'antigravity'].includes(account.provider) || !['available', 'disabled', 'unavailable', 'binding_unavailable'].includes(account.status)) return true;
        seen.add(account.id); return false;
      })) throw Object.assign(new Error('invalid_response'), {code: 'invalid_response'});
      generationCatalog = {instance, accounts: data.accounts.map(({id, provider, status}) => ({id, provider, status}))};
      $('generation-account-options').replaceChildren(...generationCatalog.accounts.map(account => {
        const option = element('option'); option.value = account.id;
        option.label = generationProviderName(account.provider) + ' · ' + ({available: '可用', disabled: '已禁用', unavailable: '暂不可用', binding_unavailable: '绑定不可用'}[account.status]);
        option.textContent = option.label; return option;
      }));
      const selected = generationCatalog.accounts.find(account => account.id === $('generation-account').value.trim());
      if (selected && !generationAccountIdentity) generationAccountIdentity = {id: selected.id, provider: selected.provider};
      if (explicit) feedback('catalog-feedback', '已读取 ' + generationCatalog.accounts.length + ' 个生成账号，未调用模型。', 'success');
    } catch (error) {
      if (current !== generation || sequence !== generationCatalogSequence || instance !== views.instance) return;
      generationCatalogError = message(error.code);
      feedback('catalog-feedback', '读取生成账号失败：' + generationCatalogError, 'error');
    } finally {
      if (current === generation && sequence === generationCatalogSequence) { generationCatalogLoading = false; renderGeneration(); }
    }
  }
  function ensureGenerationAccounts() {
    if (generationCatalogSupported() && generationCatalogAttempt !== views.instance) void reloadGenerationAccounts();
  }
  async function boundedManagementJSON(response) {
    const invalid = () => Object.assign(new Error('invalid_response'), {code: 'invalid_response'});
    if (!response.body?.getReader) {
      const data = await response.json();
      if (new TextEncoder().encode(JSON.stringify(data)).length > 4 * 1024 * 1024) throw invalid();
      return data;
    }
    const reader = response.body.getReader(), decoder = new TextDecoder('utf-8', {fatal: true});
    let size = 0, text = '';
    try {
      while (true) {
        const result = await reader.read();
        if (result.done) break;
        size += result.value.byteLength;
        if (size > 4 * 1024 * 1024) { await reader.cancel(); throw invalid(); }
        text += decoder.decode(result.value, {stream: true});
      }
      return JSON.parse(text + decoder.decode());
    } finally { reader.releaseLock(); }
  }
  function clearModelCatalog(kind) {
    const state = modelCatalogs[kind]; state.sequence++; state.loading = false;
    $(kind === 'wake' ? 'wake-model-options' : 'generation-model-options').replaceChildren();
    setText(kind === 'wake' ? 'model-catalog-status' : 'generation-model-status', '按所选账号读取 CPA 已注册模型，不查询额度、不试发模型。');
  }
  function renderModelCatalogs() {
    if (modelCatalogInstance !== views.instance) {
      clearModelCatalog('wake'); clearModelCatalog('generation'); modelCatalogInstance = views.instance;
    }
    $('model-catalog-account').disabled = !connected || !modelSupported();
    $('model-catalog-refresh').disabled = !connected || !modelSupported() || modelCatalogs.wake.loading;
    $('generation-refresh-models').disabled = !connected || !generationSupported() || modelCatalogs.generation.loading || !generationAccountState().allowed;
    $('model-codex-preset').disabled = $('model-fields').disabled;
  }
  async function reloadAccountModels(kind) {
    const state = modelCatalogs[kind], target = kind === 'wake' ? 'model-catalog-account' : 'generation-account';
    const status = kind === 'wake' ? 'model-catalog-status' : 'generation-model-status';
    if (!connected || state.loading || kind === 'generation' && !generationAccountState().allowed) return;
    clearModelCatalog(kind);
    const id = $(target).value.trim(), provider = kind === 'wake' ? 'codex' : generationAccountState().provider;
    const sequence = state.sequence, current = generation, instance = views.instance;
    const currentRequest = () => current === generation && sequence === state.sequence && instance === views.instance && $(target).value.trim() === id && (kind === 'wake' || generationAccountState().provider === provider);
    state.loading = true; renderModelCatalogs(); setText(status, '正在读取并核对 CPA 本地模型目录…');
    try {
      const before = resolveModelAccount(await api('/auth-files', {}, true), id, provider);
      if (!currentRequest()) return;
      const data = await api('/auth-files/models?name=' + encodeURIComponent(id), {}, true);
      if (!currentRequest()) return;
      if (!Array.isArray(data.models) || data.models.length > 2000) throw Object.assign(new Error('invalid_response'), {code: 'invalid_response'});
      const models = [...new Set(data.models.map(model => {
        if (typeof model?.id !== 'string' || !model.id || new TextEncoder().encode(model.id).length > 200 || /[\s\p{Cc}]/u.test(model.id)) throw Object.assign(new Error('invalid_response'), {code: 'invalid_response'});
        return model.id;
      }))];
      const after = resolveModelAccount(await api('/auth-files', {}, true), id, provider);
      if (!currentRequest()) return;
      if (before.index !== after.index) throw Object.assign(new Error('model_catalog_changed'), {code: 'model_catalog_changed'});
      $(kind === 'wake' ? 'wake-model-options' : 'generation-model-options').replaceChildren(...models.map(id => { const option = element('option'); option.value = id; return option; }));
      setText(status, models.length ? '已读取 ' + models.length + ' 个 CPA 注册模型；请选择或手动填写，不自动保存，也不保证实时权限。' : '该账号暂无 CPA 注册模型，可手动填写已确认可用的模型；未试发请求。');
      feedback('catalog-feedback', $(status).textContent, 'success');
    } catch (error) {
      if (currentRequest()) {
        setText(status, '读取模型失败：' + message(error.status === 404 ? 'model_catalog_unavailable' : error.code));
        feedback('catalog-feedback', $(status).textContent, 'error');
      }
    } finally {
      if (current === generation && sequence === state.sequence) { state.loading = false; renderModelCatalogs(); }
    }
  }
  function generationActive() { return ['queued', 'running'].includes(generationTask?.phase); }
  function renderGeneration() {
    const supported = generationSupported(), locked = !connected || !supported || generationSubmitting || generationReading || generationActive() || generationUnconfirmed || generationUnknown;
    const account = generationAccountState();
    $('generation-fields').disabled = locked;
    $('generation-prompt').disabled = locked || !editableGenerationPrompt() || modelSaving || modelLoading || !modelBase;
    $('generation-reset-prompt').disabled = $('generation-prompt').disabled;
    setText('generation-prompt-help', editableGenerationPrompt() ? '仅用于手动生成候选，与唤醒提示词独立。留空使用内置生成提示词；最多 8192 字节。当前草稿可直接用于生成，点击“保存配置”才持久保存。' : '当前后台不支持编辑生成提示词，使用内置提示词；不会提交新字段。');
    $('generation-submit').disabled = locked || !account.allowed || editableGenerationPrompt() && !validGenerationPrompt($('generation-prompt').value) || !$('generation-consent').checked;
    $('generation-refresh-accounts').disabled = !connected || !generationCatalogSupported() || generationCatalogLoading;
    setText('generation-directory-status', !connected ? '尚未连接，账号目录仅读取本地 CPA，不调用模型。' : !generationCatalogSupported() ? '当前后台没有账号目录；兼容手动输入 Codex 账号。' : generationCatalogLoading ? '正在读取本地 CPA 账号目录，不调用模型…' : generationCatalogError ? '读取账号目录失败：' + generationCatalogError + '。请显式刷新，不会发送模型请求。' : generationCatalog?.instance === views.instance ? '已读取 ' + generationCatalog.accounts.length + ' 个生成账号；与 Codex 额度统计独立。仅显式刷新，不随页面轮询。' : '请刷新生成账号，仅读取本地 CPA，不调用模型。');
    setText('generation-account-help', account.text);
    setText('generation-consent-label', '我确认通过' + (account.provider ? ' ' + generationProviderName(account.provider) : '所选账号') + ' 发送一次模型请求，可能消耗该账号额度。');
    $('generation-verify').hidden = !generationUnconfirmed && !generationUnknown;
    $('generation-verify').disabled = !connected || !supported || generationSubmitting || generationReading || generationUnknown && !generationTask?.job_id;
    const phases = {queued: '等待生成', running: '生成中', completed: '候选已就绪', failed: '生成失败', unknown: '结果未知'};
    setText('generation-status', !connected ? '等待连接' : !supported ? '尚未接入' : generationSubmitting ? '提交中' : generationUnconfirmed ? '需要核实' : generationUnknown ? '结果未知' : phases[generationTask?.phase] || '按需生成');
    if (!supported) setText('generation-message', '当前后台尚未提供候选生成功能，不会发送模型请求。');
    const candidates = generationTask?.phase === 'completed' ? generationTask.candidates || [] : [];
    $('generation-results').hidden = !candidates.length;
    const candidateNodes = candidates.map((phrase, index) => {
      const label = element('label', 'generation-candidate'), checkbox = element('input');
      label.dataset.renderKey = JSON.stringify([generationTask.job_id, index, phrase]);
      checkbox.type = 'checkbox'; checkbox.checked = generationSelected.has(index);
      checkbox.disabled = !connected || !supported;
      checkbox.setAttribute('aria-label', '选择候选短句 ' + (index + 1));
      checkbox.onchange = function () { if (this.checked) generationSelected.add(index); else generationSelected.delete(index); renderGeneration(); };
      label.append(checkbox, element('span', '', phrase)); return label;
    });
    updateChildren($('generation-candidates'), candidateNodes);
    $('generation-select-all').disabled = !connected || !supported || !candidates.length;
    $('generation-apply').disabled = !connected || !supported || !modelSupported() || !modelBase || modelSaving || modelLoading || !generationSelected.size;
    renderModelCatalogs();
    updateChildren($('generation-task-details'), generationTask && ['failed', 'unknown'].includes(generationTask.phase)
      ? [taskErrorDetails(generationTask.error_code || (generationTask.phase === 'unknown' ? 'generation_outcome_unknown' : 'generation_response_invalid'), generationTask, '', 'generation')] : []);
  }
  function acceptGeneration(data, expectedID = '') {
    const phases = ['queued', 'running', 'completed', 'failed', 'unknown'];
    if (typeof data?.job_id !== 'string' || !data.job_id || expectedID && data.job_id !== expectedID || data.instance_id !== views.instance || !phases.includes(data.phase) ||
      data.candidates !== undefined && (!Array.isArray(data.candidates) || data.candidates.length > 20 || data.candidates.some(phrase => typeof phrase !== 'string' || !phrase.trim() || new TextEncoder().encode(phrase).length > 512 || /[\p{Cc}\u2028\u2029]/u.test(phrase))) ||
      data.phase === 'completed' && !data.candidates?.length) throw Object.assign(new Error('invalid_response'), {code: 'invalid_response'});
    if (generationTask?.job_id !== data.job_id || generationTask.phase !== data.phase) generationSelected.clear();
    generationTask = data; generationUnconfirmed = false; generationUnknown = data.phase === 'unknown';
    if (data.phase === 'completed') { $('generation-consent').checked = false; feedback('generation-message', '候选已生成。请勾选后加入短句池草稿；尚未保存配置。', 'success'); }
    else if (data.phase === 'failed') { $('generation-consent').checked = false; feedback('generation-message', '生成任务失败：' + (codes[data.error_code] || '请查看后台诊断信息') + '。重新生成需要再次明确确认消耗额度。', 'error'); }
    else if (data.phase === 'unknown') feedback('generation-message', '生成结果未知，可能已消耗额度。禁止重复发送；可核实已有任务，不会创建新的模型请求。', 'warning');
    else feedback('generation-message', '任务已接受，正在后台' + (data.phase === 'queued' ? '排队' : '生成') + '。关闭或切换页面不会重发模型请求。');
  }
  async function submitGeneration(verify = false) {
    if (!connected || !generationSupported() || generationSubmitting || generationReading) return;
    if (verify && generationTask?.job_id) return pollGeneration(true);
    if (generationUnknown) return;
    if (verify ? !generationUnconfirmed || !generationIntent : generationUnconfirmed || generationActive() || !$('generation-consent').checked) return;
    if (!verify) {
      if (!generationAccountState().allowed) { feedback('generation-message', '请先核实可用的生成账号与提供方；' + generationAccountState().text, 'error'); return; }
      const account = $('generation-account').value.trim(), model = $('generation-model').value, rawCount = $('generation-count').value;
      const count = /^\d+$/.test(rawCount) ? Number(rawCount) : NaN;
      if (!account || new TextEncoder().encode(account).length > 256 || /[\p{Cc}]/u.test(account) || !model || new TextEncoder().encode(model).length > 200 || /[\s\p{Cc}]/u.test(model) || !Number.isSafeInteger(count) || count < 1 || count > 20) { feedback('generation-message', '请输入有效账号 ID、无空白的实际模型标识及 1–20 的整数数量，并确认消耗额度。', 'error'); return; }
      const prompt = editableGenerationPrompt() ? $('generation-prompt').value : '';
      if (!validGenerationPrompt(prompt)) { feedback('generation-message', '生成提示词最多 8192 字节，仅允许换行、回车和制表控制字符。', 'error'); return; }
      try { generationIntent = {key: requestID(context.crypto), body: JSON.stringify({account_id: account, model, count, ...(prompt ? {prompt} : {})}), instance: views.instance}; }
      catch (error) { feedback('generation-message', message(error.code), 'error'); return; }
      generationTask = null; generationSelected.clear();
    }
    if (generationIntent.instance !== views.instance) { generationUnknown = true; feedback('generation-message', '后台实例已更换，原任务可能已消耗额度。为避免重复调用，不再重发生成请求。', 'warning'); renderGeneration(); return; }
    const current = generation; generationSubmitting = true; renderGeneration(); feedback('generation-message', verify ? '正在使用原请求标识核实同一任务，不更换账号、模型或数量…' : '正在提交一次明确确认的模型请求…');
    try {
      const data = await api('/prompt-generations', {method: 'POST', headers: {'Idempotency-Key': generationIntent.key}, body: generationIntent.body});
      if (current !== generation) return;
      acceptGeneration(data);
    } catch (error) {
      if (current !== generation) return;
      if (verify || ['network', 'timeout', 'invalid_response', 'write_unconfirmed'].includes(error.code) || error.status >= 500 && error.code !== 'prompt_generation_unavailable') {
        generationUnconfirmed = true; feedback('generation-message', '提交结果尚未确认，可能已消耗额度。不会自动重试；请点击“核实同一任务”显式使用原标识和原参数核实。', 'warning');
      } else { generationUnconfirmed = false; $('generation-consent').checked = false; feedback('generation-message', '生成请求未接受：' + message(error.code), 'error'); }
    } finally { if (current === generation) { generationSubmitting = false; renderGeneration(); } }
  }
  async function pollGeneration(explicit = false) {
    if (!connected || !generationSupported() || generationSubmitting || generationReading || !generationTask?.job_id || !explicit && !generationActive()) return;
    const current = generation, expectedID = generationTask.job_id; generationReading = true; renderGeneration();
    try {
      const data = await api('/prompt-generations?job_id=' + encodeURIComponent(expectedID));
      if (current !== generation || generationTask?.job_id !== expectedID) return;
      acceptGeneration(data, expectedID);
    } catch (error) {
      if (current !== generation) return;
      if ([404, 410].includes(error.status)) { generationUnknown = true; generationTask = {...generationTask, phase: 'unknown'}; feedback('generation-message', '无法核实原生成任务，可能已消耗额度。不会重发或创建新请求。', 'warning'); }
      else feedback('generation-message', '读取生成任务失败：' + message(error.code) + '。仅重试读取，不会重发模型请求。', 'error');
    } finally { if (current === generation) { generationReading = false; renderGeneration(); } }
  }
  function applyGenerationCandidates() {
    if ($('generation-apply').disabled || !connected || !modelBase || modelSaving || modelLoading || generationTask?.phase !== 'completed') return;
    const chosen = generationTask.candidates.filter((phrase, index) => generationSelected.has(index));
    if (!chosen.length) return;
    const current = parseModelPhrases($('model-phrases').value);
    $('model-phrases').value = [...current, ...chosen].join('\n'); generationSelected.clear();
    modelDraftChanged(); renderGeneration(); feedback('generation-message', '已将所选候选加入短句池草稿，尚未保存。请检查重复项与差异后保存配置。', 'success');
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
    renderModelSettings();
    renderGeneration();
    renderModelCatalogs();
  }
  function stop() {
    toasts.clear();
    errorLimits.clear();
    generation++; connected = false; connecting = false; key = '';
    clearTimeout(pollTimer); clearTimeout(filterTimer);
    for (const controller of controllers) controller.abort();
    controllers.clear(); queryBusy.clear(); pending.clear();
    refreshing = false; updatingAccounts = false; syncing = false; settingsLoading = false; settingsSaving = false; settingsNeedsVerification = false; savingRule = null;
    if (modelSaving) { modelConflict = true; modelNeedsVerification = true; }
    modelLoading = false; modelPolling = false; modelSaving = false; modelConfirmation = '';
    if (generationSubmitting && generationIntent) generationUnconfirmed = true;
    generationSubmitting = false; generationReading = false;
    if (wakeSaving) { wakeConflict = true; wakeNeedsVerification = true; }
    if (wakeSubmitting && wakeIntent) wakeUnconfirmed = true;
    wakeLoading = false; wakePolling = false; wakeSaving = false; wakeSavingEnabled = null; wakeSubmitting = false; wakeReading = false; wakeJobsLoading = false; wakeSettingsAttempt = '';
    $('wake-settings-consent').checked = false; $('wake-overview-consent').checked = false; $('wake-manual-consent').checked = false;
    generationCatalogSequence++; generationCatalog = null; generationCatalogLoading = false; generationCatalogError = ''; generationCatalogAttempt = '';
    clearModelCatalog('wake'); clearModelCatalog('generation');
    $('generation-account-options').replaceChildren();
    ruleMessage('');
    $('login-section').hidden = false;
    setText('auto-status', '尚未连接');
    $('auto-status').dataset.state = 'offline';
    setText('metric-status', '未连接'); setText('metric-status-detail', '网页已断开 · 后台不受影响'); controls();
    for (const button of document.querySelectorAll('.account-query')) button.disabled = true;
    for (const button of document.querySelectorAll('.task-open')) button.disabled = true;
  }
  async function api(path, options = {}, management = false) {
    if (!key) throw Object.assign(new Error('unauthorized'), {code: 'unauthorized'});
    const current = generation, controller = new AbortController();
    controllers.add(controller);
    let timedOut = false;
    const deadline = setTimeout(() => { timedOut = true; controller.abort(); }, 10000);
    try {
      const endpoint = management ? base.slice(0, -'/codex-window-refresh/v2'.length) : base;
      const response = await fetch(endpoint + path, {...options, headers: {'Authorization': 'Bearer ' + key, 'Accept': 'application/json', ...(options.body ? {'Content-Type': 'application/json'} : {}), ...(options.headers || {})}, credentials: 'omit', redirect: 'error', cache: 'no-store', signal: controller.signal});
      if (current !== generation) throw Object.assign(new Error('stale'), {code: 'stale'});
      if (response.status === 401 || response.status === 403) {
        stop(); setConnection('认证失效，请重新连接');
        throw Object.assign(new Error('unauthorized'), {code: 'unauthorized', status: response.status});
      }
      const data = management ? await boundedManagementJSON(response) : await response.json();
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
      const source = element('small', 'quota-source sr-only', view.source + ' · ' + age(view.observedAt));
      cell.title = view.source + ' · ' + age(view.observedAt);
      if (view.reset) {
        const reset = element('small', 'quota-reset', view.reset);
        reset.dataset.resetAt = quota[name].ResetAt || '';
        cell.append(reset);
      }
      source.dataset.observedAt = view.observedAt || ''; source.dataset.quotaSource = view.source;
      cell.append(source);
    } else cell.append(element('small', 'quota-reset', '尚无额度数据'));
    return cell;
  }
  function renderAccounts() {
    const rows = [];
    for (const account of accountsData) {
      const row = element('tr', 'account-row'), name = element('td', 'account-name');
      row.dataset.renderKey = 'account:' + account.id;
      name.append(element('strong', '', account.id));
      const status = element('td'), value = accountStatus(account);
      status.append(badge(value.label, value.kind));
      status.title = (account.status_observed_at ? 'CPA 状态：' + age(account.status_observed_at) + '同步。' : '') + (value.kind === 'attention' ? '额度状态来自最近一次额度观测；重置或恢复需重新核实，不以本地倒计时自动恢复正常。' : '后台定时同步 CPA 账号状态。');
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
      row.append(name, status, five, weekly, observed, action); rows.push(row);
    }
    if (!accountsData.length) {
      const row = element('tr'), cell = element('td', 'empty-state', connected ? ($('accounts-search').value || $('accounts-filter').value !== 'all' ? '没有符合条件的账号' : '尚无账号，后台将自动同步 CPA') : '连接 CPA 后显示真实账号额度');
      cell.colSpan = 6; row.append(cell); rows.push(row);
    }
    updateChildren($('accounts'), rows);
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
    item.dataset.renderKey = 'query:' + (job.job_id || job.id);
    const phase = displayedJobPhase(job);
    heading.append(element('strong', '', job.account_id || job.target || '全部账号'), badge(phaseNames[phase] || '状态未知', phase === 'failed' ? 'attention' : ['queued', 'blocked'].includes(phase) ? 'unavailable' : 'available'));
    const counts = jobCounts(job);
    const waiting = jobWaiting(job);
    const description = (triggerNames[job.trigger] || (job.account_id || job.target ? '单账号额度查询' : '批量额度查询')) + (detailed ? ' · 成功 ' + counts.completed + ' / 失败 ' + counts.failed + ' / 共 ' + counts.total : ' · ' + counts.completed + ' / ' + counts.total + (counts.failed ? ' · 失败 ' + counts.failed : '')) + (Number.isInteger(waiting) ? ' · 等待 ' + waiting : '');
    const time = element('small', 'task-time', age(job.updated_at || job.created_at)); time.dataset.observedAt = job.updated_at || job.created_at || '';
    item.append(heading, element('p', 'task-description', description), time);
    if (['queued', 'blocked', 'running'].includes(job.phase) && (job.scheduled_at || job.hold_reason)) item.append(queueAnnotation({...job, phase: waiting > 0 ? 'queued' : phase}));
    if (job.error_code) item.append(taskErrorDetails(job.error_code, job));
    const failed = Array.isArray(job.items) ? job.items.filter(result => result.phase === 'failed' || result.error_code) : [];
    if (failed.length) {
      const errors = element('details', 'task-results task-errors'); errors.dataset.renderKey = 'errors';
      errors.append(element('summary', '', '逐账号错误详情 · ' + failed.length + ' 个账号'));
      const id = job.job_id || job.id, limit = errorLimits.get(id) || 20;
      for (const result of failed.slice(0, limit)) errors.append(taskErrorDetails(result.error_code || 'quota_query_failed', {...job, phase: result.phase}, result.account_id));
      if (failed.length > limit) {
        const more = element('button', 'text-button', '显示更多错误（剩余 ' + (failed.length - limit) + ' 个）'); more.type = 'button'; more.dataset.renderKey = 'more-errors';
        more.onclick = () => { errorLimits.delete(id); errorLimits.set(id, limit + 20); if (errorLimits.size > 64) errorLimits.delete(errorLimits.keys().next().value); renderTasks(); };
        errors.append(more);
      }
      item.append(errors);
    }
    if (detailed && Array.isArray(job.items)) {
      const details = element('details', 'task-results');
      details.dataset.renderKey = 'results';
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
      const button = element('button', 'task-open', job.error_code || job.failed ? '查看逐账号结果与错误详情' : '查看逐账号结果'); button.type = 'button'; button.disabled = !connected;
      button.dataset.renderKey = 'open';
      button.onclick = () => openJob(job.job_id || job.id); item.append(button);
    }
    if (!detailed && (job.error_code || job.failed || failed.length) && (job.job_id || job.id)) {
      const button = element('button', 'text-button', '查看错误详情'); button.type = 'button'; button.disabled = !connected;
      button.dataset.renderKey = 'open-errors'; button.onclick = () => openJob(job.job_id || job.id); item.append(button);
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
    updateChildren($('tasks'), active.length ? active.slice(0, 3).map(job => taskElement(job)) : [element('div', 'empty-state', connected ? '暂无执行中的任务' : '连接后查看后台任务')]);
    setText('tasks-count', Math.max(active.length, overviewData?.dashboard?.tasks?.active || 0) + ' 项');
    updateChildren($('history-list'), jobs.length ? jobs.map(job => taskElement(job, true)) : [element('div', 'empty-state', connected ? '暂无查询历史，任务将在后台持久保存' : '连接后查看已保存任务')]);
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
    setText('metric-total-detail', stats ? (stats.normal ?? stats.available) + ' 正常 · ' + stats.disabled + ' 禁用 · ' + (stats.quota_exhausted || 0) + ' 额度耗尽 · ' + (stats.quota_restricted || 0) + ' 额度受限 · ' + stats.unavailable + ' 暂不可用' : '等待同步账号');
    setText('metric-capacity', stats ? stats.capacity + ' / ' + stats.available : '— / —');
    setText('metric-capacity-detail', stats ? '已核实可用窗口 · ' + (stats.unknown_quota || 0) + ' 待核实' : '等待额度观测');
    setText('metric-errors', stats ? String(stats.errors + (data.last_error ? 1 : 0)) : '—');
    setText('metric-errors-detail', stats?.errors || data?.last_error ? '查看诊断详情' : stats ? '当前无已报告异常' : '等待后台数据');
    setText('last-updated', data?.inventory?.last_success_at ? age(data.inventory.last_success_at) + '同步 CPA' : '尚未同步 CPA');
    if (data) {
      setText('inventory', ready ? '账号状态每 ' + (data.inventory.refresh_interval_seconds || 30) + ' 秒同步 CPA；页面关闭后继续。额度耗尽由最近额度观测核实，查询仍按已保存规则执行。' : 'CPA 账号同步尚未就绪；后台会自动重试，保留已保存结果。');
      const automatic = data.capabilities?.automatic_quota_query === true, randomized = data.capabilities?.randomized_quota_query === true;
      setText('feature-note', (automatic ? '查询开关即改即存。' : '自动查询尚未接入。') + (automaticWakeSupported() ? '自动唤醒开关直接保存；开启前须确认模型额度消耗。详细规则在唤醒页独立保存。' : '当前后台尚未提供自动模型唤醒。'));
      setText('account-query-note', randomized ? '单账号立即查询 · 批量按账号随机延迟，后台持久排队' : '单账号立即查询 · 批量查询由后台执行');
      setText('batch-query-note', randomized ? '每个账号由后台分别设置随机延迟，关闭页面后继续执行。' : '执行方式以当前后台能力及任务反馈为准。');
      setText('diagnostic-query-note', (automatic ? '自动额度查询已接入。' : '自动额度查询尚未接入。') + (randomized ? '批量及自动查询按账号随机延迟。' : '') + (wakeExecutionSupported() ? '模型唤醒独立确认并按后台安全规则执行。' : '模型唤醒尚未接入。'));
    }
    renderSettings(); renderTasks(); renderEvents(); renderDiagnostics();
  }
  function renderDiagnostics() {
    const data = overviewData, inventory = data?.inventory;
    const rows = [
      ['后台版本', data?.version || '尚未连接'], ['后台实例', data?.instance_id || '—'],
      ['CPA 账号同步', inventory?.status === 'ready' ? '已连接' : '等待同步'],
      ['账号状态同步间隔', inventory?.refresh_interval_seconds ? inventory.refresh_interval_seconds + ' 秒（页面关闭后继续）' : '连接后核实'],
      ['CPA 接入连接', data?.activity?.connected ? '已连接' : '等待连接'],
      ['最近成功同步', inventory?.last_success_at ? new Date(inventory.last_success_at).toLocaleString('zh-CN', {hour12: false}) : '—'],
      ['额度查询并发', data?.query_concurrency ? String(data.query_concurrency) : '—'],
      ['正常业务额度观测', data?.capabilities?.passive_quota ? '支持' : '尚未连接'],
      ['自动额度查询', data?.capabilities?.automatic_quota_query ? '支持，按已保存规则执行' : '尚未接入'],
      ['按账号随机查询延迟', data?.capabilities?.randomized_quota_query ? '支持，由后台持久保存' : '尚未接入'],
      ['模型执行', wakeExecutionSupported() ? '已接入，仅明确授权的任务可执行' : '尚未接入'], ['后台异常', data?.last_error ? message(data.last_error) : '无已报告异常'],
      ['账号同步异常', inventory?.error_code ? message(inventory.error_code) : '无已报告异常']
    ];
    $('diagnostics-content').replaceChildren(...rows.map(([label, value]) => {
      const row = element('div', 'diagnostic-row'); row.append(element('span', '', label), element('strong', '', value)); return row;
    }));
  }
  async function accounts(reset = false, expectedRevision = accountsRevision, background = false) {
    const current = generation, sequence = views.begin('accounts'), filtersAt = filterSequence;
    const search = $('accounts-search').value.trim(), status = $('accounts-filter').value || 'all';
    let path = '/accounts?limit=' + PAGE_SIZE;
    if (search) path += '&search=' + encodeURIComponent(search);
    if (status !== 'all') path += '&status=' + encodeURIComponent(status);
    const after = reset ? '' : cursor;
    if (after) path += '&after=' + encodeURIComponent(after) + '&revision=' + expectedRevision;
    if (!background) updatingAccounts = true;
    controls();
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
      if (current !== generation || filtersAt !== filterSequence || views.latest.get('accounts') !== sequence || error.code === 'stale') return;
      if (after && error.code === 'view_changed') return accounts(true);
      throw error;
    } finally { if (current === generation && filtersAt === filterSequence && views.latest.get('accounts') === sequence) { updatingAccounts = false; controls(); } }
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
    if (savedModelSettings && savedModelSettings.instance_id !== data.instance_id) {
      if (modelDirty()) modelConflict = true;
      else { savedModelSettings = null; modelBase = null; }
      renderModelSettings();
    }
    if (generationIntent && generationIntent.instance !== data.instance_id && !generationTask?.job_id && generationUnconfirmed) {
      generationUnknown = true; feedback('generation-message', '后台实例已更换，原生成任务可能已消耗额度；不会向新实例重发请求。', 'warning');
    }
    if (savedWakeSettings && savedWakeSettings.instance_id !== data.instance_id) {
      if (wakeDirty()) wakeConflict = true;
      else { savedWakeSettings = null; wakeBase = null; }
      $('wake-settings-consent').checked = false; $('wake-overview-consent').checked = false;
    }
    if (wakeIntent && wakeIntent.instance !== data.instance_id && !wakeTask?.job_id && wakeUnconfirmed) {
      wakeUnknown = true; feedback('wake-manual-message', '后台实例已更换，原唤醒可能已消耗额度。禁止向新实例重发请求。', 'warning');
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
        if (job.phase === 'failed') notice('查询任务已结束：' + message(job.error_code || job.items?.find(item => item.error_code)?.error_code || 'quota_query_failed'), 'error');
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
      if (!updatingAccounts && (forceAccounts || data.accounts_revision !== accountsRevision || data.view_revision !== accountsViewRevision)) {
        await accounts(forceAccounts || !cursor, data.accounts_revision, !forceAccounts && !syncing);
      }
      await pollJobs();
      if (wakeSettingsSupported() && wakeSettingsAttempt !== views.instance) await reloadWakeSettings();
      if (activePage === 'wake') { await reloadWakeSettings(); await loadWakeJobs(); }
      if (wakeActive()) await readWakeJob();
      if (activePage === 'models') { ensureGenerationAccounts(); await reloadModelSettings(); await pollGeneration(); }
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
      if (wakeSettingsSupported()) await reloadWakeSettings();
      if (activePage === 'wake') await loadWakeJobs();
      if (activePage === 'query' && settingsSupported() && !savedSettings) await reloadSettings();
      if (activePage === 'models') { ensureGenerationAccounts(); await reloadModelSettings(); await pollGeneration(); }
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
    if (name === 'models' && loadSettings && connected) { ensureGenerationAccounts(); void reloadModelSettings(); void pollGeneration(); }
    if (name === 'wake' && loadSettings && connected) { ensureWakeSettings(); void loadWakeJobs(); }
  }
  function changeFilter() {
    clearTimeout(filterTimer); filterSequence++;
    if (!connected) return;
    const current = generation;
    filterTimer = setTimeout(() => { if (current === generation && connected) void accounts(true).catch(report); }, 200);
  }
  $('sidebar-toggle').onclick = () => renderSidebar($('app-sidebar').dataset.collapsed !== 'true', true);
  for (const button of document.querySelectorAll('[data-page]')) {
    if (!Object.hasOwn(pages, button.dataset.page)) continue;
    button.setAttribute('title', pages[button.dataset.page][0]); button.setAttribute('aria-label', pages[button.dataset.page][0]);
  }
  let sidebarCollapsed = false;
  try { sidebarCollapsed = context.localStorage.getItem(sidebarPreference) === 'true'; } catch {}
  renderSidebar(sidebarCollapsed);
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
  $('rules-reload').onclick = () => reloadSettings();
  $('model-settings-form').onsubmit = event => { event.preventDefault(); return saveModelSettings(); };
  $('model-reload').onclick = () => reloadModelSettings(true, true);
  $('model-catalog-refresh').onclick = () => reloadAccountModels('wake');
  $('generation-refresh-models').onclick = () => reloadAccountModels('generation');
  $('model-catalog-account').oninput = () => { clearModelCatalog('wake'); renderModelCatalogs(); };
  $('model-codex-preset').onclick = () => {
    if ($('model-fields').disabled) return;
    $('model-default-prompt').value = codexPromptPreset; modelDraftChanged();
    feedback('model-message', '已填入 Codex 风格编程短问句，仅修改草稿；保存后才用于唤醒，不读取文件或运行命令。', 'success');
  };
  for (const id of Object.values(modelFields)) $(id).addEventListener('input', () => {
    if (id === 'generation-prompt') {
      if ($('generation-prompt').disabled) return;
      $('generation-consent').checked = false;
    }
    modelDraftChanged();
  });
  $('generation-reset-prompt').onclick = () => {
    if ($('generation-prompt').disabled) return;
    $('generation-prompt').value = modelDefaults.generation_prompt; $('generation-consent').checked = false; modelDraftChanged();
    feedback('model-message', '已填入默认生成提示词，仅修改草稿，尚未保存或调用模型。', 'success');
  };
  $('model-preview').onclick = previewModel;
  $('model-preview-rule').addEventListener('change', () => setText('model-preview-result', '规则已修改，请重新生成本地预览。'));
  $('model-import').onclick = () => {
    if ($('model-import').disabled) return;
    const text = $('model-transfer').value;
    if (new TextEncoder().encode(text).length > 65536) { feedback('model-message', '导入文本超过 65536 字节，请拆分后再导入。', 'error'); return; }
    const phrases = parseModelPhrases(text);
    if (!phrases.length) { feedback('model-message', '请先粘贴非空的纯文本短句。', 'error'); return; }
    $('model-phrases').value = [...parseModelPhrases($('model-phrases').value), ...phrases].join('\n');
    modelDraftChanged(); feedback('model-message', '已追加导入草稿；尚未去重，也未保存后台。', 'success');
  };
  $('model-export').onclick = () => {
    if ($('model-export').disabled) return;
    $('model-transfer').value = exportModelPhrases(parseModelPhrases($('model-phrases').value));
    feedback('model-message', '纯文本已显示在导入 / 导出区，可选择复制；未修改后台。', 'success');
  };
  for (const [id, action] of [['model-deduplicate', 'deduplicate'], ['model-clear', 'clear']]) $(id).onclick = () => {
    if ($(id).disabled) return;
    modelConfirmation = action; renderModelSettings();
  };
  $('model-cancel-action').onclick = () => { modelConfirmation = ''; renderModelSettings(); };
  $('model-confirm-action').onclick = () => {
    if ($('model-confirm-action').disabled || !modelConfirmation) return;
    const action = modelConfirmation;
    $('model-phrases').value = modelConfirmation === 'clear' ? '' : deduplicateModelPhrases(parseModelPhrases($('model-phrases').value)).join('\n');
    modelDraftChanged();
    feedback('model-message', action === 'clear' ? '已清空短句池草稿，尚未保存。' : '已对短句池草稿去重，尚未保存。', 'success');
  };
  $('generation-form').onsubmit = event => { event.preventDefault(); return submitGeneration(); };
  $('generation-verify').onclick = () => submitGeneration(true);
  $('generation-consent').onchange = renderGeneration;
  for (const id of ['generation-model', 'generation-count']) $(id).oninput = () => {
    if ($('generation-fields').disabled) return;
    $('generation-consent').checked = false; renderGeneration();
  };
  $('generation-refresh-accounts').onclick = () => reloadGenerationAccounts(true);
  $('generation-account').oninput = () => {
    if ($('generation-fields').disabled) return;
    clearModelCatalog('generation');
    const id = $('generation-account').value.trim(), selected = generationCatalog?.accounts.find(account => account.id === id);
    if (selected?.provider === 'antigravity' && generationAccountIdentity?.provider !== 'antigravity' || selected && generationAccountIdentity && selected.provider !== generationAccountIdentity.provider) $('generation-model').value = '';
    if (selected) generationAccountIdentity = {id: selected.id, provider: selected.provider};
    $('generation-consent').checked = false; renderGeneration();
  };
  $('generation-use-default').onclick = () => {
    if ($('generation-fields').disabled) return;
    $('generation-model').value = $('model-default').value; $('generation-consent').checked = false;
    feedback('generation-message', '已按你的操作填入草稿默认模型，请确认它属于所选账号提供方且实际可用；尚未调用模型。'); renderGeneration();
  };
  $('generation-select-all').onclick = () => {
    if ($('generation-select-all').disabled) return;
    generationTask.candidates.forEach((phrase, index) => generationSelected.add(index)); renderGeneration();
  };
  $('generation-apply').onclick = applyGenerationCandidates;
  for (const id of Object.values(settingsFields)) $(id).addEventListener('input', () => { renderSettings(); rememberFeedback('settings-message', '草稿尚未保存，请核对差异后保存。'); });
  $('five-hour-query').onchange = () => toggleRule('fiveHour', $('five-hour-query').checked);
  $('weekly-query').onchange = () => toggleRule('weekly', $('weekly-query').checked);
  $('wake-settings-form').onsubmit = event => { event.preventDefault(); return saveWakeSettings(); };
  $('wake-settings-reload').onclick = () => reloadWakeSettings(true, true);
  $('wake-overview-reload').onclick = () => reloadWakeSettings(false, true);
  $('wake-settings-consent').onchange = renderWakeSettings;
  $('wake-overview-consent').onchange = renderWakeSettings;
  $('auto-wake').onchange = () => saveWakeSettings(true, $('auto-wake').checked);
  for (const id of Object.values(wakeFields)) $(id).addEventListener('input', () => {
    if ($('wake-settings-fields').disabled) return;
    $('wake-settings-consent').checked = false; renderWakeSettings(); rememberFeedback('wake-settings-message', '唤醒草稿尚未保存；启用后可能持续消耗额度，请核对差异并明确确认。');
  });
  $('wake-manual-form').onsubmit = event => { event.preventDefault(); return submitManualWake(); };
  $('wake-manual-verify').onclick = () => submitManualWake(true);
  $('wake-manual-consent').onchange = renderManualWake;
  $('wake-account').oninput = () => { if (!$('wake-manual-fields').disabled) { $('wake-manual-consent').checked = false; renderManualWake(); } };
  $('wake-history-refresh').onclick = () => loadWakeJobs(true);
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
