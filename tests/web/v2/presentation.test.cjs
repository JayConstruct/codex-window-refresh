'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const {quotaEvidence, quotaAssessment, accountView, overviewSummary} = require('../../../internal/v2/daemon/web/presentation.js');

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
    assert.equal(state.label, '窗口待核实'); assert.match(state.description, /额度数据已更新/);
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
  assert.equal(stale.label, '正常'); assert.equal(stale.kind, 'available');
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
  assert.equal(accountView(account, at).label, '正常');
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
