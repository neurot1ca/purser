#!/usr/bin/env node
'use strict';

const fs = require('node:fs');
const path = require('node:path');
const { spawnSync } = require('node:child_process');

function goBinaryPath() {
  const platforms = { win32: 'windows', linux: 'linux', darwin: 'darwin' };
  const architectures = { x64: 'amd64', arm64: 'arm64' };
  const goos = platforms[process.platform];
  const goarch = architectures[process.arch];
  if (!goos || !goarch) return null;
  return path.join(__dirname, '..', 'bin', `purser-hook-${goos}-${goarch}${goos === 'windows' ? '.exe' : ''}`);
}

function main(args, run = spawnSync, replace = process.execve) {
  if (args[0] !== 'go-mcp') {
    process.stderr.write('Purser: invalid launcher invocation\n');
    return 1;
  }

  const binary = goBinaryPath();
  const expected = binary || path.join(__dirname, '..', 'bin', `purser-hook-${process.platform}-${process.arch}`);
  let reason = !binary ? 'unsupported platform' : !fs.existsSync(binary) ? 'Go binary missing' : null;
  if (!reason) {
    const childArgs = ['mcp', ...args.slice(1)];
    const env = process.env;
    if (process.platform !== 'win32' && typeof replace === 'function') {
      try {
        replace(binary, [binary, ...childArgs], env);
        return 1; // A successful exec never returns.
      } catch (error) {
        if (error.code === 'EACCES') {
          try {
            fs.chmodSync(binary, 0o755);
            replace(binary, [binary, ...childArgs], env);
            return 1;
          } catch (retryError) {
            error = retryError;
          }
        }
        // Unsupported runtimes and exec failures retain the hidden fallback.
        process.stderr.write(`Purser: exec replacement unavailable: ${error.message}\n`);
      }
    }
    let child = run(binary, childArgs, { stdio: 'inherit', windowsHide: true, env });
    if (process.platform !== 'win32' && child.error && child.error.code === 'EACCES') {
      try {
        fs.chmodSync(binary, 0o755);
        child = run(binary, childArgs, { stdio: 'inherit', windowsHide: true, env });
      } catch (error) {
        child = { error };
      }
    }
    if (!child.error || child.pid) return child.status === null ? 1 : child.status;
    reason = `Go process creation failed: ${child.error.message}`;
  }
  process.stderr.write(`Purser: Go MCP unavailable on ${process.platform}/${process.arch}; expected binary ${expected} (${reason})\n`);
  return 1;
}

if (require.main === module) process.exit(main(process.argv.slice(2)));
module.exports = { goBinaryPath, main };
