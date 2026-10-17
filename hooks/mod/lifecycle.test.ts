import { expect, test } from 'claude-code/testing'

const context = (event: string, text: string) => JSON.stringify({
  hookSpecificOutput: { hookEventName: event, additionalContext: text },
})

test('classic lifecycle events send full hook input and map Go results', async ($, on) => {
  const sent: Record<string, unknown>[] = []
  const paths: string[] = []
  const continued: Record<string, unknown>[] = []
  let maintenanceRan = false
  on('env.get', (_$, e) => ({ value: e.name === 'OS' ? 'Windows_NT' : e.name === 'PROCESSOR_ARCHITECTURE' ? 'AMD64' : undefined }))
  on('process.run', (_$, e) => {
    paths.push(e.argv[0] ?? '')
    expect(e.init.timeoutMs).toBe(60000)
    const input = JSON.parse(e.init.stdin)
    if (input.hook_event_name === 'SessionStart') expect(maintenanceRan).toBe(true)
    sent.push(input)
    const stdout = input.hook_event_name === 'UserPromptSubmit' ? context('UserPromptSubmit', 'prompt context') :
      input.hook_event_name === 'SessionStart' ? context('SessionStart', 'restored context') :
      input.hook_event_name === 'Stop' ? JSON.stringify({ decision: 'block', reason: 'finish notice' }) : ''
    return { value: { exitCode: 0, stdout, stderr: '' } }
  })
  on('classic.UserPromptSubmit', () => ({ additionalContext: ['prior prompt'] }))
  on('classic.SessionStart', (_$, e) => { maintenanceRan = true; continued.push(e); return { additionalContext: ['prior session'] } })
  on('classic.PreCompact', () => ({}))
  on('classic.Stop', () => ({}))

  const prompt = await $.classic.UserPromptSubmit({ session_id: 's1', cwd: '/repo', prompt: 'Continue' })
  const start = await $.classic.SessionStart({ session_id: 's1', cwd: '/repo', source: 'compact' })
  await $.classic.SessionStart({ session_id: 's1', cwd: '/repo', source: 'clear' })
  await $.classic.SessionStart({ session_id: 's1', cwd: '/repo', source: 'resume' })
  const compact = await $.classic.PreCompact({ session_id: 's1', cwd: '/repo', trigger: 'manual', custom_instructions: null })
  const stop = await $.classic.Stop({ session_id: 's1', cwd: '/repo', stop_hook_active: false, last_assistant_message: 'Done.' })

  expect(prompt.additionalContext).toEqual(['prior prompt', 'prompt context'])
  expect(start.additionalContext).toEqual(['prior session', 'restored context'])
  expect(compact).toEqual({})
  expect(stop.block).toBe('finish notice')
  expect(sent.map(e => e.hook_event_name)).toEqual(['UserPromptSubmit', 'SessionStart', 'SessionStart', 'SessionStart', 'PreCompact', 'Stop'])
  expect(sent.filter(e => e.hook_event_name === 'SessionStart').map(e => e.source)).toEqual(['compact', 'clear', 'resume'])
  expect(continued).toEqual([
    { session_id: 's1', transcript_path: '', cwd: '/repo', source: 'compact', hook_event_name: 'SessionStart' },
    { session_id: 's1', transcript_path: '', cwd: '/repo', source: 'clear', hook_event_name: 'SessionStart' },
    { session_id: 's1', transcript_path: '', cwd: '/repo', source: 'resume', hook_event_name: 'SessionStart' },
  ])
  expect(sent[4].trigger).toBe('manual')
  expect(sent[5].last_assistant_message).toBe('Done.')
  expect(paths.every(path => path.replace(/\\/g, '/').endsWith('/bin/purser-hook-windows-amd64.exe'))).toBe(true)
})

test('each lifecycle event passes through Go failures and logs once per session', async ($, on) => {
  const logs: string[] = []
  on('env.get', (_$, e) => ({ value: e.name === 'OS' ? 'Windows_NT' : e.name === 'PROCESSOR_ARCHITECTURE' ? 'AMD64' : undefined }))
  on('process.run', (_$, e) => {
    const event = JSON.parse(e.init.stdin).hook_event_name
    if (event === 'UserPromptSubmit') return { value: { exitCode: 7, stdout: '', stderr: 'failed' } }
    if (event === 'SessionStart') return { deny: 'timed out' }
    if (event === 'PreCompact') return { deny: 'spawn failed' }
    return { value: { exitCode: 0, stdout: '{bad json', stderr: '' } }
  })
  on('ui.log', (_$, e) => { logs.push(e.text); return { value: undefined } })
  on('classic.UserPromptSubmit', () => ({ additionalContext: ['prior prompt'] }))
  on('classic.SessionStart', () => ({ additionalContext: ['prior session'] }))
  on('classic.PreCompact', () => ({}))
  on('classic.Stop', () => ({}))

  expect((await $.classic.UserPromptSubmit({ session_id: 's2', prompt: 'Continue' })).additionalContext).toEqual(['prior prompt'])
  expect((await $.classic.SessionStart({ session_id: 's2', source: 'clear' })).additionalContext).toEqual(['prior session'])
  expect(await $.classic.PreCompact({ session_id: 's2', trigger: 'auto', custom_instructions: null })).toEqual({})
  expect(await $.classic.Stop({ session_id: 's2', stop_hook_active: false })).toEqual({})
  expect(logs.length).toBe(1)
})

test('Codex enforcement Stop block survives the Go Stop result', async ($, on) => {
  on('env.get', (_$, e) => ({ value: e.name === 'OS' ? 'Windows_NT' : e.name === 'PROCESSOR_ARCHITECTURE' ? 'AMD64' : undefined }))
  on('process.run', () => ({ value: { exitCode: 0, stdout: JSON.stringify({ decision: 'block', reason: 'Go context block' }), stderr: '' } }))
  on('classic.Stop', () => ({ block: 'Codex enforcement block' }))
  expect((await $.classic.Stop({ session_id: 's3', stop_hook_active: false })).block).toBe('Codex enforcement block')
})

test('SessionStart systemMessage is toasted without entering additionalContext', async ($, on) => {
  const toasts: string[] = []
  on('env.get', (_$, e) => ({ value: e.name === 'OS' ? 'Windows_NT' : e.name === 'PROCESSOR_ARCHITECTURE' ? 'AMD64' : undefined }))
  on('process.run', () => ({ value: {
    exitCode: 0,
    stdout: JSON.stringify({
      hookSpecificOutput: { hookEventName: 'SessionStart', additionalContext: '' },
      systemMessage: '⚓ Purser 1.3.0 is available',
    }),
    stderr: '',
  } }))
  on('ui.toast', (_$, e) => { toasts.push(e.text); return { value: undefined } })
  on('classic.SessionStart', () => ({ additionalContext: ['prior session'] }))

  const result = await $.classic.SessionStart({ session_id: 's4', cwd: '/repo', source: 'resume' })

  expect(result.additionalContext).toEqual(['prior session'])
  expect(toasts).toEqual(['⚓ Purser 1.3.0 is available'])
})
