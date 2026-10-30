'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const {quotaEvidence, accountView, overviewSummary} = require('../../../internal/v2/daemon/web/presentation.js');

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
