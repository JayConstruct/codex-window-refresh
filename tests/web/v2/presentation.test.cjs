'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const {quotaEvidence, quotaAssessment, accountView, overviewSummary} = require('../../../internal/v2/daemon/web/presentation.js');
const {fiveHourUnstarted} = require('../../../internal/v2/daemon/web/presentation.js');

test('a full reported five-hour countdown identifies an unstarted window after minutes on the page', () => {
  const at = Date.parse('2026-10-31T00:00:00.750Z'), account = observedAccount(at);
  account.quota.five_hour = {State: 'unknown', ConfirmedAt: new Date(at).toISOString(), ResetAt: new Date(at + 17999000).toISOString(), ResetAfterSeconds: 18000, Remaining: {Value: 100, Unit: 'percent'}};
  const before = JSON.stringify(account);
  for (const elapsed of [0, 120000, 299999]) {
    const state = accountView(account, at + elapsed);
    assert.equal(state.label, '窗口未启动');
    assert.equal(state.kind, 'available');
    assert.match(state.description, /不计入已启动的活动窗口容量/);
    assert.equal(quotaEvidence(account, at + elapsed).usable, false);
  }
  assert.equal(JSON.stringify(account), before);
  delete account.quota.five_hour.ResetAfterSeconds;
  account.quota.five_hour.ResetAt = new Date(Math.floor(at / 1000) * 1000 + 18000000).toISOString();
  assert.equal(fiveHourUnstarted(account.quota), true);
  account.quota.weekly.State = 'unknown'; account.quota.weekly.Remaining.Value = 100;
  assert.equal(accountView(account, at).label, '窗口未启动');
  account.quota.weekly.Remaining.Value = 0;
  assert.equal(accountView(account, at).label, '周额度耗尽');
  account.status = 'disabled';
  assert.equal(accountView(account, at).label, '已禁用');
});

test('a shortened countdown and a five-hour total duration do not imply an unstarted window', () => {
  const at = Date.parse('2026-10-31T00:00:00Z'), account = observedAccount(at);
  const window = account.quota.five_hour;
  window.State = 'unknown'; window.Remaining.Value = 100;
  window.ResetAt = new Date(at + 18000000).toISOString(); window.ResetAfterSeconds = 17999;
  assert.equal(fiveHourUnstarted(account.quota), false);
  window.ResetAfterSeconds = 18001;
  assert.equal(fiveHourUnstarted(account.quota), false);
  window.ResetAfterSeconds = 18000; window.Remaining.Value = 99;
  assert.equal(fiveHourUnstarted(account.quota), false);
  window.Remaining.Value = 100; window.State = 'active';
  assert.equal(fiveHourUnstarted(account.quota), false);
  delete window.ResetAfterSeconds; window.State = 'unknown'; window.ResetAt = new Date(at + 3600000).toISOString();
  assert.equal(fiveHourUnstarted(account.quota), false);
});

test('reset query status counts down without restoring quota and respects disabled accounts', () => {
  const at = Date.parse('2026-10-31T00:00:00Z'), account = observedAccount(at - 3600000);
  account.quota.five_hour.Remaining.Value = 0;
  account.pending_query = {phase:'queued',trigger:'five_hour_expiry',scheduled_at:new Date(at+3000).toISOString()};
  assert.equal(accountView(account, at).label, '3 秒后查询');
  assert.equal(accountView(account, at+1001).label, '2 秒后查询');
  assert.equal(accountView(account, at+3000).label, '等待查询');
  assert.equal(account.quota.five_hour.Remaining.Value, 0);
  account.pending_query.phase='running'; assert.equal(accountView(account, at+4000).label, '查询中');
  account.pending_query.phase='queued'; account.pending_query.hold_reason='excluded_hours';
  assert.equal(accountView(account, at).label, '等待允许查询时段');
  account.status='disabled'; assert.equal(accountView(account, at).label, '已禁用');
});

test('summary separates available balances from fresh active capacity', () => {
  const summary = overviewSummary({dashboard:{enabled:8,quota_available:6,capacity:0,quota_exhausted:1}}, null);
  assert.equal(summary.usable, 6); assert.equal(summary.capacity, 0); assert.equal(summary.other, 1);
});

const observedAccount = at => ({status: 'available', observed_at: new Date(at).toISOString(), quota: {permission: 'allowed', observed_at: new Date(at).toISOString(),
  five_hour: {State: 'active', ResetAt: new Date(at + 3600000).toISOString(), Remaining: {Value: 64, Unit: 'percent'}},
  weekly: {State: 'active', ResetAt: new Date(at + 86400000).toISOString(), Remaining: {Value: 82, Unit: 'percent'}}}});

test('displayed usable capacity requires all fresh fields and active window evidence', () => {
  const at = Date.parse('2026-10-31T00:00:00Z');
  const account = {status: 'available', quota: {permission: 'allowed', observed_at: new Date(at).toISOString(),
    five_hour: {State: 'active', ResetAt: new Date(at + 3600000).toISOString(), Remaining: {Value: 64, Unit: 'percent'}},
    weekly: {State: 'active', ResetAt: new Date(at + 86400000).toISOString(), Remaining: {Value: 82, Unit: 'percent'}}}};
  assert.deepEqual(quotaEvidence(account, at), {known: true, usable: true});
  assert.equal(accountView(account, at).label, '可用');
  assert.equal(quotaEvidence(account, at + 300001).usable, false);
  account.quota.fields = {permission: {observed_at: ''}};
  assert.equal(quotaEvidence(account, at).usable, false);
  delete account.quota.fields;
  account.quota.five_hour.Remaining.Value = 0;
  assert.equal(accountView(account, at).label, '五小时耗尽');
  account.quota.weekly.Remaining.Value = 0;
  assert.equal(accountView(account, at).label, '周额度耗尽');
  assert.equal(accountView({...account, status: 'disabled'}, at).label, '已禁用');
});

test('summary uses the enabled pool and never substitutes missing full-history counts with a preview length', () => {
  const overview = {dashboard: {total: 18, disabled: 2, capacity: 12, quota_exhausted: 3, unknown_enabled: 1}};
  const value = overviewSummary(overview, {status_counts: {running: 2, queued: 0, unknown: 1}});
  assert.equal(value.enabled, 16); assert.equal(value.other, 1); assert.equal(value.attention, 2);
  assert.equal(overviewSummary(overview, null).running, null);
  assert.equal(overviewSummary(null, null).capacity, null);
});

test('fresh 100-percent windows await activation evidence and are never described as stale', () => {
  const at = Date.parse('2026-10-31T00:00:00Z');
  for (const fields of [['five_hour'], ['weekly'], ['five_hour', 'weekly']]) {
    const account = observedAccount(at);
    for (const field of fields) { account.quota[field].Remaining.Value = 100; account.quota[field].State = 'unknown'; }
    assert.deepEqual(quotaEvidence(account, at), {known: false, usable: false});
    const state = accountView(account, at);
    assert.equal(state.label, '可用'); assert.match(state.description, /额度数据已更新/);
    assert.doesNotMatch(state.description, /过期|已超过/);
    assert.match(state.description, /查询额度也不会启动窗口/);
    assert.equal(quotaAssessment(account, at).reason, 'unconfirmed');
  }
});

test('a five-minute evidence timeout changes capacity freshness without changing the CPA account into an unavailable account', () => {
  const at = Date.parse('2026-10-31T00:00:00Z'), account = observedAccount(at);
  const before = JSON.stringify(account);
  assert.equal(accountView(account, at + 300000).label, '可用');
  assert.deepEqual(quotaEvidence(account, at + 300001), {known: false, usable: false});
  const stale = accountView(account, at + 300001);
  assert.equal(stale.label, '可用'); assert.equal(stale.kind, 'available');
  assert.match(stale.description, /CPA 账号状态正常/); assert.match(stale.description, /已超过 5 分钟/);
  assert.equal(quotaAssessment(account, at + 300001).reason, 'stale');
  assert.equal(JSON.stringify(account), before);
  account.status = 'disabled'; assert.equal(accountView(account, at + 300001).label, '已禁用');
  account.status = 'unavailable'; assert.equal(accountView(account, at + 300001).label, '暂不可用');
});

test('missing and old per-field evidence cannot borrow a new query timestamp', () => {
  const at = Date.parse('2026-10-31T00:00:00Z'), account = observedAccount(at);
  delete account.quota.weekly;
  assert.equal(accountView(account, at).label, '额度数据不完整');
  assert.match(accountView(account, at).description, /周额度/);
  assert.deepEqual(quotaEvidence(account, at), {known: false, usable: false});
  account.quota.weekly = observedAccount(at).quota.weekly;
  account.quota.fields = {weekly: {observed_at: new Date(at - 600000).toISOString()}};
  assert.equal(quotaAssessment(account, at).reason, 'stale');
  assert.deepEqual(quotaAssessment(account, at).fields, ['周额度']);
  assert.equal(accountView(account, at).label, '可用');
  assert.match(accountView(account, at).description, /周额度/);
  assert.deepEqual(quotaEvidence(account, at), {known: false, usable: false});
});

test('due reset boundaries, absent observations and clock mismatch have separate explanations', () => {
  const at = Date.parse('2026-10-31T00:00:00Z'), account = observedAccount(at);
  account.quota.five_hour.ResetAt = new Date(at).toISOString();
  assert.equal(quotaAssessment(account, at).reason, 'reset_due');
  assert.equal(accountView(account, at).label, '窗口待核实');
  assert.match(accountView(account, at).description, /已到重置时间/);
  assert.equal(accountView({status: 'available'}, at).label, '待核实');
  account.quota.observed_at = new Date(at + 60000).toISOString();
  assert.equal(quotaAssessment(account, at).reason, 'future');
  assert.equal(accountView(account, at).label, '正常');
  assert.match(accountView(account, at).description, /系统时间/);
  assert.deepEqual(quotaEvidence(account, at), {known: false, usable: false});
});
