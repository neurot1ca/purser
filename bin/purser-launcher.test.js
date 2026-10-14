'use strict';
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const { main, goBinaryPath } = require('./purser-launcher');

function fixture(platform, body) {
  const original = Object.getOwnPropertyDescriptor(process, 'platform');
  const exists = fs.existsSync;
  Object.defineProperty(process, 'platform', { value: platform });
  fs.existsSync = () => true;
  try { body(); } finally { fs.existsSync = exists; Object.defineProperty(process, 'platform', original); }
}

test('Unix exec preserves argv and environment without spawning', () => fixture('linux', () => {
  let called = false;
  assert.equal(main(['go-mcp', '--codex-bin', 'with spaces'], () => { assert.fail('spawned'); }, (binary, args, env) => {
    called = true;
    assert.equal(binary, goBinaryPath());
    assert.deepEqual(args, [binary, 'mcp', '--codex-bin', 'with spaces']);
    assert.equal(env, process.env);
    assert.equal(env.PATH, process.env.PATH);
  }), 1);
  assert.ok(called);
}));

test('Windows keeps hidden inherited stdio and child status', () => fixture('win32', () => {
  assert.equal(main(['go-mcp', 'arg'], (binary, args, options) => {
    assert.deepEqual(args, ['mcp', 'arg']);
    assert.equal(options.stdio, 'inherit');
    assert.equal(options.windowsHide, true);
    assert.equal(options.env, process.env);
    return { status: 7, pid: 123 };
  }, () => assert.fail('Windows exec')), 7);
}));

test('Unix permission repair retries exec before spawn', () => fixture('linux', () => {
  const chmod = fs.chmodSync;
  let calls = 0;
  fs.chmodSync = (binary, mode) => assert.equal(mode, 0o755);
  try {
    assert.equal(main(['go-mcp'], () => assert.fail('spawned'), () => {
      if (++calls === 1) throw Object.assign(new Error('permission'), { code: 'EACCES' });
    }), 1);
    assert.equal(calls, 2);
  } finally { fs.chmodSync = chmod; }
}));

test('unsupported exec uses fallback and preserves signal failure', () => fixture('darwin', () => {
  assert.equal(main(['go-mcp'], () => ({ status: null, pid: 123 }), () => { throw new Error('unsupported'); }), 1);
}));

test('older Unix runtime retries executable permissions', () => fixture('linux', () => {
  const chmod = fs.chmodSync;
  let calls = 0;
  fs.chmodSync = (binary, mode) => assert.equal(mode, 0o755);
  try {
    assert.equal(main(['go-mcp'], () => ++calls === 1 ? { error: { code: 'EACCES' } } : { pid: 123, status: 0 }, null), 0);
    assert.equal(calls, 2);
  } finally { fs.chmodSync = chmod; }
}));
