'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const {resolveModelAccount} = require('../../../internal/v2/daemon/web/panel.js');

const account = (overrides = {}) => ({
  id: 'codex-account.json', provider: 'codex', name: 'codex-account.json',
  auth_index: 'index-codex', disabled: false, runtime_only: false, ...overrides
});
const directory = (...files) => ({files});
const invalid = (data, ...argumentsList) => {
  const id = argumentsList.length > 0 ? argumentsList[0] : 'codex-account.json';
  const provider = argumentsList.length > 1 ? argumentsList[1] : 'codex';
  assert.throws(() => resolveModelAccount(data, id, provider), error => {
    assert.ok(error instanceof Error);
    assert.equal(error.code, 'model_catalog_account_invalid');
    return true;
  });
};

test('model account resolver is exported as a pure function', () => {
  assert.equal(typeof resolveModelAccount, 'function');
});

test('exact Codex identity resolves to only its public directory binding', () => {
  const selected = account({email: 'fixture@example.invalid', extra: 'ignored'});
  assert.deepEqual(resolveModelAccount(directory(selected), selected.id, 'codex'), {
    id: selected.id, provider: 'codex', index: 'index-codex'
  });
});

test('Antigravity identity resolves without falling back to Codex', () => {
  const selected = account({id: 'antigravity-account.json', name: 'antigravity-account.json', provider: 'antigravity', auth_index: 'index-antigravity'});
  assert.deepEqual(resolveModelAccount(directory(selected), selected.id, 'antigravity'), {
    id: selected.id, provider: 'antigravity', index: 'index-antigravity'
  });
  invalid(directory(selected), selected.id, 'codex');
});

test('omitted optional boolean fields remain compatible with official omitempty fields', () => {
  const selected = account();
  delete selected.disabled;
  delete selected.runtime_only;
  assert.deepEqual(resolveModelAccount(directory(selected), selected.id, 'codex'), {
    id: selected.id, provider: 'codex', index: selected.auth_index
  });
});

test('malformed response containers and file entries fail closed', () => {
  for (const data of [undefined, null, false, 1, 'files', [], {}, {files: null}, {files: {}}, {files: 'files'}]) {
    invalid(data);
  }
  for (const entry of [null, false, 1, 'account', []]) {
    invalid(directory(account(), entry));
  }
});

test('missing and duplicate exact IDs cannot resolve by a name or a position', () => {
  invalid(directory());
  invalid(directory(account({id: 'another-id', name: 'codex-account.json'})));
  invalid(directory(account(), account({name: 'different-name.json', auth_index: 'different-index'})));
  invalid(directory(account(), account({provider: 'antigravity'})));
  invalid(directory(account({id: 'CODEX-ACCOUNT.JSON'})));
  invalid(directory(account({id: ' codex-account.json'})));
});

test('any other entry alias matching the target ID is rejected regardless of provider or status', () => {
  for (const alias of [
    account({id: 'other-id', auth_index: 'other-index'}),
    account({id: 'other-id', auth_index: 'other-index', provider: 'antigravity'}),
    account({id: 'other-id', auth_index: 'other-index', disabled: true}),
    account({id: 'other-id', auth_index: 'other-index', runtime_only: true})
  ]) {
    invalid(directory(alias, account()));
    invalid(directory(account(), alias));
  }
});

test('unrelated names do not prevent an exact target from resolving', () => {
  const selected = account();
  const unrelated = account({id: 'other-id', name: 'other-name.json', auth_index: 'other-index', provider: 'antigravity'});
  assert.deepEqual(resolveModelAccount(directory(unrelated, selected), selected.id, 'codex'), {
    id: selected.id, provider: 'codex', index: selected.auth_index
  });
});

test('only explicit matching Codex and Antigravity providers are accepted', () => {
  for (const provider of [undefined, null, '', 'openai', 'Codex', 'codex ', 1, {}, []]) {
    invalid(directory(account()), 'codex-account.json', provider);
    invalid(directory(account({provider})));
  }
  invalid(directory(account({provider: 'antigravity'})));
  const typeOnly = account({type: 'codex'});
  delete typeOnly.provider;
  invalid(directory(typeOnly));
});

test('disabled and runtime-only targets are rejected with strict boolean field validation', () => {
  for (const field of ['disabled', 'runtime_only']) {
    for (const value of [true, 'true', 'false', '', 0, 1, null, {}, []]) {
      invalid(directory(account({[field]: value})));
    }
  }
});

test('an auth index must be a nonempty string, never a guessed ID or filename', () => {
  for (const auth_index of [undefined, null, '', ' ', '\t', 0, 1, false, {}, []]) {
    invalid(directory(account({auth_index})));
  }
  const selected = account();
  delete selected.auth_index;
  invalid(directory(selected));
});

test('invalid requested IDs and matching malformed record IDs are rejected', () => {
  for (const id of [undefined, null, '', false, 1, {}, [], 'a\u0000b', 'a\nb', 'a\rb', 'a\tb', 'a\u001fb', 'a\u007fb', 'a'.repeat(257)]) {
    invalid(directory(account()), id);
    invalid(directory(account({id, name: 'fixture.json'})), id);
  }
});

test('the ID limit counts UTF-8 bytes rather than JavaScript character length', () => {
  for (const id of ['a'.repeat(256), '界'.repeat(85) + 'a', '😀'.repeat(64)]) {
    assert.equal(Buffer.byteLength(id, 'utf8'), 256);
    assert.deepEqual(resolveModelAccount(directory(account({id, name: 'fixture.json'})), id, 'codex'), {
      id, provider: 'codex', index: 'index-codex'
    });
  }
  for (const id of ['界'.repeat(86), '😀'.repeat(64) + 'a']) {
    invalid(directory(account({id, name: 'fixture.json'})), id);
  }
});

test('a directory of exactly 10000 files is accepted but a larger one is rejected', () => {
  const files = Array.from({length: 9999}, (unused, index) => account({
    id: 'other-' + index, name: 'other-' + index + '.json', auth_index: 'index-' + index
  }));
  files.push(account());
  assert.deepEqual(resolveModelAccount({files}, 'codex-account.json', 'codex'), {
    id: 'codex-account.json', provider: 'codex', index: 'index-codex'
  });
  invalid({files: [...files, account({id: 'overflow', name: 'overflow.json', auth_index: 'overflow-index'})]});
});

test('resolution does not mutate the caller response or its file records', () => {
  const selected = Object.freeze(account());
  const files = Object.freeze([selected]);
  const data = Object.freeze({files});
  assert.deepEqual(resolveModelAccount(data, selected.id, selected.provider), {
    id: selected.id, provider: selected.provider, index: selected.auth_index
  });
});
