import { expect, test } from 'claude-code/testing'
import { buildReadEvent, platformBinary } from './read.js'

const read = { tool: 'Read', file_path: 'notes.md', tool_use_id: 'tool-1' }

test('Read helper builds the settings-hook event and platform binary name', () => {
  expect(platformBinary('windows', 'amd64')).toBe('purser-hook-windows-amd64.exe')
  expect(platformBinary('linux', 'arm64')).toBe('purser-hook-linux-arm64')
  expect(buildReadEvent({ ...read, transcript_path: 'session.jsonl' }, 's1', '/repo')).toEqual({
    session_id: 's1', cwd: '/repo', transcript_path: 'session.jsonl', tool_name: 'Read',
    tool_input: { file_path: 'notes.md' }, hook_event_name: 'PreToolUse',
  })
})

test('Read passes through when Go succeeds', async ($, on) => {
  let argv: readonly string[] = []
  let stdin = ''
  let timeoutMs = 0
  let called: () => void = () => {}
  const invoked = new Promise<void>(resolve => { called = resolve })
  on('session.id', () => ({ value: 'session-1' }))
  on('session.cwd', () => ({ value: '/repo' }))
  on('env.get', (_$, e) => ({ value: e.name === 'OS' ? 'Windows_NT' : e.name === 'PROCESSOR_ARCHITECTURE' ? 'AMD64' : undefined }))
  on('process.run', (_$, e) => {
    argv = e.argv
    stdin = e.init.stdin
    timeoutMs = e.init.timeoutMs
    called()
    return { value: { exitCode: 0, stdout: '', stderr: '' } }
  })
  on('tool.call', () => ({ result: 'Read result' }))
  const result = await $.tool.call(read)
  await invoked
  expect(result.result).toBe('Read result')
  expect((argv[0] ?? '').replace(/\\/g, '/').endsWith('/bin/purser-hook-windows-amd64.exe')).toBe(true)
  expect(timeoutMs).toBe(5000)
  expect(JSON.parse(stdin).hook_event_name).toBe('PreToolUse')
})

test('Go completes before the Read handler runs', async ($, on) => {
  let completeGo: () => void = () => {}
  let goStarted: () => void = () => {}
  const started = new Promise<void>(resolve => { goStarted = resolve })
  let goCompleted = false
  let readStarted = false
  on('session.id', () => ({ value: 'session-1' }))
  on('session.cwd', () => ({ value: '/repo' }))
  on('env.get', (_$, e) => ({ value: e.name === 'OS' ? 'Windows_NT' : e.name === 'PROCESSOR_ARCHITECTURE' ? 'AMD64' : undefined }))
  on('process.run', () => new Promise(resolve => {
    completeGo = () => {
      goCompleted = true
      resolve({ value: { exitCode: 0, stdout: '', stderr: '' } })
    }
    goStarted()
  }))
  on('tool.call', () => {
    readStarted = true
    expect(goCompleted).toBe(true)
    return { result: 'Read result' }
  })
  const call = $.tool.call(read)
  await started
  expect(readStarted).toBe(false)
  completeGo()
  const result = await call
  expect(readStarted).toBe(true)
  expect(result.result).toBe('Read result')
})

test('Go failure is logged and Read passes through', async ($, on) => {
  const logs: string[] = []
  let runs = 0
  let envReads = 0
  let secondRun: () => void = () => {}
  const ranTwice = new Promise<void>(resolve => { secondRun = resolve })
  let logged: () => void = () => {}
  const reported = new Promise<void>(resolve => { logged = resolve })
  on('session.id', () => ({ value: 'session-1' }))
  on('session.cwd', () => ({ value: '/repo' }))
  on('env.get', (_$, e) => { envReads++; return { value: e.name === 'OS' ? 'Windows_NT' : e.name === 'PROCESSOR_ARCHITECTURE' ? 'AMD64' : undefined } })
  on('process.run', () => {
    if (++runs === 2) secondRun()
    return { value: { exitCode: 1, stdout: '', stderr: 'failure' } }
  })
  on('ui.log', (_$, e) => { logs.push(e.text); logged(); return { value: undefined } })
  on('tool.call', () => ({ result: 'Read result' }))
  const result = await $.tool.call(read)
  await reported
  await $.tool.call(read)
  await ranTwice
  expect(result.result).toBe('Read result')
  expect(logs.length).toBe(1)
  expect(envReads).toBe(4)
})

test('process timeout is swallowed and Read passes through', async ($, on) => {
  let logged: () => void = () => {}
  const reported = new Promise<void>(resolve => { logged = resolve })
  on('session.id', () => ({ value: 'session-1' }))
  on('session.cwd', () => ({ value: '/repo' }))
  on('env.get', (_$, e) => ({ value: e.name === 'OS' ? 'Windows_NT' : e.name === 'PROCESSOR_ARCHITECTURE' ? 'AMD64' : undefined }))
  on('process.run', () => ({ deny: 'timed out' }))
  on('ui.log', () => { logged(); return { value: undefined } })
  on('tool.call', () => ({ result: 'Read result' }))
  const result = await $.tool.call(read)
  await reported
  expect(result.result).toBe('Read result')
})

test('non-Read tools do not start Go', async ($, on) => {
  let processCalls = 0
  on('process.run', () => { processCalls++; return { value: { exitCode: 0, stdout: '', stderr: '' } } })
  on('tool.call', () => ({ result: 'Bash result' }))
  const result = await $.tool.call({ tool: 'Bash', command: 'echo ok' })
  expect(result.result).toBe('Bash result')
  expect(processCalls).toBe(0)
})
