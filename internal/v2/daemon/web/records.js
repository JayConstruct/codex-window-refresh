(function (context) {
  'use strict';

  const reasonNames = {session_affinity: '沿用会话', parent_affinity: '继承父会话', project_preference: '项目首选', quota_load: '额度与负载择优', failover: '故障重选', pinned: '指定账号', host_selected: '宿主选号', scheduler_busy: '绑定账号繁忙', account_unavailable: '账号不可用', available_pool: '可用候选分配', selection_reuse: '复用选号结果', unknown: '原因未提供'};
  const triggerNames = {manual: '手动唤醒', manual_single: '手动单账号', manual_batch: '手动批量', periodic: '定期查询', five_hour_expiry: '五小时到期', weekly_expiry: '周额度到期', initial_sync: '初始核实', wake_preflight: '唤醒前核实', wake_verification: '发送后核实', five_hour_reset: '五小时重置', weekly_reset: '周额度重置', daily: '每日容量准备', prompt_generation: '手动生成', reset_expiry: '临近到期', expiry_reset: '临近到期', manual_generation: '手动生成', shared_query: '合并查询需求'};
  const kindNames = {query: '额度查询', quota_query: '额度查询', wake: '窗口唤醒', generation: '候选生成', prompt_generation: '候选生成', reset: '重置卡使用', reset_card: '重置卡使用', reset_query: '重置卡详情', reset_consume: '重置卡使用'};
  const own = (value, key) => value != null && Object.hasOwn(value, key);
  const count = value => Number.isSafeInteger(value) && value >= 0 ? value : null;
  const number = value => typeof value === 'number' && Number.isFinite(value) && value >= 0 ? value : null;
  const safeText = value => typeof value === 'string' ? value : '';
  const formatNumber = value => number(value) === null ? '—' : new Intl.NumberFormat('zh-CN', {maximumFractionDigits: 2}).format(value);
  const reportedTokenTotal = record => number(record?.input_tokens) === null && number(record?.output_tokens) === null ? null : (number(record?.input_tokens) ?? 0) + (number(record?.output_tokens) ?? 0);
  const duration = value => number(value) === null ? '—' : value < 1000 ? Math.round(value) + ' 毫秒' : (value / 1000).toFixed(value < 10000 ? 1 : 0) + ' 秒';
  const callerLabel = value => safeText(value).startsWith('caller-') ? '调用方 ' + value.slice(7, 15) : safeText(value) || '调用方未知';
  const projectLabel = record => record?.project_name || (safeText(record?.project).startsWith('dir-') ? '项目 ' + record.project.slice(4, 12) : safeText(record?.project)) || '未标记项目';
  const clippedID = value => safeText(value).length > 20 ? value.slice(0, 10) + '…' + value.slice(-5) : safeText(value) || '—';
  const reasonName = value => own(reasonNames, value) ? reasonNames[value] : reasonNames.unknown;
  const triggerName = value => own(triggerNames, value) ? triggerNames[value] : '触发原因未记录';
  const kindName = value => own(kindNames, value) ? kindNames[value] : '上游调用';
  function evidenceValue(name, value) {
    if (typeof value === 'boolean') return value ? '是' : '否';
    const labels = name === 'source' ? {persisted_query_run: '已保留的额度查询记录'}
      : name === 'delivery' ? {unconfirmed: '尚未确认进入上游', normalized_response_observed: '已观测额度响应', response_observed: '已观测上游响应', model_completed: '已确认模型完成', official_outcome_observed: '已观测卡片操作结果'}
      : name === 'execution' ? {pinned_account: '指定账号'} : null;
    return labels && own(labels, value) ? labels[value] : String(value);
  }

  function recordStatus(record) {
    const status = record?.status || record?.phase;
    if (['completed', 'success', 'succeeded', 'returned', 'reset', 'alreadyRedeemed'].includes(status)) return {label: record?.recovered ? '已恢复成功' : '成功', tone: 'success'};
    if (['failed', 'error'].includes(status)) return {label: '失败', tone: 'error'};
    if (['queued', 'pending', 'blocked'].includes(status)) return {label: '等待执行', tone: 'neutral'};
    if (['not_sent', 'not_started', 'rejected', 'not_attempted', 'skipped'].includes(status)) return {label: status === 'skipped' ? '已跳过' : '未发起', tone: 'warning'};
    if (status === 'cancelled' || status === 'canceled') return {label: '已取消', tone: 'neutral'};
    if (['verifying', 'checking'].includes(status)) return {label: '核实中', tone: 'warning'};
    if (['running', 'sending', 'active', 'prepared', 'started'].includes(status)) return {label: '执行中', tone: 'neutral'};
    return {label: '结果未知', tone: 'warning'};
  }

  function pluginStatus(record) {
    if (!record?.kind) return recordStatus(record);
    if (record.status === 'unknown') return {label: '结果未知', tone: 'warning'};
    if (record.response?.completed === true || record.evidence?.delivery === 'model_completed') return {label: '模型完成', tone: 'success'};
    if (['reset', 'alreadyRedeemed'].includes(record.evidence?.outcome)) return {label: '已兑换', tone: 'success'};
    if (record.kind === 'query' && (record.http_status || record.evidence?.delivery === 'normalized_response_observed') && ['completed', 'success'].includes(record.status)) return {label: '已返回', tone: 'success'};
    if (record.attempted === false && ['completed', 'success'].includes(record.status)) return {label: '任务完成', tone: 'neutral'};
    return recordStatus(record);
  }

  function localRange(range, now = new Date()) {
    if (range === 'all') return {};
    const start = new Date(now), end = new Date(now);
    start.setHours(0, 0, 0, 0); end.setHours(24, 0, 0, 0);
    if (range === '7d') start.setDate(start.getDate() - 6);
    else if (range === '30d') start.setDate(start.getDate() - 29);
    return {from: start.toISOString(), to: end.toISOString()};
  }

  function trafficQuery(kind, filters = {}, id = '', now = new Date()) {
    const query = new URLSearchParams({kind});
    if (id) query.set('id', id);
    else {
      for (const key of ['search', 'project', 'caller', 'account', 'model', 'status', 'session']) if (safeText(filters[key]).trim()) query.set(key, filters[key].trim());
      const range = localRange(filters.range || 'today', now);
      for (const key of ['from', 'to']) if (range[key]) query.set(key, range[key]);
      query.set('page', String(Math.max(1, count(filters.page) || 1)));
      query.set('limit', String(Math.min(100, Math.max(1, count(filters.limit) || 7))));
    }
    return '/traffic?' + query.toString();
  }

  function validateEnvelope(data, kind, detail = false) {
    if (!data || typeof data !== 'object' || Array.isArray(data) || typeof data.instance_id !== 'string' || !data.instance_id || count(data.view_revision) === null) throw new Error('invalid_traffic_response');
    if (detail) {
      const record = data.record || data.records?.[0] || (typeof data.id === 'string' ? data : null);
      if (!record || typeof record.id !== 'string') throw new Error('invalid_traffic_response');
      if (record.attempts !== undefined && !Array.isArray(record.attempts)) throw new Error('invalid_traffic_response');
      return record;
    }
    if (kind === 'analysis') {
      if (!['accounts', 'reasons', 'projects'].every(key => Array.isArray(data[key]))) throw new Error('invalid_traffic_response');
    } else if (!Array.isArray(data.records) || data.records.some(record => !record || typeof record.id !== 'string') || count(data.total) === null || count(data.page) === null || data.page < 1 || count(data.limit) === null || data.limit < 1) throw new Error('invalid_traffic_response');
    return data;
  }

  class VersionGate {
    constructor(instance = '') { this.instance = instance; this.epoch = 0; this.sequence = new Map(); this.revisions = new Map(); this.retired = new Set(); }
    reset(instance = '') { if (this.instance && this.instance !== instance) this.retired.add(this.instance); this.instance = instance; this.epoch++; this.revisions.clear(); this.sequence.clear(); }
    begin(resource) { const serial = (this.sequence.get(resource) || 0) + 1; this.sequence.set(resource, serial); return {resource, serial, epoch: this.epoch}; }
    accept(ticket, data) {
      if (ticket.epoch !== this.epoch || this.sequence.get(ticket.resource) !== ticket.serial || !data || count(data.view_revision) === null || typeof data.instance_id !== 'string' || !data.instance_id || this.retired.has(data.instance_id)) return false;
      if (this.instance && this.instance !== data.instance_id) return false;
      if (!this.instance) this.instance = data.instance_id;
      const previous = this.revisions.get(ticket.resource);
      if (previous !== undefined && data.view_revision < previous) return false;
      this.revisions.set(ticket.resource, data.view_revision); return true;
    }
  }

  function mergeRecord(record, recordRevision, detail, detailRevision) {
    if (!detail || detail.id !== record?.id) return record;
    return detailRevision >= recordRevision ? {...record, ...detail} : {...detail, ...record};
  }

  function reasonExplanation(record, attempt = null) {
    const source = attempt || record || {}, evidence = source.evidence || record?.evidence || {};
    if (typeof evidence.explanation === 'string' && evidence.explanation.trim()) return evidence.explanation;
    switch (source.reason || record?.reason) {
      case 'session_affinity': return '沿用执行当时已建立的会话账号绑定。';
      case 'parent_affinity': return '依据执行当时记录的父会话绑定继承账号。';
      case 'project_preference': return '在宿主可用候选中选择了项目首选账号。';
      case 'available_pool': return '从宿主提供的可用账号候选中分配；没有记录其他优先因素。';
      case 'selection_reuse': return '复用了已建立的选号结果，没有创建新的上游尝试。';
      case 'quota_load': return '依据当时可用的额度、重置时间和占用信息择优分配。';
      case 'failover': {
        const attempts = Array.isArray(record?.attempts) ? record.attempts : [];
        const previous = attempts.find(item => item.status === 'failed' && item.account_id !== record.account_id);
        const latest = attempts.at(-1), code = count(previous?.http_status);
        if (previous && latest) return '此前账号的尝试' + (code ? '返回 HTTP ' + code : '未成功') + '，随后按当时调度规则重新选择' + (latest.evidence?.project_preferred === true ? '项目首选组中的可用账号' : '可用账号') + '；' + (record.status === 'completed' ? '后续尝试已完成。' : '后续结果见逐次尝试。');
        return '此前尝试未成功，调度器按当时规则重新选择账号；具体依据见逐次尝试。';
      }
      case 'pinned': return '请求或后台计划指定该账号，按指定身份执行。';
      case 'scheduler_busy': return '绑定账号当时繁忙，未建立新的上游调用尝试。';
      case 'account_unavailable': return '执行当时账号不可用，未建立新的上游调用尝试。';
      case 'host_selected': return '账号由 CPA 宿主选择，未提供更详细的选号原因。';
      default: return '没有记录到可核实的分流原因，不依据当前账号状态推断历史原因。';
    }
  }

  function matrixView(data, group = 'project') {
    const rows = group === 'caller' ? data?.callers : group === 'session' ? data?.sessions : data?.projects;
    if (!Array.isArray(rows)) return {rows: [], accounts: [], max: 0, available: false};
    const ids = new Set((data.accounts || []).map(item => item.account_id).filter(Boolean));
    for (const row of rows) for (const id of Object.keys(row.accounts || {})) ids.add(id);
    const accounts = [...ids].slice(0, 32); let max = 0;
    for (const row of rows) for (const id of accounts) if (count(row.accounts?.[id]) !== null) max = Math.max(max, row.accounts[id]);
    return {rows: rows.slice(0, 32), accounts, max, available: true, limited: ids.size > 32 || rows.length > 32};
  }

  function sameSessionFilter(record, filters = {}) {
    if (!record?.session || !record?.caller) return null;
    return {...filters, search: '', project: '', account: '', model: '', status: '', session: record.session, caller: record.caller, page: 1};
  }

  function matrixDrillFilter(group, row, accountID) {
    const dimension = ['project', 'caller', 'session'].includes(group) ? group : 'project';
    // Empty API filters mean all values, so an unmarked row cannot be drilled down exactly.
    if (!safeText(row?.[dimension]).trim() || !safeText(accountID).trim()) return null;
    return {[dimension]: row[dimension], account: accountID};
  }

  function mount(options) {
    if (!options?.root || typeof options.request !== 'function') throw new Error('invalid_records_options');
    const root = options.root, document = root.ownerDocument, window = document.defaultView || context;
    root.classList.add('runtime-records');
    let listController = null, detailController = null;
    let connected = options.connected === true, page = 'users', disposed = false, timer = null, listPending = null, detailPending = null;
    let selectedOpener = null, copyFallbackValue = '';
    let data = null, selected = null, selectedRevision = -1, detail = null, detailRevision = -1, detailError = '', error = '', loading = false;
    let accountDisplay = options.displayAccount || (value => value || '—'), analysisMetric = 'attempts', matrixGroup = 'project';
    let knownFacets = {projects: new Map(), callers: new Map(), accounts: new Map(), models: new Map()};
    const filtersByPage = Object.fromEntries(['users', 'plugin', 'analysis'].map(key => [key, {search: '', project: '', caller: '', account: '', model: '', status: '', session: '', range: 'today', page: 1, limit: key==='plugin'?6:7}]));
    const gate = new VersionGate(options.instanceId || '');
    const listeners = [];
    function el(tag, className = '', text = '') { const node = document.createElement(tag); if (className) node.className = className; if (text !== '') node.textContent = String(text); return node; }
    function key(node, value) { node.dataset.rrKey = value; return node; }
    function icon(name) { const paths = {search: 'M20 20l-5-5M17 10a7 7 0 1 1-14 0 7 7 0 0 1 14 0', refresh: 'M20 7v5h-5M4 17v-5h5M5 8a8 8 0 0 1 13-3l2 2M19 16a8 8 0 0 1-13 3l-2-2', copy: 'M8 8h12v13H8zM16 8V3H3v13h5', clock: 'M12 7v5l4 2M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0', request: 'M6 3h9l4 4v14H6zM14 3v5h5M9 12h7M9 16h5', arrow: 'M4 12h16m-6-6 6 6-6 6', close: 'M6 6l12 12M18 6 6 18', info: 'M12 11v6m0-10v1M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0'};
      const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg'); svg.setAttribute('viewBox', '0 0 24 24'); svg.setAttribute('class', 'rr-icon'); svg.setAttribute('aria-hidden', 'true'); const path = document.createElementNS('http://www.w3.org/2000/svg', 'path'); path.setAttribute('d', paths[name] || paths.request); svg.append(path); return svg; }
    function button(text, handler, className = 'rr-button') { const node = el('button', className, text); node.type = 'button'; node.onclick = handler; return node; }
    function empty(title, description = '') { const node = el('div', 'rr-empty'); node.append(icon('request'), el('strong', '', title)); if (description) node.append(el('p', '', description)); return node; }
    function badge(record, label) { const view = record?.kind ? pluginStatus(record) : recordStatus(record); return el('span', 'rr-badge rr-' + view.tone, label || view.label); }
    function account(id) { return id ? String(accountDisplay(id) || id) : '—'; }
    function date(value, full = false) { const at = typeof value === 'string' ? Date.parse(value) : NaN; return Number.isFinite(at) ? new Intl.DateTimeFormat('zh-CN', full ? {month:'2-digit', day:'2-digit', hour:'2-digit', minute:'2-digit', second:'2-digit', hour12:false} : {hour:'2-digit', minute:'2-digit', second:'2-digit', hour12:false}).format(at) : '—'; }
    function pair(label, value) { const row = el('div', 'rr-pair'); row.append(el('dt', '', label), el('dd', '', value)); return row; }
    function section(title, content, opened = true) { const block = el('details', 'rr-detail-section'); block.open = opened; block.append(el('summary', '', title)); const body = el('div', 'rr-section-body'); body.append(...content); block.append(body); return block; }
    function listen(node, name, callback) { node.addEventListener(name, callback); listeners.push(() => node.removeEventListener(name, callback)); }
    function morph(parent, desired) {
      const previousChildren = [...parent.childNodes]; let desiredIndex = 0;
      const keyed = new Map([...parent.childNodes].filter(node => node.nodeType === 1 && node.dataset.rrKey).map(node => [node.dataset.rrKey, node]));
      const nodes = [];
      for (const wanted of desired) {
        const previousChild = previousChildren[desiredIndex++];
        if (wanted.nodeType === 3 && previousChild?.nodeType === 3) { if (previousChild.nodeValue !== wanted.nodeValue) previousChild.nodeValue = wanted.nodeValue; nodes.push(previousChild); continue; }
        let existing = wanted.nodeType === 1 && wanted.dataset.rrKey ? keyed.get(wanted.dataset.rrKey) : null;
        if (existing && existing.tagName === wanted.tagName) {
          const open = existing.tagName === 'DETAILS' ? existing.open : null;
          for (const attr of [...existing.attributes]) if (!wanted.hasAttribute(attr.name) && attr.name !== 'open') existing.removeAttribute(attr.name);
          for (const attr of [...wanted.attributes]) if (existing.getAttribute(attr.name) !== attr.value && attr.name !== 'open') existing.setAttribute(attr.name, attr.value);
          for (const event of ['onclick', 'onchange', 'oninput', 'onkeydown']) existing[event] = wanted[event];
          if (wanted.tagName === 'BUTTON') existing.disabled = wanted.disabled;
          if (wanted.tagName === 'INPUT' && existing !== document.activeElement) existing.value = wanted.value;
          morph(existing, [...wanted.childNodes]); if (open !== null) existing.open = open;
        } else existing = wanted;
        nodes.push(existing);
      }
      let anchor = parent.firstChild;
      for (const node of nodes) { if (node === anchor) anchor = anchor.nextSibling; else parent.insertBefore(node, anchor); }
      const keep = new Set(nodes); for (const node of [...parent.childNodes]) if (!keep.has(node)) node.remove();
    }
    function keyedTree(node, prefix) { if (node.nodeType !== 1) return node; if (!node.dataset.rrKey) node.dataset.rrKey = prefix; [...node.children].forEach((child, index) => keyedTree(child, prefix + ':' + index)); return node; }
    function replace(node, children) { morph(node, children.map((child, index) => keyedTree(child, child.dataset?.rrKey || 'node:' + index))); }
    function notify() { options.onChange?.({page, loading, total: data?.total ?? null, connected, selected: selected?.id || ''}); }

    const filters = el('div', 'rr-filters');
    const searchBox = el('div', 'rr-search'); searchBox.append(icon('search')); const search = el('input'); search.type = 'search'; search.placeholder = '搜索请求、会话或模型'; search.setAttribute('aria-label', '搜索运行记录'); searchBox.append(search); filters.append(searchBox);
    const fields = {};
    for (const [name, placeholder, label] of [['project','全部项目','项目'],['caller','全部调用方','调用方'],['account','全部账号','账号']]) {
      const wrap = el('div', 'rr-filter-input'), input = el('input'); input.type = 'text'; input.placeholder = placeholder; input.setAttribute('aria-label', label + '筛选'); input.autocomplete = 'off'; const list = el('datalist'); list.id = 'rr-facet-' + name; input.setAttribute('list', list.id); wrap.append(input, el('span', 'rr-select-chevron', '⌄'), list); filters.append(wrap); fields[name] = {input, list};
    }
    const range = el('select'); range.setAttribute('aria-label', '记录时间范围'); for (const [value,label] of [['today','今天'],['7d','最近 7 天'],['30d','最近 30 天'],['all','全部时间']]) { const option = el('option', '', label); option.value = value; range.append(option); } filters.append(range);
    const more = el('details', 'rr-more'); more.append(el('summary', '', '更多筛选')); const moreBody = el('div', 'rr-more-body'); const model = el('input'); model.placeholder = '全部模型'; model.className='rr-model-filter'; model.setAttribute('aria-label', '模型筛选'); const status = el('select'); status.setAttribute('aria-label', '结果筛选'); for (const [value,label] of [['','全部结果'],['completed','成功'],['failed','失败'],['unknown','结果未知'],['running','执行中'],['not_sent','未发起']]) { const option = el('option','',label); option.value=value; status.append(option); } const clear = button('清除筛选', () => resetFilters()); moreBody.append(model, status, clear); more.append(moreBody); filters.append(more);
    const refreshButton = button('刷新记录', () => refresh(true), 'rr-button rr-refresh'); refreshButton.prepend(icon('refresh')); if (!options.hideRefresh) filters.append(refreshButton);
    const summary = el('div', 'rr-summary'), feedback = el('div', 'rr-feedback'); feedback.setAttribute('role', 'status'); feedback.hidden = true;
    const workspace = el('div', 'rr-workspace'), listPanel = el('section', 'rr-list-panel'), detailPanel = el('aside', 'rr-detail'); detailPanel.setAttribute('aria-label', '请求详情');
    const tableScroll = el('div', 'rr-table-scroll'), table = el('table', 'rr-table'), head = el('thead'), body = el('tbody'); table.append(head, body); tableScroll.append(table); const pagination = el('footer', 'rr-pagination'); const listHelp = el('p', 'rr-list-help'); listPanel.append(tableScroll, pagination, listHelp); workspace.append(listPanel, detailPanel);
    const analysis = el('div', 'rr-analysis'); analysis.hidden = true;
    root.append(filters, summary, feedback, workspace, analysis);
    listen(search, 'input', () => changeFilter('search', search.value, true));
    for (const [name, field] of Object.entries(fields)) listen(field.input, 'input', () => changeFilter(name, field.input.value, true));
    listen(range, 'change', () => changeFilter('range', range.value)); listen(model, 'input', () => changeFilter('model', model.value, true)); listen(status, 'change', () => changeFilter('status', status.value));

    function readFilters() { return filtersByPage[page] || filtersByPage.users; }
    function updateFilterControls() { const value = readFilters(); search.value = value.search; for (const [name,field] of Object.entries(fields)) field.input.value = value[name]; model.value=value.model; status.value=value.status; range.value=value.range; search.placeholder = page === 'plugin' ? '搜索账号、模型或请求 ID' : '搜索请求或会话'; fields.caller.input.parentNode.hidden = page === 'plugin'; fields.project.input.parentNode.hidden = page === 'plugin'; fields.account.input.parentNode.hidden=page==='analysis'; searchBox.hidden=page==='analysis'; more.hidden=page==='analysis'; if(page==='analysis')filters.insertBefore(model,range);else moreBody.insertBefore(model,status); }
    function changeFilter(name, value, delayed = false) { if (!filtersByPage[page]) return; clearTimeout(timer); listController?.abort(); detailController?.abort(); gate.begin('detail'); detailPending=null; Object.assign(readFilters(), {[name]:value, ...name==='search'?{session:''}:{}, page:1}); gate.begin('list'); listPending = null; selected=null; detail=null; detailError=''; if (delayed) timer=setTimeout(() => refresh(true),250); else void refresh(true); }
    function resetFilters() { Object.assign(readFilters(), {search:'',project:'',caller:'',account:'',model:'',status:'',session:'',range:'today',page:1}); updateFilterControls(); selected=null; detail=null; void refresh(true); }
    function rememberFacets(payload) {
      for (const item of payload.facets?.projects || []) if (item?.id) knownFacets.projects.set(item.id, item.name || item.id);
      for (const value of payload.facets?.callers || []) if (typeof value === 'string') knownFacets.callers.set(value, callerLabel(value));
      for (const value of payload.facets?.accounts || []) if (typeof value === 'string') knownFacets.accounts.set(value, account(value));
      for (const value of payload.facets?.models || []) if (typeof value === 'string') knownFacets.models.set(value, value);
      for (const record of payload.records || []) { if (record.project) knownFacets.projects.set(record.project, record.project_name || record.project); if (record.caller) knownFacets.callers.set(record.caller, callerLabel(record.caller)); if (record.account_id) knownFacets.accounts.set(record.account_id, account(record.account_id)); if (record.model) knownFacets.models.set(record.model, record.model); }
      for (const [name,field] of Object.entries(fields)) { const items=[...knownFacets[name === 'project' ? 'projects' : name === 'caller' ? 'callers' : 'accounts']].slice(0,512); replace(field.list,items.map(([id,label])=>{const option=el('option','',label);option.value=id;return key(option,id)})); }
    }
    function renderSummary() {
      const stats = data?.summary, labels = page === 'plugin' ? [['requests','已观测调用'],['attempts','调用尝试'],['unknown','未确认结果']] : [['requests','用户请求'],['attempts','上游尝试'],['recovered','换号恢复'],['unknown','结果未知']];
      const nodes=labels.map(([name,label])=>{const item=el('div','rr-stat'+(name==='unknown'&&stats?.[name]>0?' rr-warning':''));item.append(el('span','',label),el('strong','',formatNumber(stats?.[name])));return key(item,name)});
      const caption=el('span','rr-summary-caption',page === 'plugin' ? '按持久操作汇总，不代表完整网络日志' : readFilters().account?'筛选含该账号执行的请求，保留其它尝试':'仅统计已观测用户请求，不含插件调用'); nodes.push(key(caption,'caption')); replace(summary,nodes);
    }
    function renderFeedback() {
      const coverage=data?.coverage; let text=''; let tone='';
      if (!connected) text='连接 CPA 后读取真实运行记录。';
      else if (error) { text=error; tone='error'; }
      else if (loading&&!data) text='正在读取运行记录…';
      else if (data?.aggregation_limited) { text='仅展示调用最多的部分维度，请缩小筛选范围查看完整分配。'; tone='warning'; }
      else if (coverage?.unlinked_attempts>0) { text='存在 '+formatNumber(coverage.unlinked_attempts)+' 次尚未关联的上游用量，未纳入用户请求分流统计。'; tone='warning'; }
      else if (coverage&&['gap','disconnected','unavailable','partial'].includes(coverage.status)) { text='当前观测存在缺口，记录和统计仅代表已采集范围。'; tone='warning'; }
      feedback.hidden=!text; feedback.className='rr-feedback'+(tone?' rr-'+tone:''); feedback.textContent=text;
      refreshButton.disabled=!connected; refreshButton.setAttribute('aria-busy',String(loading));
    }
    function textCell(primary, secondary='', className='') { const cell=el('td',className);cell.append(el('strong','rr-primary',primary));if(secondary)cell.append(el('small','rr-secondary',secondary));return cell; }
    function rowFor(record) {
      const row=key(el('tr',selected?.id===record.id?'rr-selected':''),record.id); row.tabIndex=0; row.setAttribute('aria-selected',String(selected?.id===record.id));row.setAttribute('aria-label',(page==='plugin'?kindName(record.kind):projectLabel(record))+'，'+recordStatus(record).label+'，查看详情');
      row.onclick=()=>select(record,row);row.onkeydown=event=>{if(event.key==='Enter'||event.key===' '){event.preventDefault();select(record,row)}};
      row.append(textCell(date(record.created_at)));
      if(page==='plugin') {
        row.append(textCell(kindName(record.kind), ['query','quota_query','reset_query'].includes(record.kind)?'不调用模型': ['wake','generation','prompt_generation'].includes(record.kind)?'模型调用':'卡片操作'));
        row.append(textCell(account(record.account_id),record.model||'')); row.append(textCell(triggerName(record.trigger)));
      } else {
        row.append(textCell(projectLabel(record),record.session?(record.parent_session?'子会话 ':'会话 ')+clippedID(record.session):'会话未知'));
        row.append(textCell(callerLabel(record.caller),record.model||'模型未记录'));
        row.append(textCell(account(record.account_id)));
        row.append(textCell(reasonName(record.reason),record.reason==='host_selected'?'原因未提供':'',record.reason==='failover'?'rr-warning':''));
      }
      const result=el('td');result.append(badge(record));const extra=page==='plugin'?(record.http_status?'HTTP '+record.http_status:record.attempted===false?'未确认进入上游调用':''):count(record.attempt_count)!==null?record.attempt_count+' 次尝试':'';if(extra)result.append(el('small','rr-secondary',extra));row.append(result);return row;
    }
    function renderList() {
      const plugin=page==='plugin', titles=plugin?['时间','调用','账号 / 模型','触发原因','结果']:['时间','项目 / 会话','调用方 / 模型','执行账号','分流原因','结果']; const header=el('tr');for(const title of titles)header.append(el('th','',title)); replace(head,[header]);
      const records=data?.records||[]; if(records.length)replace(body,records.map(rowFor));else {const row=el('tr'),cell=el('td');cell.colSpan=titles.length;cell.append(empty(!connected?'等待连接 CPA':loading?'正在读取记录':'暂无运行记录',!connected?'连接后显示后台已采集的真实请求。':error?'读取恢复后可再次查看。':'当前筛选范围内没有已保留的记录。'));row.append(cell);replace(body,[row]);}
      const total=count(data?.total),current=count(data?.page)||readFilters().page,limit=count(data?.limit)||7,totalPages=total===null?1:Math.max(1,Math.ceil(total/limit)); const caption=el('span','',total===null?'尚未读取记录':total?'显示 '+((current-1)*limit+1)+'–'+Math.min(current*limit,total)+'，共 '+total+(plugin?' 次调用':' 个用户请求'):'共 0 条记录');
      const pages=el('div','rr-pages');for(const [label,target] of [['‹',current-1],...([...new Set([1,current-1,current,current+1,totalPages])].filter(n=>n>0&&n<=totalPages).sort((a,b)=>a-b).map(n=>[String(n),n])),['›',current+1]]) {const b=button(label,()=>{readFilters().page=target;selected=null;detail=null;void refresh(true)},'rr-page');b.disabled=!connected||target<1||target>totalPages;b.setAttribute('aria-label',label==='‹'?'上一页':label==='›'?'下一页':'第 '+label+' 页');if(target===current&&/^\d/.test(label))b.setAttribute('aria-current','page');pages.append(key(b,label+':'+target));} replace(pagination,[caption,pages]);
      listHelp.textContent=plugin?'展示已保留的插件操作汇总；同一操作可能包含多次调用，不代表完整网络日志。':'按请求分组，展开详情查看每次上游尝试；缺失原因不作推测。';
    }
    function addMetadata(record, plugin) {
      const metadata=el('dl','rr-metadata'); const entries=plugin?[['账号',account(record.account_id)],['模型',record.model||'—'],['触发原因',triggerName(record.trigger)],['创建时间',date(record.created_at,true)],['当前阶段',record.stage||'未记录'],['任务 ID',clippedID(record.task_id)]]:[['项目',projectLabel(record)],['模型',record.model||'未记录'],['调用方',record.caller?callerLabel(record.caller):'未知'],['最终账号',account(record.account_id)],['会话',clippedID(record.session)],['总耗时',duration(record.latency_ms)]];for(const [label,value]of entries)metadata.append(pair(label,value));return metadata;
    }
    function renderAttempts(record) {
      if(!Array.isArray(record.attempts))return [el('p','rr-note','尚未记录到逐次上游尝试。')];
      if(!record.attempts.length)return [el('p','rr-note',recordStatus(record).label==='未发起'?'此请求未建立上游调用尝试。':'尚未收到可关联的上游尝试事件。')];
      return record.attempts.map((attempt,index)=>{const card=key(el('div','rr-attempt'),attempt.id||attempt.selection_id||'attempt:'+index),line=el('div','rr-attempt-heading');line.append(el('strong','',(attempt.executed===false?'选号记录':'尝试 '+(index+1))+' · '+account(attempt.account_id)),el('span','rr-chip',reasonName(attempt.reason)),attempt.executed===false?el('span','rr-badge','尚未确认执行'):badge(attempt));if(attempt.http_status)line.append(el('span','rr-http','HTTP '+attempt.http_status));line.append(el('span','rr-attempt-duration',duration(attempt.latency_ms)));card.append(line);if(attempt.evidence?.explanation)card.append(el('p','rr-note',attempt.evidence.explanation));return card;});
    }
    function renderEvidence(record) {
      const attempts=record.attempts||[],attempt=attempts[attempts.length-1],evidence=attempt?.evidence||record.evidence||{},nodes=[];
      const candidates=Array.isArray(evidence.candidates)?evidence.candidates:[];
      for(const candidate of candidates.slice(0,32)){const line=el('div','rr-evidence-row');line.append(el('strong','',account(candidate.account_id||candidate.id)),el('span','',safeText(candidate.reason_text)||reasonName(candidate.reason)));nodes.push(line);}
      const labels={source:'记录来源',delivery:'调用证据',account_bound:'账号绑定一致',quota_checked:'额度已核实',activity_check_passed:'活动检查通过',capacity_missing:'当时容量缺口',recorded_at:'授权记录时间',execution:'执行方式',candidate_count:'候选账号数',snapshot_fresh:'调度快照新鲜',priority:'宿主优先级',project_preferred:'项目首选',inflight:'当时占用',max_inflight:'并发上限',quota_fresh:'额度证据新鲜',binding_match:'账号绑定一致',policy_revision:'规则版本',model_revision:'模型配置版本',capacity:'当时可用窗口',target:'目标窗口'};
      const values=el('dl','rr-evidence-fields');for(const [name,label]of Object.entries(labels))if(own(evidence,name)&&['string','number','boolean'].includes(typeof evidence[name])) values.append(pair(label,evidenceValue(name,evidence[name])));if(values.children.length)nodes.push(values);
      if(!nodes.length)nodes.push(el('p','rr-note','未记录更详细的候选依据；不使用当前额度重建历史决策。'));return nodes;
    }
    function usage(record) { const nodes=[];for(const [name,label]of [['input_tokens','输入'],['output_tokens','输出'],['cached_tokens','缓存读取']]){const item=el('span','rr-usage-item');item.append(el('span','',label),el('strong','',formatNumber(record[name])));nodes.push(item);}return nodes; }
    function renderPluginDetail(record) {
      const nodes=[],snapshot=record.evidence||{},facts=[];
      if(snapshot.account_bound===true)facts.push('账号绑定一致');if(snapshot.quota_checked===true)facts.push('额度已核实');if(snapshot.activity_check_passed===true)facts.push('活动检查通过');
      const reason=el('div','rr-reason-copy',triggerName(record.trigger)+(facts.length?'规则触发；'+facts.join('，')+'。':''));
      if(record.attempted===false)nodes.push(el('p','rr-note','当前记录无法确认已进入上游调用，不作为成功送达证据。'));
nodes.push(section('为什么发送',[reason,el('p','rr-note',snapshot.recorded_at?'依据为发送授权当时的记录。':'展示已记录的触发原因，完整授权快照未记录。')]));
      nodes.push(section('为什么使用这个账号',[el('p','rr-reason-copy',record.reason?reasonExplanation(record):'后台任务按已保存的目标账号执行；未记录额外选号依据。')]));
      nodes.push(section('发送时记录的依据',renderEvidence(record),false));
      const response=record.response||{},fields=el('dl','rr-response-fields');for(const [label,value]of [['执行阶段',record.stage||response.stage||'未记录'],['HTTP 状态',number(record.http_status||response.http_status)===null?'—':String(record.http_status||response.http_status)],['最后事件',response.event||'未记录'],['模型完成',response.completed===true?'已确认':response.completed===false?'未确认':'未记录']])fields.append(pair(label,value));const content=[fields];if(record.kind==='wake'){fields.append(pair('窗口状态',record.status==='verifying'?'仍在核实':record.status==='unknown'?'待核实':'请查看关联任务'));content.push(el('p','rr-note','模型完成与窗口启动分别确认。'));}if(response.error_message)content.push(el('p','rr-response-text',response.error_message));if(response.response_text)content.push(el('pre','rr-response-text',response.response_text));if(response.truncated)content.push(el('p','rr-note','响应摘要已截断'));nodes.push(section('响应与后续',content));if(record.attempts?.length)nodes.push(section('关联调用',renderAttempts(record),false));return nodes;
    }
    function renderDetail() {
      if(!selected){replace(detailPanel,[empty('选择一条记录','查看执行账号、分流原因与逐次上游尝试。')]);detailPanel.classList.add('rr-detail-empty');return;}
      detailPanel.classList.remove('rr-detail-empty');const record=mergeRecord(selected,selectedRevision,detail,detailRevision),plugin=page==='plugin';const content=key(el('div','rr-detail-content'),record.id),heading=el('div','rr-detail-heading');const title=el('h2','',plugin?kindName(record.kind):'请求详情');title.tabIndex=-1;heading.append(title,badge(record));const requestID=el('span','rr-request-id','请求 ID '+clippedID(record.request_id||record.id));requestID.title=record.request_id||record.id;heading.append(requestID);const close=button('×',()=>{selected=null;detail=null;copyFallbackValue='';gate.begin('detail');render();if(selectedOpener?.isConnected)selectedOpener.focus({preventScroll:true})},'rr-close');close.setAttribute('aria-label','关闭请求详情');heading.append(close);content.append(heading);
      if(recordStatus(record).label==='结果未知'){const warning=el('div','rr-safety');warning.append(icon('info'),el('div','','执行结果未知；只读核实原记录，请勿据此重复发送。'));content.append(warning);}
      content.append(addMetadata(record,plugin));
      if(detailError)content.append(el('p','rr-inline-error',detailError));
      if(plugin)content.append(...renderPluginDetail(record));else {content.append(section('为什么分到这个账号',[el('p','rr-reason-copy',reasonExplanation(record)),el('p','rr-note','依据为执行当时记录；HTTP 429 不直接判定为额度耗尽。')]));content.append(section('上游尝试'+(count(record.attempt_count)!==null?' '+record.attempt_count:''),renderAttempts(record)));content.append(section('查看候选账号依据',renderEvidence(record),true));const usageRow=el('div','rr-usage');usageRow.append(...usage(record));const usageSection=section('已报告用量',[usageRow,el('p','rr-note','未报告的用量显示“—”，不推算费用。')],true);usageSection.classList.add('rr-usage-section');content.append(usageSection);}
      const actions=el('div','rr-detail-actions'),copy=button('复制请求 ID',()=>copyID(record.request_id||record.id),'rr-button');copy.prepend(icon('copy'));copy.disabled=!record.id;actions.append(copy);
      if(plugin&&record.task_id){const task=button('查看关联任务',()=>options.onTask?.(record.task_id,record.kind),'rr-button rr-primary-button');task.disabled=typeof options.onTask!=='function';actions.append(task);}else{const same=button('查看同会话请求',()=>{const filter=sameSessionFilter(record,filtersByPage.users);if(filter){Object.assign(filtersByPage.users,filter);options.onNavigate?.('users');show('users');updateFilterControls();void refresh(true)}},'rr-button rr-primary-button');same.disabled=!sameSessionFilter(record);if(same.disabled)same.title='缺少可核实的会话或调用方标识';actions.append(same);}
      const fallback=el('div','rr-copy-fallback');fallback.hidden=!copyFallbackValue;const input=el('input');input.value=copyFallbackValue;input.readOnly=true;input.setAttribute('aria-label','手动复制请求 ID');fallback.append(el('label','','复制请求 ID'),input);content.append(actions,fallback);replace(detailPanel,[content]);
    }
    async function copyID(id) { if(!id||!selected)return;const selectedID=selected.id,epoch=gate.epoch;try { if(window.isSecureContext&&window.navigator?.clipboard?.writeText){await window.navigator.clipboard.writeText(id);if(epoch===gate.epoch&&selected?.id===selectedID)options.onNotice?.('已复制请求 ID');return;} }catch{}if(epoch!==gate.epoch||selected?.id!==selectedID)return;const fallback=detailPanel.querySelector('.rr-copy-fallback'),input=fallback?.querySelector('input');if(fallback&&input){copyFallbackValue=id;fallback.hidden=false;input.value=id;input.focus();input.select();} }
    function barRow(label,value,max,handler,tone='',secondary='',unit=' 次') {const row=el('button','rr-bar-row'+(tone?' rr-'+tone:''));row.type='button';row.onclick=handler;row.disabled=!handler;const track=el('span','rr-bar-track'),fill=el('span','rr-bar-fill');fill.style.width=number(value)!==null&&max>0?Math.min(100,value/max*100)+'%':'0';track.append(fill);row.append(el('strong','rr-bar-label',label),track,el('span','rr-bar-count',formatNumber(value)+(value===null?'':unit)));if(secondary)row.append(el('span','rr-bar-share',secondary));return row; }
    function drill(filter) {Object.assign(filtersByPage.users,{...filtersByPage.analysis,search:'',session:'',...filter,page:1});options.onNavigate?.('users');show('users');}
    function renderAnalysis() {
      const content=[],grid=el('div','rr-analysis-grid'),accountCard=el('section','rr-analysis-card'),accountHeading=el('div','rr-card-heading');accountHeading.append(el('h2','','账号分配'),el('span','rr-note','按实际执行账号统计'));const switcher=el('div','rr-segment');for(const [value,label]of [['attempts','上游尝试'],['reported_tokens','已报告 Token']]){const b=button(label,()=>{analysisMetric=value;renderAnalysis()},'rr-segment-button');b.setAttribute('aria-pressed',String(analysisMetric===value));switcher.append(b);}accountHeading.append(switcher);accountCard.append(accountHeading);const accountColumns=el('div','rr-chart-columns');accountColumns.append(el('span','','账号'),el('span','',''),el('span','','次数'),el('span','','占已观测尝试'));accountCard.append(accountColumns);const items=data?.accounts||[],maximum=Math.max(0,...items.map(item=>number(analysisMetric==='attempts'?item.attempts:reportedTokenTotal(item))??0)),total=items.reduce((sum,item)=>sum+(number(analysisMetric==='attempts'?item.attempts:reportedTokenTotal(item))??0),0);
      if(items.length)for(const item of items){const value=number(analysisMetric==='attempts'?item.attempts:reportedTokenTotal(item)),denominator=analysisMetric==='attempts'?(number(data?.summary?.attempts)??total):data?.aggregation_limited?0:total,share=value!==null&&denominator>0?Math.round(value/denominator*100)+'%':'';accountCard.append(barRow(account(item.account_id),value,analysisMetric==='attempts'?(number(data?.summary?.attempts)??maximum):maximum,()=>drill({account:item.account_id}),'',share,analysisMetric==='attempts'?' 次':''));}else accountCard.append(empty('暂无账号分配记录','当前筛选范围内没有已观测的上游尝试。'));
      accountCard.append(el('p','rr-note',analysisMetric==='attempts'?'当前筛选范围内，'+items.length+' 个账号有执行记录。':'仅累计已报告的输入与输出 Token；缓存读取不重复相加。已报告用量 '+items.reduce((sum,item)=>sum+(count(item.reported_usage_attempts)||0),0)+' / '+formatNumber(data?.summary?.attempts)+' 次尝试。'),el('p','rr-explanation','会话粘性、项目偏好和可用账号会影响分配占比，占比高不一定是异常。'));grid.append(accountCard);
      const reasonCard=el('section','rr-analysis-card'),reasonHeading=el('div','rr-card-heading');reasonHeading.append(el('h2','','分流原因'),el('span','rr-note','每次上游尝试记录一个主因'));reasonCard.append(reasonHeading);const reasonColumns=el('div','rr-chart-columns rr-reason-columns');reasonColumns.append(el('span','','原因'),el('span','',''),el('span','','次数'));reasonCard.append(reasonColumns);const reasons=data?.reasons||[],reasonMax=Math.max(0,...reasons.map(item=>number(item.attempts)??0));if(reasons.length)for(const item of reasons)reasonCard.append(barRow(item.reason==='host_selected'?'宿主未提供原因':reasonName(item.reason),item.attempts,reasonMax,()=>drill({search:item.reason}),item.reason==='failover'?'warning':!own(reasonNames,item.reason)||['unknown','host_selected'].includes(item.reason)?'unknown':''));else reasonCard.append(empty('暂无原因记录','未记录的原因不会依据当前状态补造。'));reasonCard.append(el('p','rr-note','次要因素在请求详情中查看。'));grid.append(reasonCard);content.push(grid);
      const matrix=el('section','rr-analysis-card rr-matrix'),heading=el('div','rr-card-heading');heading.append(el('h2','',(matrixGroup==='project'?'项目':matrixGroup==='caller'?'调用方':'会话')+' → 账号'),el('span','rr-note','实际分流矩阵 · 点击单元格查看对应请求'));const grouping=el('div','rr-segment');for(const [value,label]of [['project','项目'],['caller','调用方'],['session','会话']]){const b=button(label,()=>{matrixGroup=value;renderAnalysis()},'rr-segment-button');b.setAttribute('aria-pressed',String(matrixGroup===value));b.disabled=value!=='project'&&!Array.isArray(value==='caller'?data?.callers:data?.sessions);if(b.disabled)b.title='当前后台未提供此维度聚合';grouping.append(b);}heading.append(grouping);matrix.append(heading);const matrixData=matrixView(data,matrixGroup);if(matrixData.rows.length&&matrixData.accounts.length){const scroll=el('div','rr-table-scroll'),table=el('table','rr-matrix-table'),thead=el('thead'),tr=el('tr');for(const title of [matrixGroup==='project'?'项目':matrixGroup==='caller'?'调用方':'会话','用户请求','上游尝试',...matrixData.accounts.map(account),'换号恢复'])tr.append(el('th','',title));thead.append(tr);table.append(thead);const tbody=el('tbody');for(const item of matrixData.rows){const row=el('tr'),label=matrixGroup==='project'?projectLabel(item):matrixGroup==='caller'?callerLabel(item.caller):clippedID(item.session);row.append(el('th','',label),el('td','',formatNumber(item.requests)),el('td','',formatNumber(item.attempts)));for(const id of matrixData.accounts){const cell=el('td'),value=item.accounts&&typeof item.accounts==='object'&&!own(item.accounts,id)?data?.aggregation_limited?null:0:count(item.accounts?.[id]),filter=matrixDrillFilter(matrixGroup,item,id),b=button(formatNumber(value),()=>{if(filter)drill(filter)},'rr-matrix-value');b.disabled=!filter;if(!filter)b.title='此维度未记录标识，无法精确筛选对应请求';if(value!==null&&matrixData.max>0)cell.style.setProperty('--rr-cell-opacity',String(.04+.3*value/matrixData.max));cell.append(b);row.append(cell);}row.append(el('td','',formatNumber(item.recovered)));tbody.append(row);}const totalRow=el('tr','rr-matrix-total');totalRow.append(el('th','',data?.aggregation_limited?'展示小计':'合计'));for(const name of ['requests','attempts'])totalRow.append(el('td','',formatNumber(matrixData.rows.every(item=>count(item[name])!==null)?matrixData.rows.reduce((sum,item)=>sum+item[name],0):null)));for(const id of matrixData.accounts)totalRow.append(el('td','',formatNumber(data?.aggregation_limited&&matrixData.rows.some(item=>!own(item.accounts,id))?null:matrixData.rows.reduce((sum,item)=>sum+(count(item.accounts?.[id])??0),0))));totalRow.append(el('td','',formatNumber(matrixData.rows.every(item=>count(item.recovered)!==null)?matrixData.rows.reduce((sum,item)=>sum+item.recovered,0):null)));tbody.append(totalRow);table.append(tbody);scroll.append(table);matrix.append(scroll);}else matrix.append(empty('暂无分流矩阵','有已观测请求后显示项目与执行账号的对应关系。'));
      matrix.append(el('p','rr-methodology','用户请求与上游尝试分别统计；尝试包含重试，未发起的请求不计入尝试。已知结果与未知结果分别保留，不将未知计作失败。'));content.push(matrix);replace(analysis,content);
    }
    function render() {root.hidden=page==='tasks';workspace.hidden=page==='analysis';analysis.hidden=page!=='analysis';renderSummary();renderFeedback();if(page==='analysis')renderAnalysis();else if(page!=='tasks'){renderList();renderDetail();}notify();}
    async function select(record,opener) {selectedOpener=opener;copyFallbackValue='';selected=record;selectedRevision=data?.view_revision??-1;detail=null;detailRevision=-1;detailError='';render();void loadDetail();if(window.matchMedia?.('(max-width: 1100px)').matches){const heading=detailPanel.querySelector('h2');heading?.scrollIntoView?.({block:'start'});heading?.focus?.({preventScroll:true});} }
    function errorMessage(failure,detailFailure=false) {if(failure?.status===404)return '当前接入层尚未提供运行记录接口，请更新并重启 CPA 接入层。';if(failure?.status===401||failure?.status===403||failure?.code==='unauthorized')return '认证已失效，请重新连接。';if(failure?.message==='invalid_traffic_response')return '运行记录响应无法核实，请重新读取。';return detailFailure?'详情读取失败，保留最后已读取的记录。':'运行记录读取失败，保留最后已读取的数据。';}
    async function refresh(explicit=false) {
      if(disposed||!connected||page==='tasks')return;if(listPending&&!explicit)return listPending;listController?.abort(); listController = new AbortController(); const signal=listController.signal;const currentPage=page,ticket=gate.begin('list'),query=trafficQuery(page,readFilters());loading=true;if(!data)render();else renderFeedback();
      const work=(async()=>{try{const payload=await options.request(query,{signal});validateEnvelope(payload,currentPage);if(disposed||!connected||page!==currentPage||!gate.accept(ticket,payload))return;data=payload;error='';rememberFacets(payload);if(currentPage!=='analysis'){readFilters().page=payload.page;const updated=selected&&payload.records.find(item=>item.id===selected.id);if(updated){selected=updated;selectedRevision=payload.view_revision;}else if(!selected&&payload.records.length){selected=payload.records[0];selectedRevision=payload.view_revision;detail=null;detailRevision=-1;void loadDetail();}}}catch(failure){if(disposed||ticket.epoch!==gate.epoch||page!==currentPage||gate.sequence.get('list')!==ticket.serial)return;error=errorMessage(failure);}finally{if(!disposed&&ticket.epoch===gate.epoch&&page===currentPage&&gate.sequence.get('list')===ticket.serial){loading=false;listPending=null;render();if(selected&&!detailPending)void loadDetail();}}})();listPending=work;return work;
    }
    async function loadDetail() {
      if(!connected||!selected||page==='tasks'||page==='analysis')return;const id=selected.id,currentPage=page,pendingKey=currentPage+':'+id;if(detailPending===pendingKey)return;const ticket=gate.begin('detail');detailPending=pendingKey;detailController?.abort();detailController=new AbortController();
      try{const payload=await options.request(trafficQuery(page,{},id),{signal:detailController.signal});const record=validateEnvelope(payload,currentPage,true);if(disposed||!connected||page!==currentPage||selected?.id!==id||record.id!==id||!gate.accept(ticket,payload))return;detail=record;detailRevision=payload.view_revision;detailError='';renderDetail();}catch(failure){if(ticket.epoch===gate.epoch&&selected?.id===id&&page===currentPage){detailError=errorMessage(failure,true);renderDetail();}}finally{if(detailPending===pendingKey)detailPending=null;}
    }
    function show(next) {if(!['users','plugin','analysis','tasks'].includes(next))next='users';if(page!==next){copyFallbackValue='';listController?.abort();detailController?.abort();gate.reset(gate.instance);data=null;selected=null;detail=null;detailRevision=-1;selectedRevision=-1;error='';detailError='';loading=false;listPending=null;detailPending=null;clearTimeout(timer);}page=next;root.dataset.recordPage=page;updateFilterControls();render();if(connected&&page!=='tasks')void refresh();}
    function disconnect() {copyFallbackValue='';connected=false;listController?.abort();detailController?.abort();gate.reset(gate.instance);clearTimeout(timer);listPending=null;detailPending=null;loading=false;render();}
    function setContext(value={}) {if(typeof value.displayAccount==='function')accountDisplay=value.displayAccount;const instance=typeof value.instanceId==='string'&&value.instanceId?value.instanceId:gate.instance;if(instance!==gate.instance){copyFallbackValue='';listController?.abort();detailController?.abort();gate.reset(instance);data=null;selected=null;detail=null;detailError='';listPending=null;detailPending=null;knownFacets={projects:new Map(),callers:new Map(),accounts:new Map(),models:new Map()};}const prior=connected;if(typeof value.connected==='boolean')connected=value.connected;if(!connected&&prior)disconnect();else{render();if(connected&&!prior&&page!=='tasks')void refresh();}}
    function destroy(){disposed=true;connected=false;listController?.abort();detailController?.abort();gate.reset();clearTimeout(timer);for(const remove of listeners)remove();root.replaceChildren();root.classList.remove('runtime-records');}
    updateFilterControls();render();return {show,refresh,disconnect,setContext,destroy};
  }

  const exported={mount,callerLabel,projectLabel,reportedTokenTotal,recordStatus,pluginStatus,reasonName,reasonExplanation,triggerName,kindName,formatNumber,duration,localRange,trafficQuery,validateEnvelope,VersionGate,mergeRecord,matrixView,sameSessionFilter,matrixDrillFilter};
  if(typeof module==='object'&&module.exports)module.exports=exported;
  if(context&&typeof context==='object')context.CWRRecords=exported;
})(typeof window==='object'?window:globalThis);
