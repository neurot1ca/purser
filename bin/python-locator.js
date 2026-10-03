#!/usr/bin/env node
// Python selection must happen before importing any Purser Python code.
'use strict';

const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { spawnSync } = require('node:child_process');

const MESSAGE = 'Purser: no Python 3.11+ found; run /purser:check';
const VERSION = 'import json,sys; print(json.dumps({"path":sys.executable,"version":list(sys.version_info[:3])}))';

function candidates() {
  const result = [];
  if (process.env.PURSER_PYTHON) result.push({ command: process.env.PURSER_PYTHON, prefix: [], source: 'PURSER_PYTHON' });
  try {
    const stored = fs.readFileSync(path.join(os.homedir(), '.orchestrator', 'python'), 'utf8').trim();
    if (stored && !stored.includes('\n') && !stored.includes('\r')) result.push({ command: stored, prefix: [], source: '~/.orchestrator/python' });
  } catch (_) { /* no stored interpreter */ }
  if (process.platform === 'win32') result.push({ command: 'py', prefix: ['-3'], source: 'py -3' });
  result.push({ command: 'python3', prefix: [], source: 'python3' });
  result.push({ command: 'python', prefix: [], source: 'python' });
  return result;
}

function resolvePython(list = candidates(), run = spawnSync) {
  for (const candidate of list) {
    const check = run(candidate.command, [...candidate.prefix, '-c', VERSION], {
      encoding: 'utf8', timeout: 5000, windowsHide: true,
    });
    if (check.error || check.status !== 0) continue;
    try {
      const info = JSON.parse(check.stdout.trim());
      if (Array.isArray(info.version) && (info.version[0] > 3 || (info.version[0] === 3 && info.version[1] >= 11))) {
        return { ...candidate, path: info.path, version: info.version.join('.') };
      }
    } catch (_) { /* invalid probe output */ }
  }
  return null;
}

function goBinaryPath() {
  const platforms = { win32: 'windows', linux: 'linux', darwin: 'darwin' };
  const architectures = { x64: 'amd64', arm64: 'arm64' };
  const goos = platforms[process.platform];
  const goarch = architectures[process.arch];
  if (!goos || !goarch) return null;
  return path.join(__dirname, '..', 'bin', `purser-hook-${goos}-${goarch}${goos === 'windows' ? '.exe' : ''}`);
}

function main(args, resolve = resolvePython, run = spawnSync) {
  if (args[0] === '--resolve-json') {
    const selected = resolve();
    process.stdout.write(JSON.stringify(selected) + '\n');
    return selected ? 0 : 1;
  }
  let mode = args.shift();
  if (!['pre-codex', 'hook', 'go-hook', 'mcp'].includes(mode) || !args.length) {
    process.stderr.write('Purser: invalid Python locator invocation\n');
    return mode === 'pre-codex' ? 2 : 1;
  }
  if (mode === 'go-hook') {
    const binary = goBinaryPath();
    if (binary) {
      const child = run(binary, [], { stdio: 'inherit', windowsHide: true });
      if (!child.error) return child.status === null ? 1 : child.status;
    }
    mode = 'hook';
  }
  const selected = resolve();
  if (!selected) {
    process.stderr.write(MESSAGE + '\n');
    return mode === 'pre-codex' ? 2 : mode === 'hook' ? 0 : 1;
  }
  const child = run(selected.path, args, {
    stdio: 'inherit', windowsHide: true, env: { ...process.env, PYTHONUTF8: '1' },
  });
  if (child.error) {
    process.stderr.write(`Purser: Python launch failed: ${child.error.message}\n`);
    return mode === 'pre-codex' ? 2 : mode === 'hook' ? 0 : 1;
  }
  return child.status === null ? 1 : child.status;
}

if (require.main === module) process.exit(main(process.argv.slice(2)));
module.exports = { candidates, resolvePython, goBinaryPath, main };
