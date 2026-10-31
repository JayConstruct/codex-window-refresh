'use strict';
(() => {
  const finiteCount = value => Number.isSafeInteger(value) && value >= 0 ? value : null;
  const stamp = value => typeof value === 'string' && !value.startsWith('0001-') ? Date.parse(value) : NaN;

  function fiveHourUnstarted(quota) {
    const window = quota?.five_hour;
    if (window?.Remaining?.Unit !== 'percent' || window.Remaining.Value !== 100 || window.State === 'active' || window.State === 'expired') return false;
    if (window.State === 'inactive') return true;
    // Compare with the original observation, never the current browser time.
    // An explicit upstream countdown takes precedence over rounded timestamps.
    if (Object.hasOwn(window, 'ResetAfterSeconds') && window.ResetAfterSeconds != null) return window.ResetAfterSeconds === 18000;
    const observed = stamp(window.ConfirmedAt || quota.fields?.five_hour?.observed_at || quota.observed_at);
    return Math.floor(stamp(window.ResetAt) / 1000) - Math.floor(observed / 1000) === 18000;
  }

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

  function quotaAvailable(account, now = Date.now()) {
    const quota = account?.quota;
    if (account?.status !== 'available' || quota?.permission !== 'allowed') return false;
    for (const field of ['permission', 'five_hour', 'weekly']) {
      const observed = stamp(Object.hasOwn(quota.fields || {}, field) ? quota.fields[field]?.observed_at : quota.observed_at);
      if (!Number.isFinite(observed) || observed > now) return false;
    }
    return ['five_hour', 'weekly'].every(field => {
      const window = quota[field], remaining = window?.Remaining;
      if (!['active', 'inactive', 'unknown'].includes(window?.State) || typeof remaining?.Value !== 'number' || !Number.isFinite(remaining.Value) || remaining.Value <= 0 || remaining.Unit === 'percent' && remaining.Value > 100) return false;
      const reset = stamp(window.ResetAt);
      return window.State === 'active' ? reset > now : !Number.isFinite(reset) || reset > now;
    });
  }

  function pendingQueryView(query, now = Date.now()) {
    if (!['queued', 'running'].includes(query?.phase)) return null;
    if (query.phase === 'running') return {label: '查询中', kind: 'unavailable'};
    if (query.hold_reason) return {label: query.hold_reason === 'excluded_hours' ? '等待允许查询时段' : '查询暂缓', kind: 'unavailable'};
    const seconds = Math.ceil((stamp(query.scheduled_at) - now) / 1000);
    return {label: seconds > 0 ? seconds + ' 秒后查询' : '等待查询', kind: 'unavailable'};
  }

  function accountView(account, now = Date.now()) {
    if (account.status === 'disabled') return {label: '已禁用', kind: 'disabled'};
    if (account.status === 'removed') return {label: '已移除', kind: 'disabled'};
    if (account.status === 'binding_unavailable') return {label: '身份待核实', kind: 'unavailable'};
    const resetDue = ['five_hour', 'weekly'].some(field => stamp(account.quota?.[field]?.ResetAt) <= now);
    const pending = resetDue || ['five_hour_expiry', 'weekly_expiry'].includes(account.pending_query?.trigger) ? pendingQueryView(account.pending_query, now) : null;
    if (pending && ['available', 'unavailable'].includes(account.status)) return {...pending, description: '按后台已保存的查询计划执行；倒计时结束后等待实际查询结果更新额度。'};
    if (account.status !== 'available') return {label: account.status === 'unavailable' ? '暂不可用' : '状态未知', kind: 'unavailable'};
    const empty = field => account.quota?.[field]?.Remaining?.Unit === 'percent' && account.quota[field].Remaining.Value === 0;
    if (empty('weekly')) return {label: '周额度耗尽', kind: 'attention'};
    if (empty('five_hour')) return {label: '五小时耗尽', kind: 'attention'};
    if (account.display_status === 'quota_exhausted') return {label: '额度耗尽', kind: 'attention'};
    if (account.display_status === 'quota_restricted' || account.quota?.permission === 'blocked') return {label: '额度受限', kind: 'attention'};
    const evidence = quotaAssessment(account, now), fields = evidence.fields.join('、');
    if (evidence.usable) return {label: '可用', kind: 'available'};
    if (quotaAvailable(account, now)) {
      if (fiveHourUnstarted(account.quota)) return {label: '窗口未启动', kind: 'available', description: '最近观测五小时额度剩余 100%，上游返回完整的 18000 秒，表示五小时窗口未启动。查询额度不会启动窗口；该账号有可用余额，但不计入已启动的活动窗口容量。' + (evidence.reason === 'stale' ? '显示的是上次观测结果。' : '')};
      return {label: '可用', kind: 'available', description: evidence.reason === 'stale' ? 'CPA 账号状态正常；' + fields + '的观测已超过 5 分钟，显示的是上次观测额度，不表示账号不可用。活动窗口统计仍使用新鲜证据。' : '额度数据已更新，最近观测有可用额度；' + fields + '窗口是否已启动尚未确认。100% 剩余额度本身不能证明窗口已启动，查询额度也不会启动窗口。'};
    }
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
    if (!data) return {capacity: null, usable: null, enabled: null, exhausted: null, other: null, running: null, unknownTasks: null, unknownAccounts: null, attention: null};
    const total = finiteCount(data.total), disabled = finiteCount(data.disabled);
    const enabled = finiteCount(data.enabled) ?? (total !== null && disabled !== null ? Math.max(0, total - disabled) : null);
    const capacity = finiteCount(data.capacity), exhausted = finiteCount(data.quota_exhausted);
    const usable = finiteCount(data.quota_available) ?? capacity;
    const other = [enabled, usable, exhausted].every(value => value !== null) ? Math.max(0, enabled - usable - exhausted) : null;
    const counts = history?.status_counts;
    const running = finiteCount(counts?.running), queued = finiteCount(counts?.queued);
    const unknownTasks = finiteCount(counts?.unknown), unknownAccounts = finiteCount(data.unknown_enabled);
    return {capacity, usable, enabled, exhausted, other, running: running !== null && queued !== null ? running + queued : null, unknownTasks, unknownAccounts,
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

  const api = {fiveHourUnstarted, quotaEvidence, quotaAssessment, quotaAvailable, pendingQueryView, accountView, overviewSummary, matchingProjects, sortAccounts};
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  if (typeof globalThis !== 'undefined') globalThis.CWRPresentation = api;
})();
