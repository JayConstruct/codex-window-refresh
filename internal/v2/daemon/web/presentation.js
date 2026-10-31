'use strict';
(() => {
  const finiteCount = value => Number.isSafeInteger(value) && value >= 0 ? value : null;
  const stamp = value => typeof value === 'string' && !value.startsWith('0001-') ? Date.parse(value) : NaN;

  function quotaAssessment(account, now = Date.now()) {
    const quota = account?.quota;
    const names = {permission: '许可状态', five_hour: '五小时额度', weekly: '周额度'};
    const result = (reason, fields = [], known = false, usable = false) => ({known, usable, reason, fields});
    if (!quota) return result('unobserved');
    const missing = ['permission', 'five_hour', 'weekly'].filter(field => field === 'permission' ? !['allowed', 'blocked'].includes(quota.permission) : !quota[field]);
    if (missing.length) return result('incomplete', missing.map(field => names[field]));
    const stale = [], future = [], invalid = [];
    for (const field of ['permission', 'five_hour', 'weekly']) {
      const observed = stamp(Object.hasOwn(quota.fields || {}, field) ? quota.fields[field]?.observed_at : quota.observed_at);
      if (!Number.isFinite(observed)) invalid.push(names[field]);
      else if (observed > now) future.push(names[field]);
      else if (now - observed > 300000) stale.push(names[field]);
    }
    if (future.length) return result('future', future);
    if (invalid.length) return result('incomplete', invalid);
    if (stale.length) return result('stale', stale);
    const unconfirmed = [], resetDue = [];
    for (const field of ['five_hour', 'weekly']) {
      const value = quota[field];
      if (!['active', 'inactive', 'expired', 'unknown'].includes(value.State) || typeof value.Remaining?.Value !== 'number' || !Number.isFinite(value.Remaining.Value) || value.Remaining.Value < 0) return result('incomplete', [names[field]]);
      if (value.State === 'unknown') unconfirmed.push(names[field]);
      if (value.State === 'active' && !(stamp(value.ResetAt) > now)) resetDue.push(names[field]);
    }
    if (resetDue.length) return result('reset_due', resetDue);
    if (unconfirmed.length) return result('unconfirmed', unconfirmed);
    const usable = account.status === 'available' && quota.permission === 'allowed' && quota.five_hour.State === 'active' && quota.five_hour.Remaining.Value > 0 && quota.weekly.State !== 'expired' && quota.weekly.Remaining.Value > 0;
    const expired = ['five_hour', 'weekly'].filter(field => quota[field].State === 'expired');
    return result(expired.length ? 'reset_due' : quota.five_hour.State === 'inactive' ? 'inactive' : '', expired.map(field => names[field]), true, usable);
  }

  function quotaEvidence(account, now = Date.now()) {
    const {known, usable} = quotaAssessment(account, now);
    return {known, usable};
  }

  function accountView(account, now = Date.now()) {
    if (account.status === 'disabled') return {label: '已禁用', kind: 'disabled'};
    if (account.status === 'removed') return {label: '已移除', kind: 'disabled'};
    if (account.status === 'binding_unavailable') return {label: '身份待核实', kind: 'unavailable'};
    if (account.status !== 'available') return {label: account.status === 'unavailable' ? '暂不可用' : '状态未知', kind: 'unavailable'};
    const empty = field => account.quota?.[field]?.Remaining?.Unit === 'percent' && account.quota[field].Remaining.Value === 0;
    if (empty('weekly')) return {label: '周额度耗尽', kind: 'attention'};
    if (empty('five_hour')) return {label: '五小时耗尽', kind: 'attention'};
    if (account.display_status === 'quota_exhausted') return {label: '额度耗尽', kind: 'attention'};
    if (account.display_status === 'quota_restricted' || account.quota?.permission === 'blocked') return {label: '额度受限', kind: 'attention'};
    const evidence = quotaAssessment(account, now), fields = evidence.fields.join('、');
    if (evidence.usable) return {label: '可用', kind: 'available'};
    if (evidence.reason === 'stale') return {label: '正常', kind: 'available', description: 'CPA 账号状态正常；' + fields + '的观测已超过 5 分钟，显示的是上次观测额度。观测时效只影响已核实窗口统计，不表示账号不可用；最近查询可能只更新了其他字段。'};
    if (evidence.reason === 'unconfirmed') return {label: '窗口待核实', kind: 'unavailable', description: '额度数据已更新，但' + fields + '窗口是否已启动尚未确认。100% 剩余额度本身不能证明窗口已启动，查询额度也不会启动窗口。'};
    if (evidence.reason === 'reset_due') return {label: '窗口待核实', kind: 'unavailable', description: fields + '已到重置时间，等待新的窗口证据；不按本地倒计时自动判定恢复。'};
    if (evidence.reason === 'incomplete') return {label: '额度数据不完整', kind: 'unavailable', description: '尚缺可核实的' + fields + '信息。查询已返回的百分比仍正常展示，未返回字段不推算为可用。'};
    if (evidence.reason === 'future') return {label: '正常', kind: 'available', description: 'CPA 账号状态正常；' + fields + '的观测时间晚于当前页面时间，暂无法判断额度新鲜度，请检查浏览器与后台的系统时间。'};
    if (evidence.reason === 'inactive') return {label: '窗口未启动', kind: 'unavailable', description: '五小时窗口尚未启动，查询额度只读取用量，不发送模型请求。'};
    return evidence.known ? {label: '正常', kind: 'available'} : {label: '待核实', kind: 'unavailable', description: '尚未取得额度观测，请查询后查看结果。'};
  }

  function overviewSummary(overview, history) {
    const data = overview?.dashboard;
    if (!data) return {capacity: null, enabled: null, exhausted: null, other: null, running: null, unknownTasks: null, unknownAccounts: null, attention: null};
    const total = finiteCount(data.total), disabled = finiteCount(data.disabled);
    const enabled = finiteCount(data.enabled) ?? (total !== null && disabled !== null ? Math.max(0, total - disabled) : null);
    const capacity = finiteCount(data.capacity), exhausted = finiteCount(data.quota_exhausted);
    const other = [enabled, capacity, exhausted].every(value => value !== null) ? Math.max(0, enabled - capacity - exhausted) : null;
    const counts = history?.status_counts;
    const running = finiteCount(counts?.running), queued = finiteCount(counts?.queued);
    const unknownTasks = finiteCount(counts?.unknown), unknownAccounts = finiteCount(data.unknown_enabled);
    return {capacity, enabled, exhausted, other, running: running !== null && queued !== null ? running + queued : null, unknownTasks, unknownAccounts,
      attention: unknownTasks !== null && unknownAccounts !== null ? unknownTasks + unknownAccounts + (overview.last_error ? 1 : 0) + (overview.inventory?.error_code ? 1 : 0) : null};
  }

  function matchingProjects(accountID, config) {
    return (config?.projects || []).filter(project => project.accounts?.includes(accountID)).map(project => project.name || project.project);
  }

  function sortAccounts(accounts, order, now = Date.now()) {
    const result = [...accounts];
    if (order === 'attention') result.sort((a, b) => {
      const weight = account => ({unavailable: 0, attention: 1, available: 2, disabled: 3})[accountView(account, now).kind];
      return weight(a) - weight(b) || a.id.localeCompare(b.id);
    });
    if (order === 'five-hour' || order === 'weekly') {
      const field = order === 'weekly' ? 'weekly' : 'five_hour';
      const amount = account => account.quota?.[field]?.Remaining?.Unit === 'percent' && typeof account.quota[field].Remaining.Value === 'number' ? account.quota[field].Remaining.Value : Infinity;
      result.sort((a, b) => amount(a) - amount(b) || a.id.localeCompare(b.id));
    }
    return result;
  }

  const api = {quotaEvidence, quotaAssessment, accountView, overviewSummary, matchingProjects, sortAccounts};
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  if (typeof globalThis !== 'undefined') globalThis.CWRPresentation = api;
})();
