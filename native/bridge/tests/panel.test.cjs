'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const {ViewTracker} = require('../../../internal/v2/daemon/web/panel.js');
const view = (instance, revision) => ({instance_id: instance, view_revision: revision});

test('a slow earlier read cannot overwrite a later read or an accepted write', () => {
  const state = new ViewTracker();
  const early = state.begin('accounts'), late = state.begin('accounts');
  assert.equal(state.accept('accounts', late, view('a', 20)), true);
  assert.equal(state.accept('accounts', early, view('a', 10)), false);
  const pending = state.begin('accounts'); state.barrier();
  assert.equal(state.accept('accounts', pending, view('a', 21)), false);
});
test('retired instances and older unknown responses cannot roll back a restart', () => {
  const state = new ViewTracker();
  assert.equal(state.accept('overview', state.begin('overview'), view('a', 30)), true);
  const old = state.begin('accounts');
  assert.equal(state.accept('overview', state.begin('overview'), view('b', 1)), true);
  assert.equal(state.accept('accounts', old, view('a', 40)), false);
  assert.equal(state.accept('accounts', old, view('unseen-older-instance', 90)), false);
  assert.equal(state.accept('accounts', state.begin('accounts'), view('b', 2)), true);
});
test('versions are monotonic per resource and independent across resources', () => {
  const state = new ViewTracker();
  assert.equal(state.accept('job:x', state.begin('job:x'), view('a', 50)), true);
  assert.equal(state.accept('accounts', state.begin('accounts'), view('a', 40)), true);
  assert.equal(state.accept('accounts', state.begin('accounts'), view('a', 39)), false);
  assert.equal(state.accept('accounts', state.begin('accounts'), view('a', NaN)), false);
});

function panel(respond) {
  class Element {
    constructor() { this.children = []; this.value = ''; this.disabled = false; this.hidden = false; this.textContent = ''; }
    replaceChildren(...children) { this.children = children; }
    append(...children) { this.children.push(...children); }
  }
  const elements = new Map(), timers = new Map(), calls = [];
  let nextTimer = 0;
  const get = id => { if (!elements.has(id)) elements.set(id, new Element()); return elements.get(id); };
  const context = vm.createContext({
    document: {getElementById: get, createElement: () => new Element()},
    fetch: async url => { calls.push(url); const [status, body] = respond(url); return {ok: status < 400, status, json: async () => body}; },
    AbortSignal, crypto: {randomUUID: () => 'fixture-key'},
    setTimeout: (fn, delay) => { timers.set(++nextTimer, {fn, delay}); return nextTimer; },
    clearTimeout: id => timers.delete(id),
  });
  vm.runInContext(fs.readFileSync(path.resolve(__dirname, '../../../internal/v2/daemon/web/panel.js'), 'utf8'), context);
  return {get, timers, calls};
}

test('account paging sends its version and reloads from the first page after a conflict', async () => {
  let reads = 0;
  const ui = panel(url => {
    if (url.endsWith('/overview')) return [200, {...view('a', 10), read_only: true, query_concurrency: 8}];
    if (url.includes('after=')) return [409, {error: 'view_changed'}];
    reads++;
    return [200, {...view('a', 10 + reads), accounts_revision: reads === 1 ? 5 : 6,
      accounts: [{id: reads === 1 ? 'old-first' : 'new-first', status: 'available'}], next_after: reads === 1 ? 'cursor-a' : ''}];
  });
  await ui.get('connect').onclick();
  await ui.get('more').onclick();
  assert.equal(ui.calls.some(url => url.endsWith('/accounts?after=cursor-a&revision=5')), true);
  assert.equal(reads, 2);
  assert.equal(ui.get('accounts').children.length, 1);
  assert.match(ui.get('accounts').children[0].children[0].textContent, /new-first/);
  assert.equal(ui.get('more').hidden, true);
});

test('activity refresh keeps polling and stops after CPA authentication expires', async () => {
  let overviews = 0;
  const ui = panel(url => {
    if (url.endsWith('/overview')) {
      overviews++;
      if (overviews > 1) return [401, {error: 'unauthorized'}];
      return [200, {...view('a', 1), read_only: true, query_concurrency: 8,
        activity: {connected: true, status: 'observing', observed_recent_users: 2, observed_active_requests: 1, unknown_requests: 0}}];
    }
    return [200, {...view('a', 1), accounts_revision: 0, accounts: [], next_after: ''}];
  });
  await ui.get('connect').onclick();
  assert.match(ui.get('activity').textContent, /在途 1/);
  const [id, timer] = [...ui.timers.entries()].find(([, timer]) => timer.delay === 5000);
  ui.timers.delete(id); await timer.fn();
  assert.match(ui.get('connection').textContent, /认证失效/);
  assert.equal(ui.get('query').disabled, true);
  assert.equal(ui.timers.size, 0);
});

test('background quota changes reload the account view after the overview revision advances', async () => {
  let overviews = 0, accounts = 0;
  const ui = panel(url => {
    if (url.endsWith('/overview')) {
      overviews++;
      return [200, {...view('a', overviews), read_only: true, query_concurrency: 8, accounts_revision: overviews}];
    }
    accounts++;
    return [200, {...view('a', accounts), accounts_revision: accounts, accounts: [{id: accounts === 1 ? 'initial' : 'updated', status: 'available'}], next_after: ''}];
  });
  await ui.get('connect').onclick();
  const [id, timer] = [...ui.timers.entries()].find(([, timer]) => timer.delay === 5000);
  ui.timers.delete(id); await timer.fn();
  assert.equal(accounts, 2);
  assert.match(ui.get('accounts').children[0].children[0].textContent, /updated/);
});
