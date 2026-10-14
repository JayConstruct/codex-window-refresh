'use strict';
const {test} = require('node:test');
const assert = require('node:assert/strict');
const {historyTimeRange} = require('../../internal/plugin/web/panel.js');

test('history date shortcuts use local calendar days and leave the upper bound open for new records', () => {
  const now = new Date(2026, 0, 3, 15, 42, 18);
  const today = historyTimeRange('today', now.getTime());
  assert.equal(today.from, new Date(2026, 0, 3).toISOString());
  assert.equal(today.until, '');
  const week = historyTimeRange('week', now.getTime());
  assert.equal(week.from, new Date(2025, 11, 28).toISOString());
  assert.equal(week.until, '');
  assert.equal(now.getHours(), 15, 'Computing a shortcut must not mutate its source date');
});

test('clearing a date shortcut imposes no date restriction', () => {
  assert.deepEqual(historyTimeRange('all'), {from: '', until: ''});
});
