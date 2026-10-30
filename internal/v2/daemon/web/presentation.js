'use strict';
(() => {
  const finiteCount = value => Number.isSafeInteger(value) && value >= 0 ? value : null;
  const stamp = value => typeof value === 'string' && !value.startsWith('0001-') ? Date.parse(value) : NaN;

  function quotaEvidence(account, now = Date.now()) {
    const quota = account?.quota;
    if (!quota || !['allowed', 'blocked'].includes(quota.permission)) return {known: false, usable: false};
    for (const field of ['permission', 'five_hour', 'weekly']) {
      const observed = stamp(Object.hasOwn(quota.fields || {}, field) ? quota.fields[field]?.observed_at : quota.observed_at);
      if (!Number.isFinite(observed) || observed > now || now - observed > 300000) return {known: false, usable: false};
    }
    for (const field of ['five_hour', 'weekly']) {
      const value = quota[field];
      if (!value || !['active', 'inactive', 'expired'].includes(value.State) || typeof value.Remaining?.Value !== 'number' || !Number.isFinite(value.Remaining.Value) || value.Remaining.Value < 0) return {known: false, usable: false};
      if (value.State === 'active' && !(stamp(value.ResetAt) > now)) return {known: false, usable: false};
    }
    return {known: true, usable: account.status === 'available' && quota.permission === 'allowed' && quota.five_hour.State === 'active' && quota.five_hour.Remaining.Value > 0 && quota.weekly.State !== 'expired' && quota.weekly.Remaining.Value > 0};
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
    const evidence = quotaEvidence(account, now);
    return evidence.usable ? {label: '可用', kind: 'available'} : evidence.known ? {label: '正常', kind: 'available'} : {label: account.observed_at ? '观测已过期' : '待核实', kind: 'unavailable'};
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

  const api = {quotaEvidence, accountView, overviewSummary, matchingProjects, sortAccounts};
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  if (typeof globalThis !== 'undefined') globalThis.CWRPresentation = api;
})();
