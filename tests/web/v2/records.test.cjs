'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const {
  recordStatus, pluginStatus, reasonExplanation, reasonName, duration, formatNumber,
  reportedTokenTotal, localRange, trafficQuery, validateEnvelope,
  VersionGate, mergeRecord, matrixView, sameSessionFilter, matrixDrillFilter,
} = require('../../../internal/v2/daemon/web/records.js');

const envelope = (instance = 'backend-a', revision = 1) => ({instance_id: instance, view_revision: revision});

test('HTTP response evidence is distinct from execution success or unknown outcome', () => {
  assert.deepEqual(recordStatus({status: 'unknown', http_status: 200}), {label: '结果未知', tone: 'warning'});
  assert.deepEqual(recordStatus({status: 'failed', http_status: 429}), {label: '失败', tone: 'error'});
  assert.deepEqual(recordStatus({status: 'completed', recovered: true}), {label: '已恢复成功', tone: 'success'});
  assert.equal(recordStatus({status: 'verifying'}).label, '核实中');
  assert.equal(recordStatus({status: 'not_sent', attempted: false}).label, '未发起');
  assert.equal(recordStatus({status: 'unrecognized-future-state'}).label, '结果未知');
  assert.equal(recordStatus({status: 'cancelled'}).tone, 'neutral');
});

test('missing measurements remain unknown while an observed zero remains zero', () => {
  for (const value of [undefined, null, NaN, Infinity, -1, '0']) {
    assert.equal(formatNumber(value), '—');
    assert.equal(duration(value), '—');
  }
  assert.equal(formatNumber(0), '0');
  assert.equal(duration(0), '0 毫秒');
  assert.equal(duration(3600), '3.6 秒');
});

test('a completed local plugin task is not presented as proof of upstream delivery', () => {
  assert.deepEqual(pluginStatus({kind: 'query', status: 'completed', attempted: false}), {label: '任务完成', tone: 'neutral'});
  assert.deepEqual(pluginStatus({kind: 'wake', status: 'verifying', response: {completed: true}}), {label: '模型完成', tone: 'success'});
  assert.deepEqual(pluginStatus({kind: 'wake', status: 'unknown', http_status: 200, response: {completed: true}}), {label: '结果未知', tone: 'warning'});
  assert.equal(pluginStatus({kind: 'reset', status: 'completed', evidence: {outcome: 'reset'}}).label, '已兑换');
});

test('reported token totals exclude cached-token counters and do not invent unreported values', () => {
  assert.equal(reportedTokenTotal({input_tokens: null, output_tokens: null, cached_tokens: 100}), null);
  assert.equal(reportedTokenTotal({input_tokens: 8120, output_tokens: 1420, cached_tokens: 6000}), 9540);
  assert.equal(reportedTokenTotal({input_tokens: 10, output_tokens: null}), 10);
  assert.equal(reportedTokenTotal({input_tokens: 0, output_tokens: 0}), 0);
});

test('traffic filters stay in the authenticated relative GET path and preserve literal search identities', () => {
  const path = trafficQuery('users', {search: '<script>&?x=1', project: 'dir-abcdef', caller: 'caller-123', page: 2, limit: 7, range: 'all'});
  assert.match(path, /^\/traffic\?/);
  const query = new URL(path, 'https://local.test').searchParams;
  assert.equal(query.get('search'), '<script>&?x=1');
  assert.equal(query.get('project'), 'dir-abcdef');
  assert.equal(query.get('caller'), 'caller-123');
  assert.equal(query.get('page'), '2');
  assert.equal(query.has('from'), false);
  assert.equal(new URL(trafficQuery('plugin', {}, 'job?x=1&other=2'), 'https://local.test').searchParams.get('id'), 'job?x=1&other=2');
});

test('date presets use local day boundaries and an exclusive end instead of elapsed 24-hour guesses', () => {
  const now = new Date(2026, 9, 31, 15, 45, 0);
  const today = localRange('today', now), sevenDays = localRange('7d', now);
  assert.equal(new Date(today.from).getHours(), 0);
  assert.equal(new Date(today.from).getDate(), 31);
  assert.equal(new Date(today.to).getMonth(), 10);
  assert.equal(new Date(today.to).getDate(), 1);
  assert.equal(new Date(sevenDays.from).getDate(), 25);
  assert.deepEqual(localRange('all', now), {});
});

test('traffic API contracts reject malformed and unversioned responses before they reach the UI', () => {
  const list = {...envelope(), records: [], page: 1, limit: 7, total: 0};
  assert.equal(validateEnvelope(list, 'users'), list);
  assert.throws(() => validateEnvelope({...list, view_revision: NaN}, 'users'), /invalid_traffic_response/);
  assert.throws(() => validateEnvelope({...list, records: [{}]}, 'users'), /invalid_traffic_response/);
  assert.throws(() => validateEnvelope({...list, total: -1}, 'users'), /invalid_traffic_response/);
  assert.throws(() => validateEnvelope({...envelope(), accounts: [], reasons: []}, 'analysis'), /invalid_traffic_response/);
  assert.throws(() => validateEnvelope({...envelope(), record: {id: 'r', attempts: 'not-an-array'}}, 'users', true), /invalid_traffic_response/);
  assert.equal(validateEnvelope({...envelope(), record: {id: 'r', attempts: []}}, 'users', true).id, 'r');
});

test('a slower earlier filter read cannot replace a newer accepted result', () => {
  const gate = new VersionGate('backend-a');
  const early = gate.begin('list'), late = gate.begin('list');
  assert.equal(gate.accept(late, envelope('backend-a', 20)), true);
  assert.equal(gate.accept(early, envelope('backend-a', 25)), false);
  assert.equal(gate.accept(gate.begin('list'), envelope('backend-a', 19)), false);
});

test('disconnect and backend changes reject late results even with larger revisions', () => {
  const gate = new VersionGate('backend-a'), pending = gate.begin('list');
  gate.reset('backend-a');
  assert.equal(gate.accept(pending, envelope('backend-a', 100)), false);
  const earlierInstanceRead = gate.begin('detail');
  gate.reset('backend-b');
  assert.equal(gate.accept(earlierInstanceRead, envelope('backend-a', 1000)), false);
  assert.equal(gate.accept(gate.begin('list'), envelope('backend-a', 1001)), false);
  assert.equal(gate.accept(gate.begin('list'), envelope('backend-b', 1)), true);
});

test('latest list status wins over an older detail while recorded response evidence is retained', () => {
  const latest = {id: 'request-1', status: 'unknown', attempt_count: 2};
  const earlierDetail = {id: 'request-1', status: 'completed', attempts: [{http_status: 200}]};
  const merged = mergeRecord(latest, 20, earlierDetail, 19);
  assert.equal(merged.status, 'unknown');
  assert.equal(merged.attempt_count, 2);
  assert.equal(merged.attempts[0].http_status, 200);
  assert.equal(mergeRecord(latest, 20, {...earlierDetail, status: 'failed'}, 21).status, 'failed');
});

test('routing explanations use recorded decisions rather than current account quota', () => {
  const record = {reason: 'session_affinity', account_id: 'a', quota: {remaining: 100}};
  const before = reasonExplanation(record);
  record.quota.remaining = 0;
  assert.equal(reasonExplanation(record), before);
  assert.match(before, /执行当时/);
  assert.equal(reasonName('host_selected'), '宿主选号');
  assert.match(reasonExplanation({reason: 'host_selected'}), /未提供/);
  assert.equal(reasonName('future-private-code'), '原因未提供');
  assert.match(reasonExplanation({reason: 'unknown', quota: {remaining: 100}}), /不依据当前/);
});

test('routing matrix derives account columns from actual aggregated attempts including earlier failover accounts', () => {
  const data = {accounts: [{account_id: 'final-account'}], projects: [{project: 'p', requests: 1, attempts: 2, recovered: 1, accounts: {'first-account': 1, 'final-account': 1}}]};
  const matrix = matrixView(data);
  assert.deepEqual(matrix.accounts, ['final-account', 'first-account']);
  assert.equal(matrix.rows[0].requests, 1);
  assert.equal(matrix.rows[0].attempts, 2);
  assert.equal(matrix.rows[0].recovered, 1);
  assert.equal(matrixView(data, 'caller').available, false);
  assert.equal(matrixView({...data, callers: []}, 'caller').available, true);
});

test('same-session navigation uses exact session and authenticated caller scope filters', () => {
  assert.equal(sameSessionFilter({session: 'shared-client-label'}), null);
  const filter = sameSessionFilter({session: 'session-hash', caller: 'caller-hash'}, {range: '7d', account: 'previous-account', search: 'old-search'});
  assert.equal(filter.session, 'session-hash');
  assert.equal(filter.caller, 'caller-hash');
  assert.equal(filter.account, '');
  assert.equal(filter.search, '');
  assert.equal(filter.range, '7d');
  const query = new URL(trafficQuery('users', filter), 'https://local.test').searchParams;
  assert.equal(query.get('session'), 'session-hash');
  assert.equal(query.get('caller'), 'caller-hash');
});

test('matrix drilldowns never broaden an unmarked dimension to all requests', () => {
  assert.equal(matrixDrillFilter('project', {project: ''}, 'account-a'), null);
  assert.equal(matrixDrillFilter('caller', {}, 'account-a'), null);
  assert.equal(matrixDrillFilter('session', {session: ''}, 'account-a'), null);
  assert.deepEqual(matrixDrillFilter('project', {project: 'project-a'}, 'account-b'), {project: 'project-a', account: 'account-b'});
});
