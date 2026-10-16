'use strict';
(() => {
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
  if (typeof module !== 'undefined' && module.exports) module.exports = {ViewTracker};
  if (typeof document === 'undefined') return;
  const root = '/v0/management/codex-window-refresh/v2';
  const $ = id => document.getElementById(id);
  let key = '', generation = 0, busy = false, timer, statusTimer, after = '', accountsRevision = 0, views = new ViewTracker();
  async function api(path, options = {}) {
    const response = await fetch(root + path, {...options, headers: {'Authorization': 'Bearer ' + key, 'Content-Type': 'application/json', ...(options.headers || {})}, cache: 'no-store', signal: AbortSignal.timeout(10000)});
    const data = await response.json();
    if (!response.ok) { const error = new Error(data.error || '读取失败'); error.status = response.status; error.code = data.error; throw error; }
    return data;
  }
  function error(e) { $('error').textContent = '请求失败：' + e.message + '。已显示的数据可能已过期。'; }
  function authenticationFailed(e) {
    if (e.status !== 401 && e.status !== 403) return false;
    generation++; clearTimeout(timer); clearTimeout(statusTimer); busy = false;
    $('query').disabled = true; $('refresh').disabled = true; $('more').disabled = true;
    $('connection').textContent = '认证失效，请重新连接'; error(e); return true;
  }
  function showAccounts(data, append) {
    if (!append) $('accounts').replaceChildren();
    for (const account of data.accounts) {
      const row = document.createElement('div'); row.className = 'account';
      const title = document.createElement('strong'); title.textContent = account.id + ' · ' + account.status;
      const date = document.createElement('p'); date.textContent = account.observed_at ? '额度观测于 ' + account.observed_at : '尚无成功额度观测';
      const detail = document.createElement('pre'); detail.textContent = account.quota ? JSON.stringify(account.quota, null, 2) : '';
      row.append(title, date, detail); $('accounts').append(row);
    }
    if (!append && !data.accounts.length) $('accounts').textContent = '尚无已保存账号，请创建查询任务。';
    after = data.next_after; $('more').hidden = !after;
    accountsRevision = data.accounts_revision;
  }
  async function accounts(append = false) {
    const current = generation, sequence = views.begin('accounts'), previousInstance = views.instance;
    $('more').disabled = true;
    try {
      const data = await api('/accounts' + (append ? '?after=' + encodeURIComponent(after) + '&revision=' + accountsRevision : ''));
      if (current !== generation || !views.accept('accounts', sequence, data)) return;
      if (!Number.isSafeInteger(data.accounts_revision) || !Array.isArray(data.accounts)) throw new Error('账号视图协议错误');
      if (append && previousInstance !== data.instance_id) return accounts(false);
      showAccounts(data, append);
    } catch (e) {
      if (current !== generation) return;
      if (append && e.code === 'view_changed') return accounts(false);
      throw e;
    } finally { if (current === generation) $('more').disabled = false; }
  }
  async function overview() {
    const current = generation, sequence = views.begin('overview');
    const data = await api('/overview');
    if (current !== generation || !views.accept('overview', sequence, data)) return null;
    if (!data.read_only) throw new Error('后台协议不兼容');
    const activity = data.activity;
    const inventory = data.inventory;
    $('inventory').textContent = inventory?.status === 'ready'
      ? 'CPA 账号已同步 · 最近同步 ' + inventory.last_success_at + ' · 正常业务中的额度信息会自动保存'
      : 'CPA 账号同步尚未就绪' + (inventory?.error_code ? '：' + inventory.error_code : '') + '；保留已保存结果';
    $('activity').textContent = activity?.connected
      ? '已观测近期身份 ' + activity.observed_recent_users + ' · 在途 ' + activity.observed_active_requests + ' · 未知占用 ' + activity.unknown_requests + ' · ' + (activity.status === 'gap' ? '观测存在缺口：' + activity.reason : '初始在途状态尚未核实')
      : 'CPA 活动观测尚未连接；显示的是已保存数据。';
    return data;
  }
  function monitor(current) {
    statusTimer = setTimeout(async () => {
      if (current !== generation) return;
      try {
        const data = await overview();
        if (current === generation && data) {
          $('connection').textContent = '独立后台已连接 · 查询并发 ' + data.query_concurrency;
          if (data.accounts_revision !== accountsRevision) await accounts();
        }
      } catch (e) {
        if (current !== generation || authenticationFailed(e)) return;
        $('connection').textContent = '后台连接中断，正在重试'; error(e);
      }
      if (current === generation) monitor(current);
    }, 5000);
  }
  async function poll(id, current) {
    if (current !== generation) return;
    try {
      const resource = 'job:' + id, sequence = views.begin(resource);
      const job = await api('/jobs?job_id=' + encodeURIComponent(id));
      if (current !== generation) return;
      if (!views.accept(resource, sequence, job)) { timer = setTimeout(() => poll(id, current), 1000); return; }
      const completed = job.items.filter(i => i.phase === 'completed').length;
      const failed = job.items.filter(i => i.phase === 'failed').length;
      $('job').textContent = job.job_id + ' · ' + job.phase + ' · 成功 ' + completed + ' / 失败 ' + failed + ' / 共 ' + job.items.length + (job.error_code ? ' · ' + job.error_code : '');
      const failures = job.items.filter(i => i.error_code).slice(0, 10);
      if (failures.length) $('error').textContent = failures.map(i => i.account_id + '：' + i.error_code).join('；');
      if (['completed', 'failed', 'cancelled'].includes(job.phase)) { busy = false; $('query').disabled = false; await Promise.all([accounts(), overview()]); return; }
      await overview();
    } catch (e) {
      if (current !== generation) return;
      if (e.status === 404) { busy = false; $('query').disabled = false; $('job').textContent = id + ' · 任务记录已过期或不可用'; return; }
      if (authenticationFailed(e)) return;
      error(e);
    }
    timer = setTimeout(() => poll(id, current), 1000);
  }
  $('connect').onclick = async () => {
    generation++; clearTimeout(timer); clearTimeout(statusTimer); busy = false; key = $('key').value; views = new ViewTracker(); after = ''; const current = generation;
    $('query').disabled = true; $('refresh').disabled = true;
    try {
      const data = await overview(); if (current !== generation || !data) return;
      $('connection').textContent = '独立后台已连接 · 查询并发 ' + data.query_concurrency;
      $('query').disabled = false; $('refresh').disabled = false; $('error').textContent = ''; monitor(current); await accounts();
    } catch (e) { if (current === generation) { $('connection').textContent = '连接失败'; error(e); } }
  };
  $('query').onclick = async () => {
    if (busy) return; busy = true; $('query').disabled = true; $('error').textContent = '';
    const current = generation, body = {kind: 'quota_query', account_id: $('account').value.trim()};
    const idempotency = crypto.randomUUID();
    try {
      // A transport failure may follow a successful acceptance. Retry with the
      // same key, never submit a second logically different operation.
      const options = {method: 'POST', headers: {'Idempotency-Key': idempotency}, body: JSON.stringify(body)};
      let result; try { result = await api('/jobs', options); } catch { result = await api('/jobs', options); }
      if (current !== generation) return;
      views.barrier();
      $('job').textContent = result.job_id + ' · queued'; poll(result.job_id, current);
    } catch (e) { if (current === generation) { busy = false; $('query').disabled = false; error(e); } }
  };
  $('refresh').onclick = () => Promise.all([accounts(), overview()]).catch(error);
  $('more').onclick = () => accounts(true).catch(error);
})();
