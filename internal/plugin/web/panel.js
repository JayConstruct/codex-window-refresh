/* Public assets reuse the same-origin CPA panel's saved management key.
 * Host auth storage is read-only; only non-secret panel preferences are persisted.
 * Requests use fixed local endpoints and never automatically repeat writes. */
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
  const paths = new Set(['/snapshot', '/summary', '/accounts', '/capabilities', '/config-editor', '/config', '/reload', '/recheck', '/quota-query', '/quota-connection-check', '/manual-wake', '/prompt-generation', '/history']);
  class PanelError extends Error {
    constructor(code, status) { super(code); this.name = 'PanelError'; this.code = code; this.status = status || 0; }
  }
  function clone(value) { return JSON.parse(JSON.stringify(value)); }
  function validObject(value) { return value !== null && typeof value === 'object' && !Array.isArray(value); }
  function createPanelPreferences(context, base) {
    // localStorage is origin-scoped; include the API path to isolate proxy mounts.
    const key = 'codex-window-refresh:panel-preferences:v1:' + base;
    let value = {autoRefresh: false, backgroundRefresh: false};
    try {
      const saved = JSON.parse(context.localStorage.getItem(key));
      if (validObject(saved) && saved.version === 1 && typeof saved.autoRefresh === 'boolean' && typeof saved.backgroundRefresh === 'boolean') {
        value = {autoRefresh: saved.autoRefresh, backgroundRefresh: saved.autoRefresh && saved.backgroundRefresh};
      }
    } catch (_) { /* Storage can be unavailable or contain damaged JSON. */ }
    return {
      value: () => ({...value}),
      save(autoRefresh, backgroundRefresh) {
        value = {autoRefresh: autoRefresh === true, backgroundRefresh: autoRefresh === true && backgroundRefresh === true};
        try { context.localStorage.setItem(key, JSON.stringify({version: 1, ...value})); }
        catch (_) { /* Keep the current session usable when persistence is denied. */ }
      }
    };
  }
  const accountSorts = ['id', 'five-remaining', 'weekly-remaining', 'reset', 'attention'];
  function createAccountPreferences(context, base) {
    const key = 'codex-window-refresh:account-view:v1:' + base;
    let value = {sort: 'id', layout: 'cards'};
    try {
      const saved = JSON.parse(context.localStorage.getItem(key));
      if (validObject(saved) && saved.version === 1 && accountSorts.includes(saved.sort) && ['cards', 'compact'].includes(saved.layout)) value = {sort: saved.sort, layout: saved.layout};
    } catch (_) { /* Display preferences must not prevent login. */ }
    return {
      value: () => ({...value}),
      save(sort, layout) {
        if (!accountSorts.includes(sort) || !['cards', 'compact'].includes(layout)) return;
        value = {sort, layout};
        try { context.localStorage.setItem(key, JSON.stringify({version: 1, ...value})); } catch (_) {}
      }
    };
  }
  const settingDefaults = {'quota_query.automatic': false, 'quota_query.manual': true, 'quota_query.interval': '30s', 'quota_query.on_five_hour_expiry': false, 'quota_query.on_weekly_expiry': false, 'ordinary_rules.daily_enabled': true, 'planning.enabled': false, 'planning.horizon_days': 7, 'reset_rules.five_hour.model': '', 'reset_rules.five_hour.prompt': '', 'reset_rules.weekly.model': '', 'reset_rules.weekly.prompt': ''};
  Object.assign(settingDefaults, {'execution.prompt_mode': 'random', 'execution.prompt_pool': ['Reply OK.', 'Say hello briefly.', 'Respond with Ready.', 'Give a short greeting.', 'Reply with one friendly word.'], 'reset_rules.five_hour.prompt_pool': [], 'reset_rules.weekly.prompt_pool': [], 'quota_query.five_hour_recent_usage.enabled': false, 'quota_query.five_hour_recent_usage.lookback': '30m'});
  for (const window of ['five_hour', 'weekly']) {
    settingDefaults['quota_query.' + window + '.periodic_enabled'] = true;
    settingDefaults['quota_query.' + window + '.interval'] = '0s';
    for (const trigger of ['expiry', 'periodic']) {
      const prefix = 'quota_query.' + window + '.' + trigger;
      settingDefaults[prefix + '.excluded_hours'] = [];
      settingDefaults[prefix + '.random_delay.min'] = '0s';
      settingDefaults[prefix + '.random_delay.max'] = '0s';
    }
  }
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
    async function request(path, method, body, signal) {
      method = method || 'GET';
      const endpoint = path.split('?')[0];
      if (!paths.has(endpoint) || !['GET', 'POST'].includes(method) || (endpoint !== path && (endpoint !== '/history' || method !== 'GET'))) throw new PanelError('invalid_endpoint');
      if (endpoint === '/history' && path !== endpoint) {
        const query = new URLSearchParams(path.slice(endpoint.length + 1));
        for (const name of query.keys()) if (!['account_id', 'kind', 'status', 'from', 'until', 'cursor', 'limit'].includes(name) || query.getAll(name).length !== 1) throw new PanelError('invalid_endpoint');
      }
      if (!key) throw new PanelError('disconnected');
      const current = generation, controller = new AbortController();
      controllers.add(controller);
      const cancelRequest = () => controller.abort();
      if (signal) signal.addEventListener('abort', cancelRequest, {once: true});
      let timedOut = false;
      let timer;
      const readOnly = method === 'GET' || path === '/quota-connection-check';
      try {
        if (signal?.aborted) throw new PanelError('cancelled');
        timer = timers.setTimeout(() => { timedOut = true; controller.abort(); }, readOnly ? 15000 : 120000);
        const headers = {'Authorization': 'Bearer ' + key, 'Accept': 'application/json'};
        if (body !== undefined) headers['Content-Type'] = 'application/json';
        const result = await fetcher(base + path, {method, headers, body: body === undefined ? undefined : JSON.stringify(body), credentials: 'omit', redirect: 'error', cache: 'no-store', signal: controller.signal});
        if (generation !== current || !key) throw new PanelError('stale');
        if (controller.signal.aborted) throw new PanelError(timedOut ? (readOnly ? 'timeout' : 'write_unconfirmed') : 'cancelled');
        if (result.status === 401 || result.status === 403) { disconnect(); throw new PanelError('authentication', result.status); }
        if (!result.ok) throw new PanelError(result.status === 409 ? 'conflict' : result.status === 400 ? 'invalid_config' : 'http_error', result.status);
        // Ignore arbitrary error bodies and never reflect server text into the UI.
        if (!String(result.headers.get('content-type') || '').toLowerCase().includes('application/json')) throw new PanelError('invalid_response');
        const data = await result.json();
        if (generation !== current || !key) throw new PanelError('stale');
        if (controller.signal.aborted) throw new PanelError(timedOut ? (readOnly ? 'timeout' : 'write_unconfirmed') : 'cancelled');
        return data;
      } catch (error) {
        if (generation !== current && !(error instanceof PanelError && error.code === 'authentication')) throw new PanelError('stale');
        if (error instanceof PanelError) throw error;
        if (signal?.aborted && !timedOut) throw new PanelError('cancelled');
        throw new PanelError(timedOut ? (readOnly ? 'timeout' : 'write_unconfirmed') : (readOnly ? 'network' : 'write_unconfirmed'));
      } finally {
        timers.clearTimeout(timer);
        controllers.delete(controller);
        if (signal) signal.removeEventListener('abort', cancelRequest);
      }
    }
    return {setKey, disconnect, request, generation: () => generation, connected: () => !!key};
  }
  function validDraft(value) {
    return validObject(value) && typeof value.enabled === 'boolean' && typeof value.timezone === 'string'
      && validObject(value.activity) && validObject(value.execution) && validObject(value.ordinary_rules)
      && (value.execution.prompt_mode === undefined || ['fixed', 'random'].includes(value.execution.prompt_mode))
      && [value.execution.prompt_pool, value.reset_rules?.five_hour?.prompt_pool, value.reset_rules?.weekly?.prompt_pool].every(pool => pool === undefined || (Array.isArray(pool) && pool.length <= 50 && pool.every(prompt => typeof prompt === 'string' && prompt.trim())))
      && (value.quota_query?.five_hour_recent_usage === undefined || (validObject(value.quota_query.five_hour_recent_usage) && (value.quota_query.five_hour_recent_usage.enabled === undefined || typeof value.quota_query.five_hour_recent_usage.enabled === 'boolean') && (value.quota_query.five_hour_recent_usage.lookback === undefined || typeof value.quota_query.five_hour_recent_usage.lookback === 'string')))
      && Array.isArray(value.ordinary_rules.allowed_hours) && value.ordinary_rules.allowed_hours.every(x => typeof x === 'string')
      && validObject(value.reset_rules) && validObject(value.reset_rules.five_hour) && validObject(value.reset_rules.weekly)
      && [value.reset_rules.five_hour, value.reset_rules.weekly].every(rule => (rule.model === undefined || typeof rule.model === 'string') && (rule.prompt === undefined || typeof rule.prompt === 'string'))
      && validObject(value.reset_rules.five_hour.random_delay) && validObject(value.reset_rules.weekly.random_delay)
      && (value.quota_query === undefined || (validObject(value.quota_query) && typeof value.quota_query.automatic === 'boolean' && typeof value.quota_query.manual === 'boolean' && typeof value.quota_query.interval === 'string'))
      && (value.quota_query?.on_five_hour_expiry === undefined || typeof value.quota_query.on_five_hour_expiry === 'boolean')
      && (value.quota_query?.on_weekly_expiry === undefined || typeof value.quota_query.on_weekly_expiry === 'boolean')
      && [value.quota_query?.five_hour, value.quota_query?.weekly].every(window => window === undefined || (validObject(window)
        && (window.periodic_enabled === undefined || typeof window.periodic_enabled === 'boolean')
        && (window.interval === undefined || typeof window.interval === 'string')
        && [window.expiry, window.periodic].every(timing => timing === undefined || (validObject(timing)
          && (timing.excluded_hours === undefined || (Array.isArray(timing.excluded_hours) && timing.excluded_hours.every(hours => typeof hours === 'string')))
          && (timing.random_delay === undefined || (validObject(timing.random_delay) && ['min', 'max'].every(bound => timing.random_delay[bound] === undefined || typeof timing.random_delay[bound] === 'string')))))))
      && (value.ordinary_rules.daily_enabled === undefined || typeof value.ordinary_rules.daily_enabled === 'boolean')
      && (value.planning === undefined || (validObject(value.planning) && typeof value.planning.enabled === 'boolean' && Number.isInteger(value.planning.horizon_days) && value.planning.horizon_days >= 1 && value.planning.horizon_days <= 30))
      && Array.isArray(value.daily_rules) && value.daily_rules.every(validObject) && validObject(value.account_overrides)
      && Object.values(value.account_overrides).every(validObject);
  }
  function dailyTimeRange(mode, start, end) {
    const clock = value => typeof value === 'string' && /^(?:[01][0-9]|2[0-3]):[0-5][0-9]$/.test(value);
    if (!['point', 'range'].includes(mode) || !clock(start) || (mode === 'range' && (!clock(end) && end !== '24:00'))) throw new PanelError('invalid_config');
    const finish = mode === 'point' ? start : end;
    if (finish < start || (mode === 'range' && finish === start)) throw new PanelError('invalid_config');
    return start + '-' + finish;
  }
  // Match the configuration wire format: integer durations with s/m/h/d units.
  function durationSeconds(value) {
    if (typeof value !== 'string' || !/^[0-9]+[smhd]$/.test(value)) return NaN;
    const result = Number(value.slice(0, -1)) * {s: 1, m: 60, h: 3600, d: 86400}[value.slice(-1)];
    return Number.isSafeInteger(result) ? result : NaN;
  }
  function hoursError(values) {
    if (!Array.isArray(values)) return '请填写 HH:mm-HH:mm 时段，多个时段用逗号分隔。';
    const ranges = [];
    for (const value of values) {
      const match = /^(\d{2}):(\d{2})-(\d{2}):(\d{2})$/.exec(value);
      if (!match || +match[1] > 23 || +match[2] > 59 || +match[3] > 24 || +match[4] > 59 || (+match[3] === 24 && +match[4] !== 0)) return '请填写有效的 HH:mm-HH:mm 时段，结束可为 24:00。';
      const start = +match[1]*60 + +match[2], end = +match[3]*60 + +match[4];
      if (start === end) return '允许或排除时段的开始与结束不能相同；全天可填 00:00-24:00。';
      if (end < start) { ranges.push([start, 1440]); if (end > 0) ranges.push([0, end]); }
      else ranges.push([start, end]);
    }
    ranges.sort((a, b) => a[0] - b[0]);
    for (let i = 1; i < ranges.length; i++) if (ranges[i][0] < ranges[i - 1][1]) return '时段不能重叠；相邻时段可以连接，跨午夜时也需避免重叠。';
    return '';
  }
  function configFieldErrors(config) {
    const errors = {}, get = path => path.split('.').reduce((value, key) => value?.[key], config);
    const put = (path, message) => { if (!errors[path]) errors[path] = message; };
    const duration = (path, min, max, range) => {
      const value = get(path); if (value === undefined) return;
      const seconds = durationSeconds(value);
      if (!Number.isFinite(seconds) || seconds < min || seconds > max) put(path, '请填写' + range + '，使用整数加 s、m、h 或 d（例如 30s、5m）。');
    };
    const number = (path, min, max, integer) => {
      const value = get(path); if (value === undefined) return;
      if (typeof value !== 'number' || !Number.isFinite(value) || value < min || value > max || (integer && !Number.isInteger(value))) put(path, '请填写 ' + min + '–' + max + ' 之间的' + (integer ? '整数。' : '数值。'));
    };
    if (config.timezone !== undefined) {
      // Go's zone database is authoritative. Intl rejects some valid server
      // aliases (for example Factory), so reject only malformed names here.
      const timezone = config.timezone;
      if (typeof timezone !== 'string' || timezone === 'Local' || !/^[A-Za-z][A-Za-z0-9_+\-]*(?:\/[A-Za-z0-9_+\-]+)*$/.test(timezone)) put('timezone', '请填写 IANA 时区名称，例如 Asia/Shanghai 或 UTC；不能使用 Local、空格或数字偏移。名称是否可用由服务器校验。');
    }
    duration('activity.lookback', 60, 86400, '1 分钟至 24 小时');
    duration('quota_query.interval', 30, 3600, '30 秒至 1 小时');
    duration('quota_query.five_hour_recent_usage.lookback', 60, 604800, '1 分钟至 7 天');
    number('activity.accounts_per_user', 0, 100, false); number('activity.max_accounts', 0, 100000, true); number('planning.horizon_days', 1, 30, true);
    number('execution.max_concurrency', 1, 32, true);
    for (const [key, min, max, text] of [['quota_read_timeout',1,300,'1 秒至 5 分钟'],['request_timeout',1,1800,'1 秒至 30 分钟'],['min_dispatch_spacing',0,3600,'0 秒至 1 小时'],['quota_max_age_before_send',0,300,'0 秒至 5 分钟'],['verification_capacity_hold',60,86400,'1 分钟至 24 小时']]) duration('execution.' + key, min, max, text);
    const delay = path => {
      duration(path + '.min', 0, 86400, '0 秒至 24 小时'); duration(path + '.max', 0, 86400, '0 秒至 24 小时');
      if (durationSeconds(get(path + '.min')) > durationSeconds(get(path + '.max'))) put(path + '.max', '最长延迟不能小于最短延迟。');
    };
    const hours = path => { if (get(path) !== undefined) { const message = hoursError(get(path)); if (message) put(path, message); } };
    hours('ordinary_rules.allowed_hours');
    for (const window of ['five_hour', 'weekly']) {
      delay('reset_rules.' + window + '.random_delay');
      const path = 'quota_query.' + window + '.interval', interval = get(path);
      if (interval !== undefined && interval !== '0s' && durationSeconds(interval) !== 0) {
        duration(path, 30, 3600, '0 秒（继承）或 30 秒至 1 小时');
        if (durationSeconds(interval) < durationSeconds(get('quota_query.interval'))) put(path, '本类查询间隔不能低于公共查询间隔；填 0s 可继承。');
      } else if (interval !== undefined && !Number.isFinite(durationSeconds(interval))) put(path, '请填写 0s（继承）或 30s–1h 的整数时长。');
      for (const trigger of ['expiry', 'periodic']) { const prefix = 'quota_query.' + window + '.' + trigger; hours(prefix + '.excluded_hours'); delay(prefix + '.random_delay'); }
    }
    const bytes = value => new TextEncoder().encode(value).length;
    for (const prefix of ['execution', 'reset_rules.five_hour', 'reset_rules.weekly']) {
      const model = get(prefix + '.model'), prompt = get(prefix + '.prompt'), pool = get(prefix + '.prompt_pool');
      if (model !== undefined && (typeof model !== 'string' || bytes(model) > 256 || (prefix !== 'execution' && model !== '' && !model.trim()) || /REPLACE_WITH/i.test(model))) put(prefix + '.model', '模型名最多 256 字节，请替换示例占位符；专用模型留空可继承默认值。');
      if (prompt !== undefined && (typeof prompt !== 'string' || bytes(prompt) > 4096 || (prefix === 'execution' || prompt !== '') && !prompt.trim())) put(prefix + '.prompt', prefix === 'execution' ? '默认发送内容不能为空，最多 4096 字节。' : '专用内容可留空继承；填写时不能为空白，最多 4096 字节。');
      if (pool !== undefined) {
        const required = prefix === 'execution' && get('execution.prompt_mode') === 'random';
        if (!Array.isArray(pool) || pool.length > 50 || (required && !pool.length)) put(prefix + '.prompt_pool', required ? '随机模式需要 1–50 条短句，每行一条。' : '最多填写 50 条短句，每行一条；专用池可留空继承。');
        else if (pool.some(value => typeof value !== 'string' || !value.trim() || bytes(value) > 256 || /[\x00\r\n]/.test(value))) put(prefix + '.prompt_pool', '每条短句必须为单行非空内容，最多 256 字节（中文通常每字 3 字节）。');
        else if (new Set(pool.map(value => value.trim())).size !== pool.length) put(prefix + '.prompt_pool', '短句池中有重复内容，请删除重复行。');
      }
    }
    if (get('execution.prompt_mode') !== undefined && !['fixed', 'random'].includes(get('execution.prompt_mode'))) put('execution.prompt_mode', '请选择固定内容或随机短句池。');
    const modelText = path => typeof get(path) === 'string' ? get(path).trim() : '';
    const needsFive = get('reset_rules.five_hour.enabled') || (get('ordinary_rules.daily_enabled') !== false && config.daily_rules?.some(rule => rule.enabled));
    if (config.enabled && ((needsFive && !(modelText('reset_rules.five_hour.model') || modelText('execution.model'))) || (get('reset_rules.weekly.enabled') && !(modelText('reset_rules.weekly.model') || modelText('execution.model'))))) put('execution.model', '开启的唤醒规则需要默认模型或对应的专用模型。');
    for (const path of ['execution.catch_up', 'reset_rules.weekly.bypass_activity_limit', 'reset_rules.weekly.bypass_allowed_hours']) if (get(path) === false) put(path, '此安全设置必须保持开启。');
    const ids = new Set();
    for (const [index, rule] of (config.daily_rules || []).entries()) {
      const path = 'daily_rules.' + index;
      if (typeof rule.id !== 'string' || !/^[a-zA-Z0-9_-]{1,64}$/.test(rule.id) || ids.has(rule.id)) put(path + '.id', '规则 ID 必须唯一，包含 1–64 个字母、数字、下划线或连字符。'); ids.add(rule.id);
      const match = /^(\d{2}:\d{2})-(\d{2}:\d{2})$/.exec(rule.random_time_range || '');
      try { if (!match) throw new Error(); dailyTimeRange(match[1] === match[2] ? 'point' : 'range', match[1], match[2]); }
      catch (_) { put(path + '.random_time_range', '请输入有效的 HH:mm 时间；每日时段不能跨午夜，结束可为 24:00。'); }
      if (!Number.isInteger(rule.minimum_ready_accounts) || rule.minimum_ready_accounts < 0 || rule.minimum_ready_accounts > get('activity.max_accounts')) put(path + '.minimum_ready_accounts', '最低就绪账号数必须为非负整数，且不超过普通唤醒账号数上限。');
      if (rule.catch_up === false) put(path + '.catch_up', '每日规则必须启用错过窗口后补发。');
    }
    if ((config.daily_rules || []).length > 100) put('daily_rules', '每日规则最多 100 条。');
    return errors;
  }
  function createEditor() {
    let draft = null, revision = '', baseline = '', version = 0, raw = false, formRaw = false;
    const editor = {
      load(envelope, force) {
        if (!validObject(envelope) || !validObject(envelope.config) || typeof envelope.revision !== 'string') throw new PanelError('invalid_response');
        if (editor.dirty() && !force) return false;
        draft = clone(envelope.config); revision = envelope.revision; baseline = JSON.stringify(draft); raw = false; formRaw = false; version++;
        return true;
      },
      value: () => draft ? clone(draft) : null,
      baseline: () => baseline ? JSON.parse(baseline) : null,
      revision: () => revision,
      version: () => version,
      dirty: () => raw || formRaw || (draft !== null && JSON.stringify(draft) !== baseline),
      rawDirty: () => raw,
      formDirty: () => formRaw,
      markFormDirty(value) { formRaw = value === true; version++; },
      markRaw() { raw = true; version++; },
      replace(value) { if (!validDraft(value)) throw new PanelError('invalid_config'); draft = clone(value); raw = false; version++; },
      update(path, value) {
        if (!draft || raw) throw new PanelError('unapplied_json');
        let current = draft;
        const parts = path.split('.');
        for (let i = 0; i < parts.length - 1; i++) {
          if (i === 0 && !Object.hasOwn(current, parts[i]) && Object.hasOwn(settingDefaults, path)) {
            if (parts[i] === 'planning') current.planning = {enabled: false, horizon_days: 7};
            if (parts[i] === 'quota_query') current.quota_query = {automatic: false, manual: true, interval: '30s', on_five_hour_expiry: false, on_weekly_expiry: false};
          }
          if (!Object.hasOwn(current, parts[i]) && Object.hasOwn(settingDefaults, path)) current[parts[i]] = {};
          if (!Object.hasOwn(current, parts[i]) || !validObject(current[parts[i]])) throw new PanelError('invalid_config');
          current = current[parts[i]];
        }
        if (!Object.hasOwn(current, parts[parts.length - 1]) && !Object.hasOwn(settingDefaults, path)) throw new PanelError('invalid_config');
        current[parts[parts.length - 1]] = clone(value); version++;
      },
      setDailyRules(rules) {
        if (!draft || raw) throw new PanelError('unapplied_json');
        if (!Array.isArray(rules) || !rules.every(validObject)) throw new PanelError('invalid_config');
        draft.daily_rules = clone(rules); version++;
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
      clear() { draft = null; revision = ''; baseline = ''; raw = false; formRaw = false; version++; }
    };
    return editor;
  }
  function resetTime(value) {
    if (!value || typeof value !== 'string' || value.startsWith('0001-')) return null;
    const time = Date.parse(value);
    return Number.isFinite(time) ? time : null;
  }
  function observationAge(value, now = Date.now()) {
    const time = resetTime(value);
    if (time === null) return '尚无观测时间';
    const seconds = Math.floor((now - time) / 1000);
    if (seconds < -5) return '观测时间晚于本机时间';
    if (seconds < 60) return '刚刚';
    if (seconds < 3600) return Math.floor(seconds / 60) + ' 分钟前';
    if (seconds < 86400) return Math.floor(seconds / 3600) + ' 小时前';
    return Math.floor(seconds / 86400) + ' 天前';
  }
  function accountComparator(sort) {
    function rank(account) {
      if (sort === 'five-remaining' || sort === 'weekly-remaining') {
        const amount = account[sort === 'five-remaining' ? 'FiveHour' : 'Weekly']?.Remaining;
        return amount?.Unit === 'percent' && Number.isFinite(amount.Value) ? amount.Value : Infinity;
      }
      if (sort === 'reset') return ['active', 'expired'].includes(account.FiveHour?.State) ? resetTime(account.FiveHour.ResetAt) ?? Infinity : Infinity;
      if (sort === 'attention') {
        if (account.Enabled === false || account.Permission === 'blocked') return 0;
        if (['account_inventory_unavailable', 'quota_adapter_unavailable', 'credential_changed', 'weekly_reset_conflict', 'backoff', 'quota_exhausted'].includes(account.Task?.HoldReason)) return 1;
        if (!account.FiveHour?.State || account.FiveHour.State === 'unknown' || account.Permission === 'unknown') return 2;
        return 3;
      }
      return 0;
    }
    return ([idA, a], [idB, b]) => {
      const rankA = rank(a), rankB = rank(b);
      return (rankA < rankB ? -1 : rankA > rankB ? 1 : 0) || idA.localeCompare(idB);
    };
  }
  const changeLabels = {
    enabled: '启用', timezone: '规则时区', schema_version: '规则格式版本', quota_query: '额度查询', automatic: '定期查询总开关', manual: '手动查询兼容字段', interval: '查询间隔',
    on_five_hour_expiry: '5 小时到期查询', on_weekly_expiry: '周到期查询', five_hour: '5 小时规则', weekly: '周规则',
    five_hour_recent_usage: '近期用户条件', lookback: '活动回溯', periodic_enabled: '参与定期查询', expiry: '到期查询', periodic: '定期查询', excluded_hours: '排除时段',
    reset_rules: '自动唤醒规则', random_delay: '随机延迟', min: '最短', max: '最长', model: '模型', prompt: '固定内容', prompt_mode: '内容模式', prompt_pool: '短句池',
    activity: '账号容量', accounts_per_user: '每用户账号数', max_accounts: '账号数上限', identity_source: '用户统计来源',
    ordinary_rules: '普通唤醒', allowed_hours: '允许时段', allow_outside_hours_when_users_active: '有用户时允许时段外唤醒', daily_enabled: '每日规则总开关',
    daily_rules: '每日规则', id: '规则名称', minimum_ready_accounts: '最低就绪账号数', random_time_range: '执行时间', account_overrides: '账号覆盖', paused: '暂停唤醒', execution: '模型执行', catch_up: '错过后补发',
    max_concurrency: '最大并发', min_dispatch_spacing: '发送间隔', quota_read_timeout: '额度查询超时', request_timeout: '模型请求超时', quota_max_age_before_send: '发送前额度有效期',
    verify_delays: '验证间隔', verification_capacity_hold: '验证容量保留', planning: '未来计划', horizon_days: '覆盖天数', bypass_activity_limit: '豁免人数限制', bypass_allowed_hours: '豁免允许时段'
  };
  function configChanges(before, after) {
    const changes = [];
    function walk(left, right, parts) {
      if (validObject(left) || validObject(right)) {
        const keys = [...new Set([...Object.keys(validObject(left) ? left : {}), ...Object.keys(validObject(right) ? right : {})])].sort();
        for (const key of keys) walk(validObject(left) && Object.hasOwn(left, key) ? left[key] : undefined, validObject(right) && Object.hasOwn(right, key) ? right[key] : undefined, [...parts, key]);
      } else if (JSON.stringify(left) !== JSON.stringify(right)) {
        const label = parts.map((part, index) => parts[0] === 'account_overrides' && index === 1 ? '账号「' + part + '」' : parts.length === 1 && part === 'enabled' ? '自动模型唤醒' : Object.hasOwn(changeLabels, part) ? changeLabels[part] : part).join(' / ');
        changes.push({label, before: left, after: right});
      }
    }
    walk(before, after, []);
    return changes;
  }
  function changeValue(value) {
    if (value === undefined) return '未设置（继承默认）';
    if (typeof value === 'boolean') return value ? '开启' : '关闭';
    if (Array.isArray(value)) return value.length ? value.map(item => validObject(item) ? Object.entries(item).map(([key, value]) => (changeLabels[key] || key) + '：' + changeValue(value)).join('，') : String(item)).join('；') : '空列表';
    if (value === '') return '留空';
    return String(value);
  }
  function patchAccountNode(current, next, selection) {
    if (current.nodeType !== next.nodeType || current.nodeName !== next.nodeName) {
      current.replaceWith(next);
      return;
    }
    if (current.nodeType === 3) {
      let selected = false;
      try { selected = selection && !selection.isCollapsed && selection.containsNode(current, true); } catch (_) {}
      if (!selected && current.nodeValue !== next.nodeValue) current.nodeValue = next.nodeValue;
      return;
    }
    for (const attribute of Array.from(current.attributes || [])) if (!next.hasAttribute(attribute.name)) current.removeAttribute(attribute.name);
    for (const attribute of Array.from(next.attributes || [])) if (current.getAttribute(attribute.name) !== attribute.value) current.setAttribute(attribute.name, attribute.value);
    const children = Array.from(next.childNodes);
    for (let index = 0; index < children.length; index++) {
      const previous = current.childNodes[index];
      if (previous) patchAccountNode(previous, children[index], selection);
      else current.append(children[index]);
    }
    while (current.childNodes.length > children.length) current.lastChild.remove();
  }
  function updateLiveText(item, value, selection) {
    if (item.textContent === value) return;
    try { if (selection && !selection.isCollapsed && selection.containsNode(item, true)) return; } catch (_) {}
    item.textContent = value;
  }
  function countdown(value, now) {
    const time = resetTime(value);
    if (time === null) return '重置时间未知';
    const seconds = Math.ceil((time - (now === undefined ? Date.now() : now)) / 1000);
    if (seconds <= 0) return '已到重置时间 · 等待最新证据';
    const days = Math.floor(seconds / 86400), hours = Math.floor(seconds % 86400 / 3600), minutes = Math.floor(seconds % 3600 / 60), s = seconds % 60;
    return (days ? days + ' 天 ' : '') + String(hours).padStart(2, '0') + ':' + String(minutes).padStart(2, '0') + ':' + String(s).padStart(2, '0');
  }
  function windowCountdown(evidence, now) {
    if (evidence?.State === 'active') return countdown(evidence.ResetAt, now);
    if (evidence?.State === 'inactive') return '窗口未启动 · 不倒计时';
    if (evidence?.State === 'expired') return '窗口已到期 · 等待实际请求启动';
    return '尚未确认窗口启动 · 不倒计时';
  }
  function manualWakeReason(account, config, summary, capabilities) {
    if (!summary?.running || summary.host_enabled === false) return '后台未运行或 CPA 插件已停用';
    if (!config?.execution?.model?.trim()) return '请先填写并保存唤醒模型，无需开启自动唤醒';
    if (capabilities?.StableAccountIdentity !== true || capabilities?.ExactAccountExecution !== true || capabilities?.QuotaAdapterReady !== true) return '宿主账号绑定、模型执行或额度接口未就绪';
    if (config.account_overrides?.[account.ID]?.paused === true) return '已暂停唤醒；恢复并保存后可手动唤醒';
    if (account.Task?.OperationID) return '已有请求待验证，不重复唤醒';
    return '';
  }
  function modelFailureMessage(code) {
    const messages = {
      model_callback_failed: 'CPA 模型执行回调失败，尚不能确认请求是否完成',
      host_callback_failed: 'CPA 宿主回调失败，尚不能确认请求是否完成',
      model_stream_missing: 'CPA 没有返回可读取的模型流',
      model_stream_read_failed: '读取模型响应流失败，尚不能确认完整结果',
      model_stream_frame_too_large: '模型响应事件超过大小限制，未完成结果解析',
      model_stream_canceled: '模型响应读取被中断，尚不能确认完整结果',
      model_stream_missing_completion: '模型流未识别到完成事件，不能仅凭请求成功确认窗口启动',
      model_stream_failed: '模型响应包含失败或未完成事件，请核对 CPA 对应请求的错误记录',
      invalid_exact_request: '模型请求缺少必要参数，请核对已保存的模型和内容',
      send_failed: '模型执行出现异常，尚不能确认请求是否完成',
      send_not_sent: '请求已确认未发送'
    };
    return Object.hasOwn(messages, code) ? messages[code] : '';
  }
  function quotaFailure(job) {
    const messages = {
      quota_credential_expired: '账号凭据已过期或上游拒绝认证，请在 CPA 刷新或重新登录账号',
      quota_credential_missing: '账号缺少有效的访问凭据', quota_proxy_invalid: '账号代理配置无效或字段冲突',
      quota_host_operation_failed: 'CPA 未能创建或取消查询操作，请检查宿主与插件接口兼容性',
      quota_host_http_failed: 'CPA HTTP 请求失败，请检查全局代理与上游连接',
      quota_native_query_failed: '账号代理请求失败，请检查代理与上游连接',
      quota_query_timeout: '额度查询超时，请检查连接或调整额度查询超时', quota_query_cancelled: '额度查询已取消',
      quota_upstream_forbidden: '上游拒绝访问，请检查账号状态与出口连接',
      quota_upstream_rate_limited: '上游限制查询频率，请稍后重试',
      quota_upstream_server_failed: '上游服务暂时异常，请稍后重试', quota_upstream_http_failed: '上游返回异常状态',
      quota_response_invalid: '额度响应格式无效，未更新已有额度', quota_response_too_large: '额度响应超过大小限制',
      quota_response_unrecognized: '额度响应缺少有效窗口或字段冲突，未更新已有额度',
      quota_management_auth_failed: '后台管理密钥认证失败，请检查 CPA 进程中的管理密钥',
      quota_management_endpoint_missing: '管理查询接口不存在，请检查管理地址与 CPA 版本',
      quota_management_http_failed: 'CPA 管理接口返回异常状态', quota_management_bridge_failed: '无法连接管理桥，请检查本地地址与服务状态',
      quota_management_bridge_missing: '管理桥地址或进程密钥未配置',
      account_identity_unavailable: '无法核对账号身份，请检查 CPA 账号凭据', account_binding_changed: '账号身份已更换，请重新查询',
      account_inventory_unavailable: '账号清单暂不可用，请检查 CPA 账号状态',
      account_read_failed: '账号清单读取失败，请检查 CPA 账号状态与宿主日志',
      capability_read_failed: '宿主能力读取失败，请检查 CPA 插件接口与宿主日志',
      durable_commit_failed: '后台状态未能保存，任务已停止；请检查存储与日志并重启后台',
      quota_read_failed: '可手动重试'
    };
    const stages = {credential: '读取凭据', proxy: '解析账号代理', operation_open: '创建宿主操作', operation_cancel: '取消宿主操作', host_http: '宿主请求', proxy_http: '账号代理请求', management_config: '管理桥配置', management_http: '管理接口', management_parse: '管理响应解析', upstream_http: '上游响应', parse: '额度解析', binding: '账号核对', query: '查询'};
    const detail = job.diagnostic || {}, parts = [];
    if (Object.hasOwn(stages, detail.stage)) parts.push(stages[detail.stage]);
    if (Number.isInteger(detail.http_status) && detail.http_status >= 100 && detail.http_status <= 599) parts.push('HTTP ' + detail.http_status);
    if (Number.isSafeInteger(detail.elapsed_ms) && detail.elapsed_ms >= 0) parts.push(detail.elapsed_ms + ' 毫秒');
    return (modelFailureMessage(job.last_error) || (Object.hasOwn(messages, job.last_error) ? messages[job.last_error] : '可手动重试')) + (parts.length ? '（' + parts.join(' · ') + '）' : '');
  }
  function sourceText(source) {
    const labels = {periodic: '定期查询', five_hour_reset: '5 小时到期', weekly_reset: '周到期', daily_schedule: '每日规则', manual_wake: '手动唤醒', manual_query: '手动查询', verification: '发送后验证', preflight: '发送前核对', generation_preflight: '生成前核对', prompt_generation: '用户确认生成内容', early_recovery: '双满额提前恢复'};
    const evidence = {cpa_response_headers: '用户响应头', cpa_websocket_quota: '用户 WebSocket 响应', cpa_wham_usage: '额度接口查询', cpa_execution_quota: '模块模型响应'};
    return (labels[source.trigger] || source.trigger || '未提供来源') + (source.config_path ? ' · ' + source.config_path : '') + (source.rule_id ? ' · 规则 ' + source.rule_id : '') + (source.event_id ? ' · 事件 ' + source.event_id : '') + (source.evidence_source ? ' · 证据：' + (evidence[source.evidence_source] || source.evidence_source) : ' · 无窗口证据');
  }
  function historyTimeRange(range, now = Date.now()) {
    if (range !== 'today' && range !== 'week') return {from: '', until: ''};
    const start = new Date(now);
    start.setHours(0, 0, 0, 0);
    if (range === 'week') start.setDate(start.getDate() - 6);
    return {from: start.toISOString(), until: ''};
  }
  const exported = {PanelError, createSession, createEditor, createPanelPreferences, createAccountPreferences, configChanges, changeValue, accountComparator, observationAge, resetTime, countdown, windowCountdown, manualWakeReason, apiBase, readPanelKey, quotaFailure, dailyTimeRange, sourceText, historyTimeRange, durationSeconds, hoursError, configFieldErrors};
  if (typeof module !== 'undefined' && module.exports) module.exports = exported;
  if (!root.document) return;
  const document = root.document, $ = id => document.getElementById(id);
  const editor = createEditor();
  const preferences = createPanelPreferences(root, apiBase(root.location));
  const accountPreferences = createAccountPreferences(root, apiBase(root.location));
  const expandedAccounts = new Set();
  const accountNodes = new Map();
  let renderedAccountOrder = [];
  let pendingQuotaQuery = null;
  let pendingManualWake = null;
  let noticeSource = '';
  let snapshotInstance = '', snapshotVersion = -1, readSequence = 0, appliedRead = 0, mutationEpoch = 0;
  const retiredInstances = new Set();
  let pollFailures = 0, pollController = null, taskPollKey = '', taskPollStartedAt = 0;
  let pendingGeneration = null, previewGeneration = '', historyCursor = 0, historyNextCursor = 0, historyLoaded = false, historySequence = 0;
  let planView = 'upcoming', historyPreviousCursors = [];
  let summary = null, runningConfig = null, accounts = {}, capabilities = {}, pollTimer = null, pollBusy = false, actionBusy = false, diskMatchesRunning = true;
  const session = createSession(root.fetch.bind(root), clearView, undefined, apiBase(root.location));
  function el(tag, text, className) {
    const item = document.createElement(tag);
    if (text !== undefined) item.textContent = String(text);
    if (className) item.className = className;
    return item;
  }
  function notice(text, type, source) { noticeSource = source || ''; $('notice').textContent = text; $('notice').className = 'notice' + (type ? ' ' + type : ''); $('notice').hidden = !text; }
  function clearView() {
    if (pollTimer) root.clearTimeout(pollTimer);
    pollTimer = null; pollBusy = false; actionBusy = false;
    if (pollController) pollController.abort();
    pollController = null; pollFailures = 0; taskPollKey = ''; taskPollStartedAt = 0;
    $('auto-refresh').checked = false;
    $('background-refresh').checked = false; $('background-refresh').disabled = true;
    $('refresh-status').textContent = '自动刷新已关闭';
    $('quota-freshness').textContent = ''; $('quota-freshness-warning').hidden = true;
    $('config-changes').replaceChildren(); $('changes-preview').open = false;
    snapshotInstance = ''; snapshotVersion = -1; readSequence++; appliedRead = readSequence; mutationEpoch++; retiredInstances.clear();
    summary = null; runningConfig = null; accounts = {}; capabilities = {}; editor.clear(); diskMatchesRunning = true;
    expandedAccounts.clear();
    accountNodes.clear(); renderedAccountOrder = [];
    pendingQuotaQuery = null;
    pendingManualWake = null;
    pendingGeneration = null; previewGeneration = ''; historyLoaded = false; historyCursor = 0; historyNextCursor = 0; historySequence++;
    historyPreviousCursors = []; $('history-next').disabled = true; $('history-prev').disabled = true;
    $('history-list').setAttribute('aria-busy', 'false');
    $('generation-preview').value = ''; $('generation-status').textContent = ''; $('history-status').textContent = ''; $('history-list').replaceChildren();
    $('workspace').hidden = true; $('login-section').hidden = false; $('management-key').value = ''; $('config-json').value = '';
    $('connection-status').textContent = '未连接'; $('connection-status').className = 'badge neutral'; $('disconnect').disabled = true;
    for (const id of ['metrics', 'schedule-info', 'hold-counts', 'account-list', 'daily-rules', 'capabilities-list', 'plan-list']) $(id).replaceChildren();
    $('plan-status').textContent = '';
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
  function report(error, source) { if (!['stale', 'disconnected', 'cancelled'].includes(error.code)) notice(errorMessage(error), 'error', source); }
  function reportAcceptedRead(error) {
    if (error.code === 'authentication') report(error);
    else if (!['stale', 'disconnected', 'cancelled'].includes(error.code)) notice('任务已接受；' + errorMessage(error), 'warning', 'snapshots');
  }
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
  const holds = {plugin_disabled: '模型唤醒未启用', account_inventory_unavailable: '账号清单暂不可用', account_disabled: 'CPA 账号不可用或已暂停唤醒', account_unavailable: 'CPA 仍报告账号暂时不可用，额度查询不会解除此状态', delivery_evidence_pending: '已发送，等待确认', quota_adapter_unavailable: '额度适配器不可用', foreground_busy: '账号正在处理前台请求', backoff: '失败退避中', active_five_hour: '5 小时窗口内', quota_exhausted: '额度耗尽', quota_unknown: '额度证据未知', not_due: '尚未到执行时间', outside_hours: '不在允许时段', target_satisfied: '人数比例容量已满足', credential_changed: '账号凭据已变化', weekly_reset_unconfirmed: '周重置尚未确认', weekly_reset_conflict: '周与短窗口证据冲突', no_concurrency_slot: '等待执行空位'};
  const states = {unknown: '未知', inactive: '未启动', active: '窗口内', expired: '已到期', pending: '待处理', waiting: '等待', reserved: '已预留', dispatch_intent: '准备发送', verifying: '核实窗口', awaiting_evidence: '等待证据', satisfied: '已满足', suspended: '已暂停'};
  const reasons = {five_hour_reset: '5 小时重置', weekly_reset: '周重置', daily_schedule: '每日时间规则', manual_wake: '手动唤醒'};
  function renderOverview() {
    if (!summary) return;
    const s = summary.scheduler || {}, cap = s.capacity || {}, cfg = runningConfig;
    const pool = Object.values(accounts || {});
    const known = pool.filter(a => Number.isFinite(a.FiveHour?.Remaining?.Value) && Number.isFinite(a.Weekly?.Remaining?.Value)).length;
    const active = pool.filter(a => a.FiveHour?.State === 'active' && (resetTime(a.FiveHour.ResetAt) || 0) > Date.now()).length;
    const cards = [['账号总数', summary.accounts ?? pool.length, 'CPA 中发现的 Codex 账号'], ['双窗口额度已知', known, '5 小时与周额度都有数值'], ['5 小时窗口内', active, '窗口尚未到重置时间'], ['额度待确认', s.quota_unknown_accounts ?? '—', '窗口或请求权限仍未知']];
    $('mode-banner').classList.toggle('wake-enabled', summary.enabled === true);
    $('mode-title').textContent = !summary.running ? '后台未运行' : summary.enabled ? '模型唤醒已开启' : '仅查看与查询额度';
    $('mode-description').textContent = !summary.running ? '当前显示已保存的数据；请在“诊断与高级”检查后台状态。' : summary.enabled ? '后台会按已保存的规则发送模型请求。额度查询由独立开关控制。' : '不会自动创建新的模型唤醒请求；明确确认的单账号手动唤醒仍可发送模型请求。用户响应被动更新，额度接口按查询设置运行。';
    $('mode-badge').textContent = '已应用配置';
    $('metrics').replaceChildren();
    for (const [label, value, hint] of cards) { const card = el('div', undefined, 'metric'); card.append(el('div', label, 'metric-label'), el('div', value, 'metric-value'), el('div', hint, 'metric-detail')); $('metrics').append(card); }
    details($('schedule-info'), [['近期用户范围', s.recent_users ?? '未知'], ['普通目标 / 已就绪', (cap.target ?? '—') + ' / ' + (cap.active ?? '—')], ['预留 / 缺口', (cap.reserved ?? '—') + ' / ' + (cap.missing ?? '—')], ['规则时区', cfg?.timezone], ['普通允许时段', Array.isArray(cfg?.ordinary_rules?.allowed_hours) ? cfg.ordinary_rules.allowed_hours.join('、') || '无' : '未知'], ['周任务限制', cfg ? '不受人数与普通时段限制' : '未知'], ['有效并发', summary.effective_concurrency], ['下一次账号清单检查', date(s.next_inventory_at)], ['后台快照时间', date(s.observed_at)]]);
    $('hold-counts').replaceChildren();
    const counts = Object.entries(s.hold_counts || {}).sort((a, b) => b[1] - a[1]);
    if (!counts.length) $('hold-counts').append(el('p', '当前没有报告的阻塞原因。', 'hint'));
    for (const [reason, count] of counts) { const row = el('div', undefined, 'hold-row'); row.append(el('span', holds[reason] || reason), badge(count, 'neutral')); $('hold-counts').append(row); }
    const diagnostic = s.last_quota_diagnostic;
    $('last-error').textContent = (summary.last_error ? '最近诊断：' + summary.last_error : '最近诊断：无错误') + (diagnostic ? '；最近额度失败：' + quotaFailure({last_error: diagnostic.code, diagnostic}) : '');
    renderQuotaQuery(); renderSchedulePlan(); renderGeneration();
    renderFreshness();
  }
  function renderFreshness() {
    const times = Object.values(accounts).flatMap(account => [account.FiveHour, account.Weekly].map(window => resetTime(window?.ConfirmedAt))).filter(time => time !== null);
    const missing = Object.values(accounts).filter(account => [account.FiveHour, account.Weekly].some(window => resetTime(window?.ConfirmedAt) === null)).length;
    const earliest = times.length ? Math.min(...times) : null, latest = times.length ? Math.max(...times) : null;
    $('quota-freshness').textContent = times.length ? '额度观测：' + (earliest === latest ? observationAge(new Date(latest).toISOString()) : '最早 ' + observationAge(new Date(earliest).toISOString()) + '，最新 ' + observationAge(new Date(latest).toISOString())) + (missing ? '；' + missing + ' 个账号观测不完整' : '') : '尚无额度观测，可查询上游获取；更新页面只读取已有数据。';
    const job = quotaJob(), failed = job.failed > 0 || job.phase === 'failed';
    $('quota-freshness-warning').hidden = !failed;
    $('quota-freshness-warning').textContent = failed ? '最近查询有失败' + (resetTime(job.finished_at) !== null ? '（' + date(job.finished_at) + '）' : '') + '，未成功更新的账号保留上次观测。' : '';
  }
  function activeJob(job) {
    if (job?.phase === 'blocked' && resetTime(job.finished_at || job[job.kind]?.finished_at) !== null) return false;
    return ['queued', 'running', 'blocked', 'dispatched', 'verifying'].includes(job?.phase);
  }
  function registryJob(job, kind) {
    const detail = {...(job[kind] || {}), job_id: job.job_id, phase: job.phase, account_id: job.account_id || '', blocked_reason: job.blocked_reason, retry_at: job.retry_at};
    if (detail.phase === 'blocked' && resetTime(detail.finished_at) !== null) detail.phase = 'skipped';
    return detail;
  }
  function trackedJob(pending, kind) {
    if (pending?.detail) return pending.detail;
    const jobs = summary?.scheduler?.jobs || [];
    for (let index = jobs.length - 1; index >= 0; index--) if (jobs[index].kind === kind) return registryJob(jobs[index], kind);
    return summary?.scheduler?.[kind] || {};
  }
  function quotaJob() { return trackedJob(pendingQuotaQuery, 'quota_query'); }
  function generationJob() { return trackedJob(pendingGeneration, 'prompt_generation'); }
  function startTracking(target) {
    return {target: target || '', instance_id: snapshotInstance, job_id: '', detail: {phase: 'queued', account_id: target || ''}};
  }
  function acceptedJob(pending, result) {
    if (typeof result?.job_id !== 'string' || !result.job_id) {
      pending.detail = {phase: 'unconfirmed', account_id: pending.target};
      throw new PanelError('invalid_response');
    }
    pending.job_id = result.job_id;
    pending.detail = {phase: 'queued', account_id: pending.target};
  }
  function reconcileJob(pending, kind) {
    if (!pending?.job_id) return;
    const job = summary?.scheduler?.jobs?.find(item => item.job_id === pending.job_id && item.kind === kind);
    if (job) {
      pending.instance_id = snapshotInstance;
      pending.detail = registryJob(job, kind);
    } else if (activeJob(pending.detail)) pending.detail = {phase: pending.instance_id !== snapshotInstance ? 'interrupted' : 'lost', account_id: pending.target};
    if (!summary.running && activeJob(pending.detail)) pending.detail = {phase: 'interrupted', account_id: pending.target};
  }
  function jobProblem(job) {
    if (job.phase === 'interrupted') return '后台已停止或重启，任务已中断；请核对结果后再操作';
    if (job.phase === 'lost') return '任务记录已不可用，请核对账号数据后再操作';
    if (job.phase === 'unconfirmed') return '操作结果尚未确认，请手动更新页面核对；不会自动重试';
    const sendCode = job.last_error || job.error_code;
    if (['unknown', 'verifying'].includes(job.phase) && !['send_not_sent', 'invalid_exact_request'].includes(sendCode) && modelFailureMessage(sendCode)) return modelFailureMessage(sendCode) + '；继续核对窗口证据，不自动重发';
    if (job.phase === 'failed') return '任务失败，不自动重试：' + quotaFailure({last_error: job.last_error || job.error_code || job.blocked_reason, diagnostic: job.diagnostic});
    if (activeJob(job) && job.blocked_reason) return '暂时阻塞：' + ((Object.hasOwn(holds, job.blocked_reason) ? holds[job.blocked_reason] : '') || quotaFailure({last_error: job.blocked_reason})) + (resetTime(job.retry_at) !== null ? ' · 下次检查 ' + date(job.retry_at) : '');
    return '';
  }
  function quotaQueryReason() {
    if (!summary?.running) return '后台未运行';
    if (capabilities.QuotaAdapterReady !== true) return bridgeDescription() + '，请查看诊断';
    return '';
  }
  function jobDuration(job) {
    const start = resetTime(job.started_at), end = resetTime(job.finished_at);
    const elapsed = Number.isFinite(job.timing?.total_ms) && job.timing.total_ms >= 0 ? job.timing.total_ms : start !== null && end !== null ? end - start : null;
    return elapsed !== null && elapsed >= 0 ? ' · 耗时 ' + Number((elapsed / 1000).toFixed(1)) + ' 秒' : '';
  }
  function renderQuotaQuery() {
    const query = runningConfig?.quota_query || {}, job = quotaJob();
    const running = activeJob(job);
    const phases = {queued: '等待查询', running: '正在查询', completed: '查询结束', cancelled: '已取消', unavailable: '额度查询接口未就绪'};
    const progress = jobProblem(job) || (job.phase ? (job.account_id ? '单个账号 · ' : '全部账号 · ') + (phases[job.phase] || job.phase) + ' · 成功 ' + (job.completed || 0) + '，失败 ' + (job.failed || 0) + '，跳过 ' + (job.skipped || 0) + ' / 共 ' + (job.total || 0) + ' 个账号' + (job.phase === 'completed' ? jobDuration(job) : '') + (job.failed && job.last_error ? '；最近失败：' + quotaFailure(job) : '') : '尚未执行手动查询');
    const reason = quotaQueryReason();
    const periodic = [['five_hour', '5 小时'], ['weekly', '7 天']].filter(([window]) => query[window]?.periodic_enabled !== false).map(([window, label]) => label + '（' + (query[window]?.interval && query[window].interval !== '0s' ? query[window].interval : query.interval || '30s') + '）');
    $('quota-query-status').textContent = '定期查询：' + (query.automatic ? periodic.join(' / ') || '无参与策略' : '关闭') + ' · 5 小时到期查询：' + (query.on_five_hour_expiry ? '开启' : '关闭') + ' · 周到期查询：' + (query.on_weekly_expiry ? '开启' : '关闭') + ' · ' + (reason || progress);
    $('quota-query').disabled = !session.connected() || actionBusy || !!reason || running;
  }
  function renderSchedulePlan() {
    const plan = summary?.scheduler?.plan;
    $('plan-list').replaceChildren();
    if (!plan?.enabled) {
      $('plan-status').textContent = '自动生成未来计划已关闭。开启并保存后展示生效配置的计划；不会因此开启查询或模型请求。';
      return;
    }
    $('plan-status').textContent = '已应用配置 · 生成于 ' + date(plan.generated_at) + ' · 未来 ' + plan.horizon_days + ' 天，至 ' + date(plan.until) + (plan.truncated ? ' · 仅显示最早 256 项' : '') + ' · 预计计划随额度证据更新，不保证实际执行';
    if (!Array.isArray(plan.entries) || !plan.entries.length) {
      $('plan-list').append(el('p', '当前没有可展示计划。请检查独立开关或先查询账号；未知窗口不会推算新的五小时或周周期。', 'hint'));
      return;
    }
    const triggers = {periodic: '定期额度查询', five_hour_reset: '5 小时到期', weekly_reset: '周额度到期', daily_schedule: '每日时间点 / 随机时段', verification: '发送后窗口核实'};
    let lastDay = '';
    const entries = [...plan.entries].sort((first, second) => new Date(first.earliest_at) - new Date(second.earliest_at));
    for (const entry of entries) {
      const day = date(entry.earliest_at).split(' ')[0];
      if (day !== lastDay) {
        $('plan-list').append(el('li', day, 'timeline-day'));
        lastDay = day;
      }
      const row = el('li', undefined, 'card plan-item plan-' + (entry.kind === 'wake' ? 'wake' : 'query') + (entry.status === 'blocked' ? ' plan-blocked' : ''));
      const window = {five_hour: ' · 5 小时', weekly: ' · 7 天'}[entry.window] || '';
      row.append(el('h3', (entry.kind === 'wake' ? '模型唤醒 · ' : '额度查询 · ') + (triggers[entry.trigger] || entry.trigger) + window), el('p', entry.account_id || '账号池目标' + (entry.rule_id ? ' · ' + entry.rule_id : ''), 'account-id'));
      const range = date(entry.earliest_at) + (entry.latest_at && entry.latest_at !== entry.earliest_at ? ' — ' + date(entry.latest_at) : '');
      row.append(el('p', range + (entry.repeat_every ? ' · 每 ' + entry.repeat_every + ' 循环' : '')), el('p', ({scheduled: '已分配时间', estimated: '预计时间 / 随机范围', blocked: '暂不可执行'}[entry.status] || entry.status) + (entry.model ? ' · 模型 ' + entry.model : ''), 'hint'), el('p', entry.detail, 'hint'));
      renderSources(row, entry.sources || [{trigger: entry.trigger}]);
      $('plan-list').append(row);
    }
  }
  function renderCapabilities() {
    const labels = {StableAccountIdentity: '稳定账号身份', ExactAccountExecution: '指定账号发送', QuotaAdapterReady: '额度适配器就绪', ActivityIdentityAtStart: '请求开始时识别用户范围', ActivityIdentityAtCompletion: '请求完成时识别用户范围', ForegroundGuard: '前台请求与调度互斥保护'};
    details($('capabilities-list'), Object.entries(labels).map(([key, label]) => [label, capabilities[key] === true ? (key === 'QuotaAdapterReady' ? '已就绪' : '支持') : capabilities[key] === false ? (key === 'QuotaAdapterReady' ? '未就绪' : '不支持') : '未知']));
    $('bridge-status').textContent = capabilities.QuotaAdapterReady === true ? '后台额度查询配置已就绪' : bridgeDescription();
    const hostStatus = capabilities.quota_bridge?.host_http_status;
    const hostLabels = {unchecked: '宿主查询接口尚未检查', verified: '宿主操作创建与取消已验证', operation_open_verified: '宿主操作创建已验证，取消尚未单独检查', operation_open_failed: '宿主查询操作创建失败', operation_cancel_failed: '宿主查询操作取消失败', callback_missing: '宿主回调不可用'};
    if (capabilities.quota_bridge?.mode === 'host') $('bridge-status').textContent += ' · ' + (hostLabels[hostStatus] || '宿主查询接口尚未检查');
    $('quota-connection-check').hidden = capabilities.quota_bridge?.mode !== 'host';
    $('quota-connection-check').disabled = !session.connected() || actionBusy;
    const env = capabilities.quota_bridge?.management_key_env || 'management_key_env 指定的变量';
    const help = {
      host_callback: '当前使用 CPA 宿主账号凭据查询，不需要配置后台管理地址或管理密钥环境变量。凭据由 CPA 维护；账号专用代理优先，否则沿用 CPA 全局代理。',
      missing_management_url: '在 CPA 主配置的 plugins.configs.codex-window-refresh 中设置 management_url 和 management_key_env，然后重启 CPA。',
      missing_management_key_env: '在 CPA 主配置的 plugins.configs.codex-window-refresh 中设置 management_key_env（环境变量名），然后重启 CPA。',
      missing_management_key: '将现有 CPA 管理密钥设置到 CPA 进程的 ' + env + ' 环境变量，然后重启 CPA。systemd 需给实际服务配置 EnvironmentFile（用户服务用 --user 管理）；Docker 需将变量传入容器。',
      invalid_management_url: '检查 CPA 主配置中的 management_url。支持 http(s) 服务基址、/v0/management、/v0/management/api-call 或完整 /v8/management/requests/api-call；修改后重启 CPA。',
      configured: '管理地址及密钥环境变量已配置。接口能力随页面数据刷新；查询失败时再检查管理密钥、地址及上游连接。'
    };
    $('bridge-help').textContent = help[capabilities.quota_bridge?.status] || '接口能力随页面数据刷新，可手动更新或开启自动刷新。若持续未就绪，请检查查询方式并查看 CPA 日志。';
  }
  function renderSources(container, sources) {
    for (const source of sources) {
      container.append(el('p', '来源：' + sourceText(source) + (resetTime(source.evidence_at) !== null ? ' · 观测于 ' + date(source.evidence_at) : '') + (resetTime(source.reset_at) !== null ? ' · 原边界 ' + date(source.reset_at) : ''), 'hint task-source'));
    }
  }
  function renderGeneration() {
    const select = $('generation-account'), selected = select.value;
    select.replaceChildren(el('option', '选择可用账号'));
    select.children[0].value = '';
    for (const [id, account] of Object.entries(accounts)) {
      if (!account.Enabled || runningConfig?.account_overrides?.[id]?.paused) continue;
      const option = el('option', id); option.value = id; select.append(option);
    }
    select.value = selected;
    const job = generationJob();
    const phases = {queued: '等待生成', running: '正在核对账号额度', dispatched: '模型正在生成', completed: '生成完成，尚未应用到草稿', failed: '生成失败，不自动重试', skipped: '未发送，安全条件不满足', unknown: '送达不确定，不自动重试'};
    $('generation-status').textContent = jobProblem(job) || (job.phase ? (phases[job.phase] || '生成状态待确认') + (job.error_code ? ' · ' + quotaFailure({last_error: job.error_code}) : '') : '仅在点击并确认后调用一次模型；生成结果不会自动保存。');
    const running = activeJob(job);
    $('generation-start').disabled = !session.connected() || actionBusy || !!running || summary?.host_enabled === false || !summary?.running || capabilities.QuotaAdapterReady !== true || capabilities.ExactAccountExecution !== true;
    if (job?.phase === 'completed' && Array.isArray(job.candidates) && previewGeneration !== job.request.request_id) {
      $('generation-preview').value = job.candidates.join('\n'); previewGeneration = job.request.request_id;
    }
    $('generation-apply').disabled = actionBusy || editor.rawDirty() || !previewGeneration || previewGeneration !== job?.request?.request_id || job?.phase !== 'completed' || !session.connected();
  }
  async function readHistory(cursor, signal) {
    if (!session.connected()) return;
    const generation = session.generation(), sequence = ++historySequence;
    $('history-list').setAttribute('aria-busy', 'true');
    $('history-next').disabled = true; $('history-prev').disabled = true;
    $('history-status').className = 'hint';
    if (!signal) $('history-status').textContent = '正在读取执行历史…';
    try {
      const query = new URLSearchParams({limit: '50'});
      if (cursor) query.set('cursor', String(cursor));
      for (const id of ['history-from', 'history-until']) $(id).setAttribute('aria-invalid', 'false');
      for (const [id, name] of [['history-account', 'account_id'], ['history-kind', 'kind'], ['history-result', 'status'], ['history-from', 'from'], ['history-until', 'until']]) {
        const value = $(id).value.trim();
        if (!value) continue;
        if (['from', 'until'].includes(name) && (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/.test(value) || !Number.isFinite(Date.parse(value)))) {
          $(id).setAttribute('aria-invalid', 'true');
          $('history-status').textContent = (name === 'from' ? '开始' : '结束') + '时间需包含日期、时分秒和时区，例如 2026-10-14T00:00:00+08:00。';
          $('history-status').className = 'hint history-error';
          $(id).focus?.(); return;
        }
        query.set(name, value);
      }
      if (query.has('from') && query.has('until') && Date.parse(query.get('from')) > Date.parse(query.get('until'))) {
        $('history-until').setAttribute('aria-invalid', 'true'); $('history-until').focus?.();
        $('history-status').textContent = '结束时间不能早于开始时间。'; $('history-status').className = 'hint history-error'; return;
      }
      const page = await session.request('/history?' + query, 'GET', undefined, signal);
      if (generation !== session.generation() || signal?.aborted || sequence !== historySequence) return;
      if (!validObject(page) || !Array.isArray(page.entries)) throw new PanelError('invalid_response');
      if (!cursor) historyPreviousCursors = [];
      else if (cursor !== historyCursor && cursor === historyPreviousCursors[historyPreviousCursors.length - 1]) historyPreviousCursors.pop();
      else if (cursor !== historyCursor) historyPreviousCursors.push(historyCursor);
      historyLoaded = true; historyCursor = cursor; historyNextCursor = page.next_cursor || 0;
      $('history-list').replaceChildren();
      const statuses = {completed: '成功', failed: '失败', skipped: '跳过', dispatched: '已发送', verifying: '已发送待验证', verified: '已验证', unknown: '送达不确定'};
      const kinds = {query: '额度查询', wake: '模型唤醒', generation: '内容生成', reset: '提前恢复事件'};
      for (const entry of page.entries) {
        const row = el('li', undefined, 'card plan-item plan-' + (entry.kind === 'wake' || entry.kind === 'generation' ? 'wake' : 'query'));
        row.append(el('h3', (kinds[entry.kind] || entry.kind) + ' · ' + (statuses[entry.status] || entry.status)), el('p', entry.account_id, 'account-id'), el('p', date(entry.started_at) + (resetTime(entry.finished_at) !== null ? ' — ' + date(entry.finished_at) : ' · 结果尚未确认')));
        if (entry.model) row.append(el('p', '模型：' + entry.model, 'hint'));
        if (entry.prompt) row.append(el('p', '实际发送：' + entry.prompt, 'history-prompt'));
        if (entry.error_code) row.append(el('p', (modelFailureMessage(entry.error_code) ? modelFailureMessage(entry.error_code) + '；' : '') + '结果代码：' + entry.error_code, 'hint'));
        if (entry.operation_id) row.append(el('p', '操作：' + entry.operation_id, 'hint'));
        renderSources(row, entry.sources || []); $('history-list').append(row);
      }
      if (!page.entries.length) $('history-list').append(el('li', '没有符合当前筛选条件的执行记录。可以清空筛选，或调整账号、日期和结果。', 'card history-empty'));
      $('history-status').textContent = '第 ' + (historyPreviousCursors.length + 1) + ' 页 · 本页 ' + page.entries.length + ' 条 · 保留 ' + page.retention_days + ' 天，最多 ' + page.maximum_entries + ' 条 · 重启后保留';
      if (noticeSource === 'history') notice('');
    } catch (error) {
      if (generation === session.generation() && sequence === historySequence && !signal?.aborted) {
        $('history-status').textContent = '历史读取失败，已保留上次结果。可稍后重新读取。'; $('history-status').className = 'hint history-error';
        report(error, 'history');
      }
    } finally {
      if (generation === session.generation() && sequence === historySequence) {
        $('history-list').setAttribute('aria-busy', 'false');
        $('history-next').disabled = !historyNextCursor;
        $('history-prev').disabled = !historyPreviousCursors.length;
      }
    }
  }
  function historyFiltersChanged() {
    historyLoaded = false; historyNextCursor = 0; historyPreviousCursors = []; historySequence++;
    $('history-list').setAttribute('aria-busy', 'false');
    $('history-next').disabled = true; $('history-prev').disabled = true;
    $('history-status').className = 'hint'; $('history-status').textContent = '筛选已更改，点击“筛选并读取最新历史”查看结果。';
  }
  for (const id of ['history-account', 'history-kind', 'history-result', 'history-from', 'history-until']) {
    $(id).addEventListener('input', historyFiltersChanged);
    $(id).addEventListener('change', historyFiltersChanged);
  }
  for (const [id, range] of [['history-today', 'today'], ['history-week', 'week']]) $(id).addEventListener('click', () => {
    const bounds = historyTimeRange(range); $('history-from').value = bounds.from; $('history-until').value = bounds.until;
    historyFiltersChanged(); void readHistory(0);
  });
  $('history-failed').addEventListener('click', () => { $('history-result').value = 'failed'; historyFiltersChanged(); void readHistory(0); });
  $('history-clear').addEventListener('click', () => {
    for (const id of ['history-account', 'history-kind', 'history-result', 'history-from', 'history-until']) $(id).value = '';
    historyFiltersChanged(); void readHistory(0);
  });
  $('history-refresh').addEventListener('click', () => void readHistory(0));
  $('history-prev').addEventListener('click', () => { if (historyPreviousCursors.length) void readHistory(historyPreviousCursors[historyPreviousCursors.length - 1]); });
  $('history-next').addEventListener('click', () => { if (historyNextCursor) void readHistory(historyNextCursor); });
  $('generation-start').addEventListener('click', async () => {
    if ($('generation-start').disabled) return;
    const accountID = $('generation-account').value, model = $('generation-model').value.trim() || runningConfig?.execution?.model?.trim(), count = Number($('generation-count').value);
    const target = $('generation-target').value || 'default';
    if (!accountID || !model || !Number.isInteger(count) || count < 5 || count > 50) { report(new PanelError('invalid_config')); return; }
    if (!root.confirm('将使用账号 ' + accountID + ' 和模型 ' + model + ' 生成 ' + count + ' 条候选短句。这会发送真实模型请求、消耗额度，可能启动五小时窗口。生成后仍需预览并保存。继续？')) return;
    const generation = session.generation();
    const request = {request_id: root.crypto?.randomUUID ? root.crypto.randomUUID() : 'prompt-' + Date.now() + '-' + Math.random().toString(16).slice(2), account_id: accountID, model, count, target};
    pendingGeneration = startTracking(accountID); pendingGeneration.job_id = request.request_id;
    let accepted = false;
    busy(true); renderGeneration();
    try {
      const result = await session.request('/prompt-generation', 'POST', request);
      if (generation !== session.generation()) return;
      mutationEpoch++; accepted = true; acceptedJob(pendingGeneration, result);
      notice('已安排内容生成。正在更新进度；生成结果需要预览并保存。');
      await readSnapshots();
    } catch (error) {
      if (error.code === 'authentication') { report(error); return; }
      if (generation !== session.generation()) return;
      if (!accepted && error.code !== 'write_unconfirmed') pendingGeneration = null;
      else if (!accepted) pendingGeneration.detail.phase = 'unconfirmed';
      if (accepted) reportAcceptedRead(error); else report(error);
    } finally { if (generation === session.generation()) { busy(false); renderGeneration(); schedulePoll(); } }
  });
  $('generation-apply').addEventListener('click', () => {
    if ($('generation-apply').disabled) return;
    try {
      const job = generationJob();
      if (job?.request?.request_id !== previewGeneration) throw new PanelError('invalid_config');
      const pool = [...new Set($('generation-preview').value.split(/\r?\n/).map(value => value.trim()).filter(Boolean))];
      if (!pool.length || pool.length > 50 || pool.some(value => new TextEncoder().encode(value).length > 256)) throw new PanelError('invalid_config');
      const path = {default: 'execution.prompt_pool', five_hour: 'reset_rules.five_hour.prompt_pool', weekly: 'reset_rules.weekly.prompt_pool'}[job.request.target];
      if (!path) throw new PanelError('invalid_config');
      editor.update(path, pool);
      for (const [control, field] of validationControls) if (field === path) { control.value = pool.join('\n'); pendingFormInputs.delete(control); pendingParseErrors.delete(control); }
      editor.markFormDirty(pendingFormInputs.size > 0); validateForm(false); draftChanged(); renderAccounts(); notice('候选已替换对应短句池草稿；请检查后保存并应用，不会自动保存。');
    } catch (error) { report(error); }
  });
  function bridgeDescription() {
    return {host_callback: '宿主额度查询等待后台确认', missing_management_url: '未配置后台管理接口地址', missing_management_key_env: '未配置管理密钥环境变量名', missing_management_key: 'CPA 进程未读取到管理密钥环境变量', invalid_management_url: '后台管理接口地址配置无效', configured: '额度查询配置已具备，等待后台确认'}[capabilities.quota_bridge?.status] || '额度查询接口未就绪';
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
    const observed = el('div', '额度观测：' + observationAge(evidence.ConfirmedAt), 'window-time observation-age');
    observed.dataset.observed = evidence.ConfirmedAt || ''; observed.title = '额度观测时间：' + date(evidence.ConfirmedAt);
    cell.append(el('div', source, 'quota-source ' + kind), observed);
    if (evidence.State === 'active' || evidence.State === 'expired') cell.append(el('div', '重置 ' + date(evidence.ResetAt), 'window-time'));
    const timer = el('div', windowCountdown(evidence), 'countdown');
    if (evidence.State === 'active') timer.dataset.reset = evidence.ResetAt || '';
    cell.append(timer);
    return cell;
  }
  function renderAccounts(forceOrder) {
    const cfg = editor.value(), search = $('account-search').value.toLowerCase(), filter = $('account-filter').value;
    const list = $('account-list'), selection = root.getSelection?.();
    const interacting = list.contains?.(document.activeElement) || (selection && !selection.isCollapsed && (list.contains?.(selection.anchorNode) || list.contains?.(selection.focusNode)));
    const all = Object.entries(accounts || {}).sort(accountComparator($('account-sort').value));
    $('account-list').classList.toggle('compact', $('account-layout').value === 'compact');
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
    if (interacting && forceOrder !== true) {
      const positions = new Map(renderedAccountOrder.map((id, index) => [id, index]));
      visible.sort(([a], [b]) => (positions.get(a) ?? Infinity) - (positions.get(b) ?? Infinity));
    }
    $('account-count').textContent = '显示 ' + visible.length + ' / ' + all.length + ' 个账号';
    const liveIDs = new Set(visible.map(([id]) => id));
    for (const [id, node] of accountNodes) if (!liveIDs.has(id)) { node.remove?.(); accountNodes.delete(id); expandedAccounts.delete(id); }
    const nativeDOM = typeof list.insertBefore === 'function';
    if (!nativeDOM || !visible.length || !accountNodes.size) list.replaceChildren();
    if (!visible.length) list.append(el('div', all.length ? '没有符合筛选条件的账号。' : '暂无账号；等待调度器读取 CPA 账号清单。', 'empty'));
    for (const [id, a] of visible) {
      const existing = accountNodes.get(id);
      if (existing && nativeDOM) {
        const disclosure = existing.querySelector('.account-disclosure');
        if (disclosure?.open) expandedAccounts.add(id); else expandedAccounts.delete(id);
      }
      const paused = cfg?.account_overrides && Object.hasOwn(cfg.account_overrides, id) && cfg.account_overrides[id]?.paused === true;
      const card = el('article', undefined, 'card account-card'), header = el('div', undefined, 'account-head'), name = el('div');
      card.dataset.accountId = id;
      name.append(el('div', id, 'account-id'));
      const runningPaused = runningConfig?.account_overrides && Object.hasOwn(runningConfig.account_overrides, id) && runningConfig.account_overrides[id]?.paused === true;
      const pauseChanged = paused !== Boolean(runningPaused);
      const accountStatus = pauseChanged ? (paused ? '待保存：暂停唤醒' : '待保存：恢复唤醒') : paused ? '唤醒已暂停' : ({disabled: 'CPA 账号已禁用', unavailable: 'CPA 账号暂时不可用', binding_unavailable: 'CPA 账号绑定不可用'}[a.Availability] || (a.Enabled ? '账号已启用' : 'CPA 账号不可用'));
      header.append(name, badge(accountStatus, paused || pauseChanged || !a.Enabled ? 'warning' : '')); card.append(header);
      if (!a.Enabled && !runningPaused) card.append(el('p', 'CPA 状态约每 30 秒检查；页面自动刷新默认关闭。额度查询不解除 CPA 禁用或暂时不可用，手动唤醒会重新核对。', 'hint'));
      const windows = el('div', undefined, 'window-grid'); windows.append(windowCell('5 小时额度', a.FiveHour), windowCell('周额度', a.Weekly)); card.append(windows);
      const queryRow = el('div', undefined, 'account-query'), job = quotaJob();
      const querying = activeJob(job), selected = job.account_id === id;
      const reason = quotaQueryReason();
      const queryButton = el('button', querying && selected ? '正在查询…' : '查询此账号额度', 'quiet');
      queryButton.type = 'button'; queryButton.dataset.quotaAccount = id;
      queryButton.disabled = !session.connected() || actionBusy || !!reason || querying || activeJob(manualWakeJob());
      queryButton.title = reason || (querying ? '请等待当前手动查询结束' : '只访问此账号的额度接口，不发送模型请求');
      queryButton.addEventListener('click', () => { if (!queryButton.disabled) void requestQuotaQuery(id); });
      let queryText = reason || '仅查询额度，不发送模型请求';
      if (selected && job.phase) {
        queryText = jobProblem(job) || ({queued: '已排队，等待查询', running: '正在读取此账号额度', cancelled: '查询已取消', unavailable: '额度查询接口未就绪'}[job.phase]) || (job.failed ? '查询失败，' + quotaFailure(job) : job.skipped ? '未查询：账号已移除或绑定不存在' : '查询完成 · ' + date(job.finished_at) + jobDuration(job));
        if (job.failed > 0 || job.phase === 'failed') queryText += ' · 保留上次额度' + (resetTime(job.finished_at) !== null ? ' · 最近失败 ' + date(job.finished_at) : '');
      }
      const queryStatus = el('span', queryText, 'hint'); queryStatus.setAttribute('role', 'status');
      const actions = el('div', undefined, 'account-actions');
      queryRow.append(queryButton, queryStatus); actions.append(queryRow);
      const wakeRow = el('div', undefined, 'account-query'), wake = manualWakeJob();
      const waking = activeJob(wake), wakeSelected = wake.account_id === id;
      const wakeReason = manualWakeReason(a, runningConfig, summary, capabilities);
      const wakeButton = el('button', waking && wakeSelected ? '正在唤醒…' : '手动唤醒此账号', 'quiet');
      wakeButton.type = 'button'; wakeButton.dataset.wakeAccount = id;
      wakeButton.disabled = !session.connected() || actionBusy || !!wakeReason || waking || querying;
      wakeButton.title = wakeReason || '先查询最新额度和 CPA 状态，满足条件才发送真实模型请求，否则显示跳过原因';
      wakeButton.addEventListener('click', () => { if (!wakeButton.disabled) void requestManualWake(id); });
      let wakeText = wakeReason || '先查询额度与状态 · 符合条件才唤醒，否则跳过 · 需确认';
      if (wakeSelected && wake.phase) {
        const phases = {queued: '已排队，等待手动唤醒', running: '正在核对额度并唤醒', verifying: '请求已发送，等待服务端窗口证据', completed: '已确认窗口启动', unknown: '送达或窗口启动未确认，不自动重试', skipped: '未发送，安全条件不满足'};
        wakeText = jobProblem(wake) || phases[wake.phase] || '未完成唤醒：' + (holds[wake.last_error] || ({model_required: '请填写并保存唤醒模型', host_capability_unavailable: '宿主接口未就绪', dispatch_spacing: '发送间隔未到，请稍后重试', send_not_sent: '模型请求未送达，请稍后重试', credential_changed: '账号凭据已更换，请重新查询'}[wake.last_error]) || quotaFailure({last_error: wake.last_error}));
      }
      const wakeStatus = el('span', wakeText, 'hint'); wakeStatus.setAttribute('role', 'status');
      wakeRow.append(wakeButton, wakeStatus); actions.append(wakeRow); card.append(actions);
      const historyButton = el('button', '查看执行历史', 'text-button account-history'); historyButton.type = 'button'; historyButton.dataset.historyAccount = id;
      historyButton.addEventListener('click', () => openAccountHistory(id)); actions.append(historyButton);
      const task = a.Task, taskInfo = el('dl', undefined, 'details task-info'), disclosure = el('details', undefined, 'account-disclosure');
      disclosure.open = expandedAccounts.has(id);
      disclosure.addEventListener('toggle', () => { if (disclosure.isConnected) { if (disclosure.open) expandedAccounts.add(id); else expandedAccounts.delete(id); } });
      disclosure.append(el('summary', '唤醒任务与诊断' + (task ? ' · ' + (states[task.State] || task.State) : ' · 无任务')));
      const pendingReasons = task?.Intents?.filter(i => i.Status === 'pending').map(i => (reasons[i.Reason] || i.Reason) + (i.RuleID ? ' / ' + i.RuleID : '')) || [];
      const due = task?.Intents?.filter(i => i.Status === 'pending').map(i => resetTime(i.RandomizedDueAt)).filter(t => t !== null) || [];
      details(taskInfo, [['任务状态', task ? states[task.State] || task.State : '无任务'], ['触发原因', [...new Set(pendingReasons)].join('、') || '无'], ['阻塞原因', task?.HoldReason ? holds[task.HoldReason] || task.HoldReason : '无'], ['随机延迟结束', due.length ? date(new Date(Math.min(...due)).toISOString()) : '无'], ['下一次检查', resetTime(task?.NextCheckAt) !== null ? date(task.NextCheckAt) : '未安排'], ['下一次窗口核实', resetTime(task?.NextVerificationAt) !== null ? date(task.NextVerificationAt) : '未安排'], ['失败退避到', resetTime(a.BackoffUntil) !== null ? date(a.BackoffUntil) : '无'], ['请求权限', a.Permission === 'allowed' ? '允许' : a.Permission === 'blocked' ? '额度阻止' : '未知'], ['普通唤醒参与', a.OrdinaryParticipant ? '是' : '否'], ['周任务参与', a.WeeklyEnabled ? '是' : '否']]); disclosure.append(taskInfo);
      const footer = el('div', undefined, 'account-footer'); footer.append(el('span', '最近真实请求 ' + date(a.LastRealRequestAt), 'hint'));
      const pause = el('button', paused ? '恢复唤醒（草稿）' : '暂停唤醒（草稿）', 'quiet'); pause.type = 'button'; pause.disabled = actionBusy || editor.rawDirty();
      pause.addEventListener('click', () => { try {
        const overrides = editor.value()?.account_overrides;
        const currentPaused = overrides && Object.hasOwn(overrides, id) && overrides[id]?.paused === true;
        editor.pause(id, !currentPaused); draftChanged(); renderAccounts(); notice('已更新账号规则草稿；保存并应用后生效。');
      } catch (error) { report(error); } });
      footer.append(pause); disclosure.append(footer); card.append(disclosure);
      if (nativeDOM && existing) patchAccountNode(existing, card, selection);
      else accountNodes.set(id, card);
    }
    visible.forEach(([id], index) => {
      const node = accountNodes.get(id);
      if (nativeDOM) { if (list.children[index] !== node) list.insertBefore(node, list.children[index] || null); }
      else list.append(node);
    });
    renderedAccountOrder = visible.map(([id]) => id);
  }
  const validationControls = new Map(), validationMessages = new Map(), pendingFormInputs = new Set(), pendingParseErrors = new Map();
  let validationSequence = 0, formErrors = {};
  function setFieldError(control, message) {
    control.setAttribute('aria-invalid', message ? 'true' : 'false');
    let item = validationMessages.get(control);
    if (!item && message && control.parentElement) {
      item = el('small', '', 'validation-error'); item.id = 'validation-error-' + ++validationSequence;
      control.parentElement.append(item); validationMessages.set(control, item);
      const described = control.getAttribute('aria-describedby');
      control.setAttribute('aria-describedby', [described, item.id].filter(Boolean).join(' '));
    }
    if (item) { item.textContent = message || ''; item.hidden = !message; }
  }
  function validateForm(focus) {
    const cfg = editor.value(); if (!cfg) return true;
    formErrors = configFieldErrors(cfg);
    let first;
    for (const [control, path] of validationControls) {
      let message = pendingParseErrors.get(control) || formErrors[path] || '';
      if (pendingFormInputs.has(control)) message = control.type === 'number' && (!control.value.trim() || !Number.isFinite(Number(control.value))) ? '请填写有效数值，不能留空。' : message;
      setFieldError(control, message);
      if (message && !first && !control.disabled) first = control;
    }
    if (focus && (first || Object.keys(formErrors).length)) {
      if (first) {
        if (first.closest?.('#plan-upcoming')) activatePlanView('upcoming', false, false);
        const view = first.closest?.('.view'); if (view) activateTab(view.id.slice(4));
        for (let parent = first.parentElement; parent; parent = parent.parentElement) if (parent.tagName === 'DETAILS') parent.open = true;
        first.focus?.(); first.scrollIntoView?.({block: 'center', behavior: 'smooth'});
      } else { activateTab('capabilities'); $('advanced-editor').open = true; $('config-json').focus?.(); }
      notice('请先修正标出的配置字段；输入与草稿已保留。' + (!first ? '完整配置中仍有错误，请在 JSON 中检查。' : ''), 'error', 'validation');
    }
    if (!first && !Object.keys(formErrors).length && noticeSource === 'validation') notice('');
    return !first && !Object.keys(formErrors).length;
  }
  function registerValidation(control, path) {
    validationControls.set(control, path);
    control.addEventListener('input', () => { pendingFormInputs.add(control); editor.markFormDirty(true); updateDraftBadge(); });
  }
  function commitFormInput(control) {
    pendingFormInputs.delete(control); pendingParseErrors.delete(control); editor.markFormDirty(pendingFormInputs.size > 0); validateForm(false);
  }
  function resetFormValidation() {
    for (const [control, path] of validationControls) { setFieldError(control, ''); if (path.startsWith('daily_rules.')) validationMessages.delete(control); }
    validationControls.clear(); pendingFormInputs.clear(); pendingParseErrors.clear(); formErrors = {}; editor.markFormDirty(false);
    for (const control of document.querySelectorAll('[data-field]')) validationControls.set(control, control.dataset.field);
  }
  function updateDraftBadge() {
    const dirty = editor.dirty();
    const invalid = Object.keys(formErrors).length > 0 || pendingFormInputs.size > 0 || pendingParseErrors.size > 0;
    $('config-save').textContent = invalid ? '检查并定位错误' : '保存并应用';
    $('draft-status').textContent = dirty ? '有未保存的草稿' : '已与服务器同步'; $('draft-status').className = 'badge' + (dirty ? ' warning' : ' neutral');
    $('save-bar').hidden = !dirty && diskMatchesRunning && !actionBusy;
    $('config-save').disabled = actionBusy || (!dirty && diskMatchesRunning) || editor.rawDirty();
    $('save-help').textContent = invalid ? '有待检查或修正的输入；点击右侧按钮可定位错误，草稿会保留。' : !diskMatchesRunning ? '服务器规则文件与当前运行策略不同。保存将应用草稿；开启模型唤醒时可能发送模型请求。' : '保存会写入服务器规则文件并立即应用。额度查询不生成内容；开启模型唤醒时可能发送模型请求。';
    $('wake-draft-hint').textContent = editor.value()?.enabled ? '草稿：自动模型唤醒已开启。保存后按下方规则发送模型请求。' : '草稿：自动模型唤醒已关闭。单账号手动唤醒仍需明确确认，额度查询由独立设置控制。';
    const changes = draftChanges();
    $('changes-preview').hidden = editor.rawDirty() || !changes.length;
    $('changes-summary').textContent = '查看 ' + changes.length + ' 项配置变更';
    $('changes-context').textContent = diskMatchesRunning ? '对比载入时的服务器规则；所有页面的草稿一起保存。' : '对比当前生效规则；保存会应用文件草稿中的全部差异。';
    $('config-changes').replaceChildren();
    for (const change of changes) {
      const row = el('tr'); row.append(el('th', change.label), el('td', changeValue(change.before)), el('td', changeValue(change.after)));
      row.children[0].setAttribute('scope', 'row'); $('config-changes').append(row);
    }
  }
  function draftChanges() { return configChanges(diskMatchesRunning ? editor.baseline() : runningConfig, editor.value()); }
  function draftChanged() { $('config-json').value = JSON.stringify(editor.value(), null, 2); $('json-message').textContent = ''; updateDraftBadge(); renderOverview(); }
  function fieldValue(cfg, path) { let v = cfg; for (const part of path.split('.')) v = v?.[part]; return v === undefined && Object.hasOwn(settingDefaults, path) ? settingDefaults[path] : v; }
  function renderForm() {
    const cfg = editor.value(); if (!cfg) return;
    for (const input of document.querySelectorAll('[data-field]')) {
      const value = fieldValue(cfg, input.dataset.field);
      if (input.type === 'checkbox') input.checked = value === true;
      else input.value = Array.isArray(value) ? value.join(input.dataset.field.endsWith('.prompt_pool') ? '\n' : ', ') : value ?? '';
    }
    resetFormValidation(); renderDaily(); validateForm(false); draftChanged();
  }
  function renderDaily() {
    for (const [control, path] of validationControls) if (path.startsWith('daily_rules.')) { validationControls.delete(control); pendingFormInputs.delete(control); pendingParseErrors.delete(control); validationMessages.delete(control); }
    editor.markFormDirty(pendingFormInputs.size > 0);
    $('daily-rules').replaceChildren();
    const rawDaily = editor.value()?.daily_rules;
    const daily = Array.isArray(rawDaily) ? rawDaily : [];
    if (!daily.length) $('daily-rules').append(el('p', '尚未配置每日规则。09:00-09:00 表示固定时间点；09:00-09:10 表示该时段内随机时间。', 'hint'));
    daily.forEach((rule, index) => {
      const row = el('div', undefined, 'daily-row');
      function input(labelText, key, type) {
        const label = el('label', labelText), field = el('input'); field.type = type || 'text'; field.value = rule[key] ?? ''; if (type === 'number') { field.min = '0'; field.step = '1'; }
        registerValidation(field, 'daily_rules.' + index + '.' + key);
        field.addEventListener('change', () => {
          if (editor.rawDirty()) { report(new PanelError('unapplied_json')); return; }
          const value = editor.value(), parsed = type === 'number' ? Number(field.value) : field.value;
          if (type === 'number' && (!field.value.trim() || !Number.isFinite(parsed))) { pendingFormInputs.add(field); editor.markFormDirty(true); validateForm(false); updateDraftBadge(); return; }
          value.daily_rules[index][key] = parsed; editor.setDailyRules(value.daily_rules); commitFormInput(field); draftChanged();
        }); label.append(field); row.append(label);
      }
      input('规则 ID', 'id'); input('最低就绪账号数', 'minimum_ready_accounts', 'number');
      const timeControls = el('div', undefined, 'form-grid daily-times'), mode = el('select'), startTime = el('input'), endTime = el('input');
      const bounds = String(rule.random_time_range || '09:00-09:10').split('-');
      startTime.value = bounds[0] || '09:00'; endTime.value = bounds[1] || startTime.value;
      startTime.placeholder = '09:00'; endTime.placeholder = '09:10 / 24:00';
      for (const [value, text] of [['point', '固定时间点'], ['range', '随机时间段']]) { const option = el('option', text); option.value = value; mode.append(option); }
      mode.value = startTime.value === endTime.value ? 'point' : 'range'; endTime.disabled = mode.value === 'point'; endTime.dataset.fixed = String(endTime.disabled);
      for (const control of [startTime, endTime, mode]) registerValidation(control, 'daily_rules.' + index + '.random_time_range');
      function updateTime() {
        if (editor.rawDirty()) { report(new PanelError('unapplied_json')); return; }
        try {
          const range = dailyTimeRange(mode.value, startTime.value, endTime.value), value = editor.value();
          value.daily_rules[index].random_time_range = range; editor.setDailyRules(value.daily_rules);
          if (mode.value === 'point') endTime.value = startTime.value;
          endTime.disabled = mode.value === 'point'; endTime.dataset.fixed = String(endTime.disabled);
          for (const control of [mode, startTime, endTime]) { pendingFormInputs.delete(control); pendingParseErrors.delete(control); }
          editor.markFormDirty(pendingFormInputs.size > 0); validateForm(false); draftChanged();
        } catch (error) {
          endTime.disabled = mode.value === 'point'; endTime.dataset.fixed = String(endTime.disabled); pendingFormInputs.add(startTime); pendingParseErrors.set(startTime, '请输入有效的 HH:mm 时间；每日随机时段不跨午夜，结束可为 24:00。'); editor.markFormDirty(true);
          setFieldError(startTime, '请输入有效的 HH:mm 时间；每日随机时段不跨午夜，结束可为 24:00。');
          formErrors['daily_rules.' + index + '.random_time_range'] = '时间无效'; updateDraftBadge();
        }
      }
      mode.addEventListener('change', () => {
        if (mode.value === 'range' && endTime.value === startTime.value && /^(?:[01][0-9]|2[0-3]):[0-5][0-9]$/.test(startTime.value)) {
          const [hours, minutes] = startTime.value.split(':').map(Number), finish = Math.min(1440, hours*60 + minutes + 10);
          endTime.value = String(Math.floor(finish/60)).padStart(2, '0') + ':' + String(finish%60).padStart(2, '0');
        }
        updateTime();
      }); startTime.addEventListener('change', updateTime); endTime.addEventListener('change', updateTime);
      for (const [text, control] of [['时间类型', mode], ['时间点 / 开始时间', startTime], ['随机时段结束时间', endTime]]) { const label = el('label', text); label.append(control); timeControls.append(label); }
      const remove = el('button', '移除', 'quiet danger'); remove.type = 'button'; remove.addEventListener('click', () => { if ([...pendingFormInputs].some(control => validationControls.get(control)?.startsWith('daily_rules.'))) { validateForm(true); return; } if (editor.rawDirty()) { report(new PanelError('unapplied_json')); return; } const cfg = editor.value(); cfg.daily_rules.splice(index, 1); editor.setDailyRules(cfg.daily_rules); renderDaily(); validateForm(false); draftChanged(); }); row.append(remove);
      row.append(timeControls);
      const switches = el('div', undefined, 'daily-switches');
      for (const [key, text] of [['enabled', '启用规则'], ['catch_up', '错过窗口后补发']]) { const label = el('label', undefined, 'check-field'), field = el('input'); field.type = 'checkbox'; field.checked = rule[key] === true; registerValidation(field, 'daily_rules.' + index + '.' + key); field.addEventListener('change', () => { if (editor.rawDirty()) { report(new PanelError('unapplied_json')); return; } const cfg = editor.value(); cfg.daily_rules[index][key] = field.checked; editor.setDailyRules(cfg.daily_rules); commitFormInput(field); draftChanged(); }); label.append(field, document.createTextNode(text)); switches.append(label); }
      row.append(switches); $('daily-rules').append(row);
    });
  }
  function busy(value) {
    actionBusy = value;
    if (value) mutationEpoch++;
    for (const id of ['recheck', 'reload', 'draft-restore', 'json-apply', 'json-format', 'daily-add']) $(id).disabled = value;
    for (const field of document.querySelectorAll('#workspace [data-field], #config-json, #daily-rules button, #daily-rules input, #daily-rules select')) field.disabled = value || field.dataset.fixed === 'true';
    updateDraftBadge(); renderAccounts(); renderQuotaQuery(); renderCapabilities(); renderGeneration();
  }
  async function readSnapshots(signal) {
    const generation = session.generation(), sequence = ++readSequence, epoch = mutationEpoch;
    let snapshot;
    try { snapshot = await session.request('/snapshot', 'GET', undefined, signal); }
    catch (error) { if (error.code !== 'authentication' && (generation !== session.generation() || signal?.aborted || epoch !== mutationEpoch || sequence < appliedRead)) return false; throw error; }
    if (generation !== session.generation() || signal?.aborted || epoch !== mutationEpoch || sequence < appliedRead) return false;
    if (!validObject(snapshot) || typeof snapshot.instance_id !== 'string' || !snapshot.instance_id || !Number.isSafeInteger(snapshot.snapshot_seq) || snapshot.snapshot_seq < 0 || ![snapshot.summary, snapshot.accounts, snapshot.capabilities, snapshot.config].every(validObject) || !Array.isArray(snapshot.summary.scheduler?.jobs)) throw new PanelError('invalid_response');
    if (retiredInstances.has(snapshot.instance_id) || (snapshot.instance_id === snapshotInstance && snapshot.snapshot_seq < snapshotVersion)) return false;
    if (snapshotInstance && snapshotInstance !== snapshot.instance_id) retiredInstances.add(snapshotInstance);
    snapshotInstance = snapshot.instance_id; snapshotVersion = snapshot.snapshot_seq; appliedRead = sequence;
    summary = snapshot.summary; accounts = snapshot.accounts; capabilities = snapshot.capabilities; runningConfig = snapshot.config;
    reconcileJob(pendingQuotaQuery, 'quota_query'); reconcileJob(pendingManualWake, 'manual_wake'); reconcileJob(pendingGeneration, 'prompt_generation');
    renderOverview(); renderAccounts(); renderCapabilities(); $('updated-at').textContent = '页面更新于 ' + new Date().toLocaleTimeString('zh-CN', {hour12: false});
    if (noticeSource === 'snapshots') notice('');
    if (historyLoaded && !$('tab-plan').hidden && planView === 'history') await readHistory(historyCursor, signal);
    return true;
  }
  function activePollJobs() {
    return summary?.running === true ? [quotaJob(), manualWakeJob(), generationJob(), ...(summary?.scheduler?.jobs || [])].filter(activeJob) : [];
  }
  function shouldPoll() {
    return $('auto-refresh').checked || activePollJobs().length > 0;
  }
  function schedulePoll() {
    if (pollTimer) root.clearTimeout(pollTimer);
    pollTimer = null;
    $('background-refresh').disabled = !$('auto-refresh').checked;
    const jobs = activePollJobs(), key = [...new Set(jobs.map(job => job.job_id || (job.kind || 'task') + ':' + (job.account_id || '')))].sort().join('|');
    if (key !== taskPollKey) { taskPollKey = key; taskPollStartedAt = Date.now(); }
    if (!session.connected() || !shouldPoll()) { $('refresh-status').textContent = '自动刷新已关闭'; return; }
    if (root.navigator?.onLine === false) { $('refresh-status').textContent = '网络离线 · 联网后自动继续'; return; }
    if (document.hidden && !$('background-refresh').checked) { $('refresh-status').textContent = '标签页隐藏 · 返回后自动继续'; return; }
    const tracking = jobs.length > 0;
    const interval = tracking ? Date.now() - taskPollStartedAt < 10000 ? 500 : 1000 : 5000;
    const delay = pollFailures ? Math.min((tracking ? 1000 : 5000) * 2 ** (tracking ? pollFailures - 1 : pollFailures), 60000) : interval;
    $('refresh-status').textContent = pollFailures ? '读取暂未恢复 · ' + delay / 1000 + ' 秒后自动重试，保留已有数据' : tracking ? '手动任务进行中 · 每 ' + interval / 1000 + ' 秒更新进度' + (!$('auto-refresh').checked ? '，结束后停止' : '') : '持续刷新已开启' + ($('background-refresh').checked ? '（含后台标签页）' : '') + ' · 每 5 秒读取状态';
    if (!pollBusy) pollTimer = root.setTimeout(() => refreshSnapshots(false), delay);
  }
  function stopAutomaticPoll() {
    if (pollTimer) root.clearTimeout(pollTimer);
    pollTimer = null;
    if (pollController) pollController.abort();
    schedulePoll();
  }
  function resumeAutomaticPoll() {
    if (!session.connected() || !shouldPoll() || (document.hidden && !$('background-refresh').checked) || root.navigator?.onLine === false) { schedulePoll(); return; }
    if (pollBusy) return;
    if (pollTimer) root.clearTimeout(pollTimer);
    pollTimer = null;
    void refreshSnapshots(false);
  }
  async function refreshSnapshots(manual) {
    if (!session.connected() || pollBusy || (!manual && ((document.hidden && !$('background-refresh').checked) || !shouldPoll() || root.navigator?.onLine === false))) return;
    if (pollTimer) root.clearTimeout(pollTimer);
    pollTimer = null;
    const generation = session.generation(); pollBusy = true; $('refresh').disabled = true;
    const controller = new AbortController();
    if (!manual) pollController = controller;
    try {
      const applied = await readSnapshots(controller.signal);
      if (generation !== session.generation() || controller.signal.aborted || !applied) return;
      pollFailures = 0;
      if (manual) notice('状态快照已更新。');
    } catch (error) {
      if (error.code === 'authentication') { report(error); return; }
      if (generation !== session.generation() || controller.signal.aborted) return;
      pollFailures = Math.min(pollFailures + 1, 7);
      report(error, 'snapshots');
    } finally {
      controller.abort();
      if (generation === session.generation()) { if (pollController === controller) pollController = null; pollBusy = false; $('refresh').disabled = false; schedulePoll(); }
    }
  }
  async function loadEditor() {
    const generation = session.generation(), epoch = mutationEpoch, version = editor.version(), envelope = await session.request('/config-editor');
    if (generation !== session.generation() || epoch !== mutationEpoch || version !== editor.version()) return;
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
      // Probe once: CPA counts rejected authentication per request.
      await readSnapshots();
      if (generation !== session.generation()) return;
      await loadEditor();
      if (generation !== session.generation()) return;
      $('workspace').hidden = false; $('login-section').hidden = true; $('connection-status').textContent = '已连接'; $('connection-status').className = 'badge'; $('disconnect').disabled = false;
      const accountView = accountPreferences.value(); $('account-sort').value = accountView.sort; $('account-layout').value = accountView.layout;
      renderForm(); renderOverview(); renderAccounts(); renderCapabilities(); $('updated-at').textContent = '页面更新于 ' + new Date().toLocaleTimeString('zh-CN', {hour12: false});
      if (!diskMatchesRunning) notice('服务器规则文件尚未应用。这里显示的是文件草稿；运行概览中的启用状态来自当前调度器。', 'warning');
      const savedPreferences = preferences.value();
      $('auto-refresh').checked = savedPreferences.autoRefresh;
      $('background-refresh').checked = savedPreferences.backgroundRefresh;
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
  function activatePlanView(name, focus, load = true) {
    if (!['upcoming', 'history'].includes(name)) return;
    planView = name;
    for (const view of ['upcoming', 'history']) {
      const selected = view === name, button = $('plan-view-' + view);
      button.classList.toggle('active', selected); button.setAttribute('aria-selected', String(selected)); button.tabIndex = selected ? 0 : -1;
      $('plan-' + view).hidden = !selected;
    }
    if (focus) $('plan-view-' + name).focus?.();
    if (name === 'history' && load && session.connected()) void readHistory(0);
  }
  function openAccountHistory(accountID) {
    for (const id of ['history-kind', 'history-result', 'history-from', 'history-until']) $(id).value = '';
    $('history-account').value = String(accountID);
    historyFiltersChanged(); activatePlanView('history', false, false); activateTab('plan');
    $('plan-view-history').focus?.();
  }
  for (const name of ['upcoming', 'history']) {
    const button = $('plan-view-' + name);
    button.addEventListener('click', () => activatePlanView(name));
    button.addEventListener('keydown', event => {
      if (!['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(event.key)) return;
      event.preventDefault();
      activatePlanView(event.key === 'Home' ? 'upcoming' : event.key === 'End' ? 'history' : name === 'upcoming' ? 'history' : 'upcoming', true);
    });
  }
  function activateTab(name, focus) {
    const button = Array.from(document.querySelectorAll('[data-tab]')).find(tab => tab.dataset.tab === name);
    if (!button) return;
    document.querySelectorAll('[data-tab]').forEach(tab => { const selected = tab === button; tab.classList.toggle('active', selected); tab.setAttribute('aria-selected', String(selected)); tab.tabIndex = selected ? 0 : -1; });
    for (const view of document.querySelectorAll('.view')) view.hidden = view.id !== 'tab-' + name;
    if (focus) button.focus();
    if (name === 'plan' && planView === 'history' && session.connected()) void readHistory(0);
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
  for (const input of document.querySelectorAll('[data-field]')) {
    registerValidation(input, input.dataset.field);
    input.addEventListener('change', () => {
    try {
      if (editor.rawDirty()) throw new PanelError('unapplied_json');
      let value = input.type === 'checkbox' ? input.checked : input.type === 'number' ? Number(input.value) : input.value;
      if (input.type === 'number' && (!input.value.trim() || !Number.isFinite(value))) { pendingFormInputs.add(input); editor.markFormDirty(true); validateForm(false); updateDraftBadge(); return; }
      if (input.dataset.field === 'ordinary_rules.allowed_hours' || input.dataset.field.endsWith('.excluded_hours')) value = input.value.split(/[,，\n]/).map(x => x.trim()).filter(Boolean);
      if (input.dataset.field.endsWith('.prompt_pool')) value = input.value.split(/\r?\n/).map(text => text.trim()).filter(Boolean);
      editor.update(input.dataset.field, value); commitFormInput(input); draftChanged(); renderAccounts();
    } catch (error) { report(error); if (error.code !== 'unapplied_json') { pendingFormInputs.add(input); editor.markFormDirty(true); setFieldError(input, '此输入尚未应用，请修正后重试；草稿已保留。'); updateDraftBadge(); } }
    });
  }
  $('daily-add').addEventListener('click', () => {
    if ([...pendingFormInputs].some(control => validationControls.get(control)?.startsWith('daily_rules.'))) { validateForm(true); return; }
    if (editor.rawDirty()) { report(new PanelError('unapplied_json')); return; }
    const cfg = editor.value(); cfg.daily_rules = cfg.daily_rules || []; const ids = new Set(cfg.daily_rules.map(r => r.id)); let n = 1; while (ids.has('morning-' + n)) n++;
    cfg.daily_rules.push({id: 'morning-' + n, enabled: true, random_time_range: '09:00-09:10', minimum_ready_accounts: 1, catch_up: true}); editor.setDailyRules(cfg.daily_rules); renderDaily(); validateForm(false); draftChanged();
  });
  $('config-json').addEventListener('input', () => { editor.markRaw(); $('json-message').textContent = 'JSON 尚未应用到草稿。'; updateDraftBadge(); renderAccounts(); });
  $('json-apply').addEventListener('click', () => { try { const cfg = JSON.parse($('config-json').value); editor.replace(cfg); renderForm(); renderAccounts(); notice('JSON 已应用到本地草稿，尚未保存到服务器。'); } catch (_) { notice('JSON 无法解析或缺少完整规则结构。请保留完整字段并修正后再应用。', 'error'); } });
  $('json-format').addEventListener('click', () => { try { const value = JSON.parse($('config-json').value); if (!validObject(value)) throw new Error(); $('config-json').value = JSON.stringify(value, null, 2); } catch (_) { notice('JSON 无法解析或顶层不是对象。', 'error'); } });
  $('config-save').addEventListener('click', async () => {
    if (!session.connected() || actionBusy || editor.rawDirty() || (!editor.dirty() && diskMatchesRunning)) return;
    if (!validateForm(true)) { updateDraftBadge(); return; }
    const config = editor.value();
    const changes = draftChanges(), shortValue = value => { const text = changeValue(value).replace(/\s+/g, ' '); return text.length > 90 ? text.slice(0, 90) + '…' : text; };
    const changeSummary = changes.slice(0, 12).map(change => change.label + '：' + shortValue(change.before) + ' → ' + shortValue(change.after)).join('\n');
    const impact = config.enabled ? '模型唤醒已开启，调度器可能发送真实模型请求。' : '模型唤醒保持关闭，额度查询按独立设置运行；已发出的请求仍可能完成。';
    if (!root.confirm('保存并应用 ' + changes.length + ' 项配置变更？\n\n' + changeSummary + (changes.length > 12 ? '\n其余 ' + (changes.length - 12) + ' 项请在保存栏的变更预览中查看。' : '') + '\n\n' + impact)) return;
    const generation = session.generation(), version = editor.version(); let accepted = false; busy(true);
    try { const envelope = await session.request('/config', 'POST', {config, revision: editor.revision()}); if (generation !== session.generation()) return; mutationEpoch++; accepted = true; editor.saved(envelope, version); runningConfig = {...envelope.config, enabled: envelope.config.enabled === true && summary?.host_enabled !== false}; if (summary) summary.enabled = runningConfig.enabled; diskMatchesRunning = envelope.disk_matches_running !== false; renderForm(); renderOverview(); renderAccounts(); notice('规则已保存并应用。'); await readSnapshots(); }
    catch (error) { if (error.code === 'authentication') report(error); else if (generation === session.generation()) { if (accepted && error.code !== 'authentication') notice('规则已保存并应用；状态暂未读取成功，请更新页面数据核对。', 'warning', 'snapshots'); else report(error); } }
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
    const generation = session.generation(); let accepted = false; busy(true);
    try { await session.request(path, 'POST'); if (generation !== session.generation()) return; mutationEpoch++; accepted = true; if (id === 'reload') { await loadEditor(); } await readSnapshots(); notice(id === 'reload' ? '规则文件已重新读取并应用。' : '已通知调度器重新检查，后续执行由规则决定。'); }
    catch (error) { if (error.code === 'authentication') report(error); else if (generation === session.generation()) { if (accepted && error.code !== 'authentication') notice('操作已接受；状态暂未读取成功，请更新页面数据核对。', 'warning', 'snapshots'); else report(error); } }
    finally { if (generation === session.generation()) busy(false); }
  });
  $('refresh').addEventListener('click', () => refreshSnapshots(true));
  $('auto-refresh').addEventListener('change', () => {
    if (!$('auto-refresh').checked) $('background-refresh').checked = false;
    preferences.save($('auto-refresh').checked, $('background-refresh').checked);
    if (!$('auto-refresh').checked) stopAutomaticPoll(); else { pollFailures = 0; schedulePoll(); }
  });
  $('background-refresh').addEventListener('change', () => {
    preferences.save($('auto-refresh').checked, $('background-refresh').checked);
    if (document.hidden && !$('background-refresh').checked) stopAutomaticPoll(); else resumeAutomaticPoll();
  });
  function manualWakeJob() { return trackedJob(pendingManualWake, 'manual_wake'); }
  async function requestManualWake(accountID) {
    if (actionBusy || !session.connected()) return;
    if (!root.confirm('手动唤醒账号“' + accountID + '”？将先查询最新额度并核对 CPA 状态；符合条件时使用已保存的模型和消息发送真实模型请求并消耗额度，否则跳过并显示原因。不会开启自动唤醒。仅查询额度不会启动倒计时，窗口启动以服务端证据为准。')) return;
    const generation = session.generation();
    pendingManualWake = startTracking(accountID);
    let accepted = false;
    busy(true);
    try {
      const result = await session.request('/manual-wake', 'POST', {account_id: accountID});
      if (generation !== session.generation()) return;
      mutationEpoch++; accepted = true; acceptedJob(pendingManualWake, result);
      notice(result.status === 'already_queued' ? '已有此账号的手动唤醒任务，请等待结果。' : '已安排此账号的手动唤醒；将查询最新额度与状态，再决定唤醒或跳过。');
      await readSnapshots();
    } catch (error) {
      if (error.code === 'authentication') { report(error); return; }
      if (generation !== session.generation()) return;
      if (!accepted && error.code !== 'write_unconfirmed') pendingManualWake = null;
      else if (!accepted) pendingManualWake.detail.phase = 'unconfirmed';
      if (accepted) reportAcceptedRead(error);
      else if (['conflict', 'invalid_config', 'http_error'].includes(error.code)) notice('未能安排手动唤醒。请检查已保存模型、账号暂停状态、后台能力及待验证请求；不会自动重试。', 'error');
      else report(error);
    } finally { if (generation === session.generation()) { busy(false); schedulePoll(); } }
  }
  async function requestQuotaQuery(accountID) {
    if (actionBusy || !session.connected()) return;
    const generation = session.generation();
    let accepted = false;
    pendingQuotaQuery = startTracking(accountID);
    busy(true); renderQuotaQuery();
    try {
      const result = await session.request('/quota-query', 'POST', accountID ? {account_id: accountID} : undefined);
      if (generation !== session.generation()) return;
      mutationEpoch++; accepted = true; acceptedJob(pendingQuotaQuery, result);
      notice(result.status === 'already_queued' ? '已有相同的额度查询任务，正在等待结果。' : accountID ? '已安排此账号的额度查询，不发送模型请求。' : '已安排全部账号的额度查询，不发送模型请求。');
      await readSnapshots();
    } catch (error) {
      if (error.code === 'authentication') { report(error); return; }
      if (generation !== session.generation()) return;
      if (!accepted && error.code !== 'write_unconfirmed') pendingQuotaQuery = null;
      else if (!accepted) pendingQuotaQuery.detail.phase = 'unconfirmed';
      if (!accepted && ['conflict', 'invalid_config', 'http_error'].includes(error.code)) notice('未能安排额度查询。请检查账号和后台状态；已有查询任务时请等待结束后重试。', 'error');
      else if (accepted) reportAcceptedRead(error); else report(error);
    }
    finally { if (generation === session.generation()) { busy(false); renderQuotaQuery(); schedulePoll(); } }
  }
  $('quota-query').addEventListener('click', () => { if (!$('quota-query').disabled) void requestQuotaQuery(); });
  $('quota-connection-check').addEventListener('click', async () => {
    if ($('quota-connection-check').disabled) return;
    const generation = session.generation();
    let accepted = false;
    busy(true);
    try {
      const result = await session.request('/quota-connection-check', 'POST');
      if (generation !== session.generation()) return;
      mutationEpoch++; accepted = true;
      notice(result.error ? quotaFailure({last_error: result.error}) : '宿主查询操作创建与取消检查通过；上游连通性请通过账号额度查询确认。', result.error ? 'error' : undefined);
      await readSnapshots();
    } catch (error) { if (error.code === 'authentication') report(error); else if (generation === session.generation()) { if (accepted) notice('连通性检查已完成；状态暂未读取成功，请更新页面数据核对。', 'warning', 'snapshots'); else report(error); } }
    finally { if (generation === session.generation()) busy(false); }
  });
  $('account-search').addEventListener('input', renderAccounts); $('account-filter').addEventListener('change', renderAccounts);
  for (const id of ['account-sort', 'account-layout']) $(id).addEventListener('change', () => {
    accountPreferences.save($('account-sort').value, $('account-layout').value); renderAccounts(true);
  });
  document.addEventListener('visibilitychange', () => { if (document.hidden && !$('background-refresh').checked) stopAutomaticPoll(); else resumeAutomaticPoll(); });
  root.addEventListener('offline', stopAutomaticPoll);
  root.addEventListener('online', resumeAutomaticPoll);
  root.addEventListener('focus', resumeAutomaticPoll);
  root.addEventListener('pageshow', () => { if (session.connected()) resumeAutomaticPoll(); });
  root.addEventListener('beforeunload', event => { if (editor.dirty()) { event.preventDefault(); event.returnValue = ''; } });
  root.addEventListener('pagehide', () => session.disconnect());
  root.setInterval(() => {
    if (!session.connected() || document.hidden) return;
    const selection = root.getSelection?.();
    for (const item of document.querySelectorAll('[data-reset]')) updateLiveText(item, countdown(item.dataset.reset), selection);
    for (const item of document.querySelectorAll('[data-observed]')) updateLiveText(item, '额度观测：' + observationAge(item.dataset.observed), selection);
    renderFreshness();
  }, 1000);
  (function autoConnect() {
    const key = readPanelKey(root, apiBase(root.location));
    if (key) void connectPanel(key, true);
  })();
})(typeof globalThis !== 'undefined' ? globalThis : this);
