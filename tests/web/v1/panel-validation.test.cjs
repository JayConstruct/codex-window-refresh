'use strict';
const {test} = require('node:test');
const assert = require('node:assert/strict');
const {durationSeconds, hoursError, configFieldErrors, createEditor} = require('../../../internal/plugin/web/panel.js');

function policy() {
  return {
    schema_version: 1, enabled: false, timezone: 'Asia/Shanghai',
    activity: {lookback: '30m', accounts_per_user: 1, max_accounts: 10},
    quota_query: {automatic: false, manual: true, interval: '30s', five_hour: {interval: '0s'}},
    ordinary_rules: {daily_enabled: true, allowed_hours: ['08:00-23:00']},
    reset_rules: {five_hour: {enabled: true, random_delay: {min: '0s', max: '10m'}}, weekly: {enabled: true, random_delay: {min: '0s', max: '10m'}, bypass_activity_limit: true, bypass_allowed_hours: true}},
    execution: {model: 'model', prompt: 'Reply OK.', prompt_mode: 'random', prompt_pool: ['Hello.', 'Ready.'], catch_up: true},
    planning: {enabled: false, horizon_days: 7}, daily_rules: [], account_overrides: {}
  };
}

test('duration parsing follows the JSON contract, including days and zero inheritance', () => {
  for (const [text, seconds] of [['0s', 0], ['00m', 0], ['30s', 30], ['5m', 300], ['24h', 86400], ['7d', 604800]]) assert.equal(durationSeconds(text), seconds);
  for (const text of ['30', 30, '1h30m', '0.5h', '-1s', '+1s', '1ms', ' 30s', '30s ', '', '999999999999999999999999999s']) assert.ok(Number.isNaN(durationSeconds(text)), String(text));
});

test('allowed hours accept crossing midnight and adjacent ranges but reject overlaps', () => {
  assert.equal(hoursError([]), '');
  assert.equal(hoursError(['22:00-08:00', '08:00-09:00']), '');
  assert.equal(hoursError(['00:00-24:00']), '');
  for (const values of [['08:00-08:00'], ['22:00-08:00', '07:00-09:00'], ['08:00-12:00', '11:00-13:00'], ['24:00-01:00'], ['08:00-24:01'], ['8:00-12:00']]) assert.ok(hoursError(values), values.join(', '));
});

test('valid defaults and backend boundary values produce no field errors', () => {
  const cfg = policy();
  assert.deepEqual(configFieldErrors(cfg), {});
  Object.assign(cfg.activity, {accounts_per_user: 100, max_accounts: 100000, lookback: '24h'});
  cfg.planning.horizon_days = 30;
  cfg.reset_rules.five_hour.random_delay = {min: '0s', max: '1d'};
  cfg.quota_query.five_hour.interval = '0m';
  assert.deepEqual(configFieldErrors(cfg), {});
  cfg.activity.accounts_per_user = 0; cfg.activity.max_accounts = 0;
  cfg.daily_rules = [{id: 'zero_is_allowed', enabled: true, minimum_ready_accounts: 0, random_time_range: '09:00-09:00', catch_up: true}];
  assert.deepEqual(configFieldErrors(cfg), {});
});

test('numeric settings enforce integer and fractional bounds without HTML step assumptions', () => {
  const cfg = policy();
  cfg.activity.accounts_per_user = 0.05;
  assert.deepEqual(configFieldErrors(cfg), {});
  cfg.activity.accounts_per_user = Infinity; cfg.activity.max_accounts = 1.5; cfg.planning.horizon_days = 31;
  const errors = configFieldErrors(cfg);
  assert.ok(errors['activity.accounts_per_user']); assert.ok(errors['activity.max_accounts']); assert.ok(errors['planning.horizon_days']);
});

test('query intervals enforce shared minimum and retain independent zero inheritance', () => {
  const cfg = policy(); cfg.quota_query.interval = '5m'; cfg.quota_query.five_hour.interval = '1m';
  assert.match(configFieldErrors(cfg)['quota_query.five_hour.interval'], /不能低于公共/);
  cfg.quota_query.five_hour.interval = '0s';
  assert.deepEqual(configFieldErrors(cfg), {});
  cfg.quota_query.interval = '29s';
  assert.match(configFieldErrors(cfg)['quota_query.interval'], /30 秒/);
  cfg.quota_query.five_hour.interval = '2h';
  assert.ok(configFieldErrors(cfg)['quota_query.five_hour.interval']);
});

test('cross-field random delay errors clear once either bound is corrected', () => {
  const cfg = policy(); cfg.reset_rules.five_hour.random_delay.min = '20m';
  assert.match(configFieldErrors(cfg)['reset_rules.five_hour.random_delay.max'], /最长延迟不能小于最短延迟/);
  cfg.reset_rules.five_hour.random_delay.max = '30m';
  assert.deepEqual(configFieldErrors(cfg), {});
  cfg.quota_query.weekly = {expiry: {excluded_hours: ['23:00-06:00'], random_delay: {min: '2d', max: '3d'}}};
  const errors = configFieldErrors(cfg);
  assert.ok(errors['quota_query.weekly.expiry.random_delay.min']); assert.ok(errors['quota_query.weekly.expiry.random_delay.max']);
});

test('timezone validation leaves unsupported well-formed aliases to server tzdata and rejects malformed names', () => {
  for (const timezone of ['UTC', 'Asia/Shanghai', 'America/New_York', 'Etc/GMT+8', 'Factory', 'Mars/Olympus']) assert.equal(configFieldErrors({...policy(), timezone}).timezone, undefined, timezone);
  for (const timezone of ['', 'Local', ' Asia/Shanghai', 'Asia//Shanghai', 'Asia/../Shanghai', 'UTC\u0000', '+08:00']) assert.ok(configFieldErrors({...policy(), timezone}).timezone, timezone);
});

test('prompt validation counts UTF-8 bytes and distinguishes empty inherited pools', () => {
  const cfg = policy(); cfg.execution.prompt_pool = ['好'.repeat(85)];
  assert.deepEqual(configFieldErrors(cfg), {});
  cfg.execution.prompt_pool = ['好'.repeat(86)];
  assert.match(configFieldErrors(cfg)['execution.prompt_pool'], /256 字节/);
  cfg.execution.prompt_pool = [];
  assert.ok(configFieldErrors(cfg)['execution.prompt_pool']);
  cfg.execution.prompt_mode = 'fixed'; cfg.reset_rules.weekly.prompt_pool = [];
  assert.deepEqual(configFieldErrors(cfg), {});
  cfg.reset_rules.weekly.prompt_pool = [' same ', 'same'];
  assert.match(configFieldErrors(cfg)['reset_rules.weekly.prompt_pool'], /重复/);
  cfg.reset_rules.weekly.prompt_pool = ['Hello\nworld'];
  assert.ok(configFieldErrors(cfg)['reset_rules.weekly.prompt_pool']);
});

test('models and fixed prompts respect optional inheritance and enabled rule requirements', () => {
  const cfg = policy(); cfg.execution.model = ''; cfg.reset_rules.five_hour.model = ''; cfg.reset_rules.weekly.model = '';
  assert.deepEqual(configFieldErrors(cfg), {});
  cfg.enabled = true;
  assert.match(configFieldErrors(cfg)['execution.model'], /需要默认模型/);
  cfg.reset_rules.five_hour.model = 'five'; cfg.reset_rules.weekly.model = 'week';
  assert.deepEqual(configFieldErrors(cfg), {});
  cfg.execution.prompt = ' '.repeat(5);
  assert.ok(configFieldErrors(cfg)['execution.prompt']);
  cfg.reset_rules.weekly.model = 'REPLACE_WITH_MODEL';
  assert.ok(configFieldErrors(cfg)['reset_rules.weekly.model']);
});

test('daily rules validate identifiers, capacity and time points with backend semantics', () => {
  const cfg = policy();
  cfg.daily_rules = [{id: 'morning', enabled: true, minimum_ready_accounts: 0, random_time_range: '09:00-09:00', catch_up: true}, {id: 'night', enabled: false, minimum_ready_accounts: 10, random_time_range: '23:59-24:00', catch_up: true}];
  assert.deepEqual(configFieldErrors(cfg), {});
  cfg.daily_rules[1] = {...cfg.daily_rules[1], id: 'morning', minimum_ready_accounts: 11, random_time_range: '23:00-01:00', catch_up: false};
  const errors = configFieldErrors(cfg);
  for (const path of ['daily_rules.1.id', 'daily_rules.1.minimum_ready_accounts', 'daily_rules.1.random_time_range', 'daily_rules.1.catch_up']) assert.ok(errors[path], path);
});

test('unparsed form edits stay dirty without masquerading as unapplied JSON', () => {
  const editor = createEditor(); editor.load({config: policy(), revision: 'r1'});
  editor.markFormDirty(true);
  assert.equal(editor.dirty(), true); assert.equal(editor.formDirty(), true); assert.equal(editor.rawDirty(), false);
  editor.update('activity.max_accounts', 12);
  assert.equal(editor.value().activity.max_accounts, 12);
  editor.markRaw(); assert.equal(editor.rawDirty(), true);
  editor.load({config: policy(), revision: 'r2'}, true);
  assert.equal(editor.formDirty(), false); assert.equal(editor.rawDirty(), false); assert.equal(editor.dirty(), false);
  editor.markFormDirty(true); editor.clear(); assert.equal(editor.dirty(), false);
});

test('daily form edits remain possible while other fields contain semantic errors', () => {
  const editor = createEditor(); editor.load({config: policy(), revision: 'r1'});
  editor.update('planning.horizon_days', 0);
  editor.setDailyRules([{id: 'morning', minimum_ready_accounts: 0, random_time_range: '09:00-09:00', catch_up: true}]);
  assert.equal(editor.value().planning.horizon_days, 0);
  assert.equal(editor.value().daily_rules.length, 1);
  assert.ok(configFieldErrors(editor.value())['planning.horizon_days']);
  editor.markRaw(); assert.throws(() => editor.setDailyRules([]), {code: 'unapplied_json'});
});
