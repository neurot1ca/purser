import { expect, test } from 'claude-code/testing'
import { register as registerEnforcement } from './enforcement.js'

const prompt = 'ORCH|task=t1|phase=implementation|route=normal'
const success = JSON.stringify({
  hookSpecificOutput: {
    hookEventName: 'PreToolUse', updatedInput: { prompt: 'routed prompt', model: 'gpt-6-sol' },
  },
  systemMessage: 'Purser: route=normal -> gpt-6-sol / medium',
})
const bypassContext = 'Orchestrator routing was NOT enforced for this Codex call because of an internal error: fixture failure. The call is allowed unmodified. The bypass was recorded for PostToolUse.'
const options = { plugins: [
  {
    name: 'p66-enforcement-observer', tier: 'prepend' as const,
    register(on) {
      on('classic.PreToolUse', async ($, e, next) => {
        const result = await next(e)
        await $.ui.toast(`p66-result:${JSON.stringify(result)}`)
        return result
      })
    },
  },
  { name: 'p66-enforcement', register: registerEnforcement },
] }

function stubGo(
  on, stdout: string, calls: string[], toasts: string[],
  env = { OS: 'Windows_NT', PROCESSOR_ARCHITECTURE: 'AMD64' },
  uname = { exitCode: 1, stdout: '' },
  binary = '/bin/purser-hook-windows-amd64.exe',
  outcome?: () => { deny?: string; exitCode?: number; stdout?: string } | undefined,
) {
  on('env.get', (_$, e) => ({ value: env[e.name] }))
  on('session.id', () => ({ value: 'p66-session' }))
  on('session.cwd', () => ({ value: 'C:/p66-repo' }))
  on('process.run', (_$, e) => {
    if (e.argv[0] === 'uname') {
      expect(e.argv).toEqual(['uname', '-s', '-m'])
      expect(e.init.timeoutMs).toBe(1000)
      return { value: { ...uname, stderr: '' } }
    }
    calls.push(e.init.stdin)
    expect(e.argv[0].replace(/\\/g, '/').endsWith(binary)).toBe(true)
    expect(e.init.timeoutMs).toBe(60000)
    const result = outcome?.()
    if (result?.deny) return { deny: result.deny }
    return { value: { exitCode: result?.exitCode ?? 0, stdout: result?.stdout ?? stdout, stderr: '' } }
  })
  on('ui.toast', (_$, e) => { toasts.push(e.text); return { value: undefined } })
}

function stubToolCall(on, calls: unknown[]) {
  on('tool.call', (_$, e) => { calls.push(e); return { result: JSON.stringify(e) } })
}

function observed(toasts: string[]) {
  const result = toasts.find(line => line.startsWith('p66-result:'))
  expect(result).toBeDefined()
  return JSON.parse(result!.slice('p66-result:'.length))
}

test('matches both Codex tool identities and continues once per call', options, async ($, on) => {
  const calls: string[] = []
  const toolCalls: unknown[] = []
  const toasts: string[] = []
  let continuations = 0
  stubGo(on, JSON.stringify({ hookSpecificOutput: { hookEventName: 'PreToolUse', updatedInput: { prompt: 'changed' } } }), calls, toasts)
  on('classic.PreToolUse', () => { continuations++; return {} })
  stubToolCall(on, toolCalls)

  for (const tool of ['mcp__plugin_purser_codex-main__codex', 'mcp__plugin_purser_codex-main__codex-reply']) {
    await $.tool.call({ tool, prompt })
  }

  expect(calls).toHaveLength(2)
  expect(continuations).toBe(2)
  expect(toolCalls.map(call => call.prompt)).toEqual(['changed', 'changed'])
  expect(calls.map(stdin => JSON.parse(stdin).tool_name)).toEqual([
    'mcp__plugin_purser_codex-main__codex', 'mcp__plugin_purser_codex-main__codex-reply',
  ])
})

test('passes non-Codex tool identities through without starting Go', options, async ($, on) => {
  const calls: string[] = []
  const toolCalls: unknown[] = []
  let continuations = 0
  on('process.run', () => { calls.push('unexpected'); return { value: { exitCode: 0, stdout: '', stderr: '' } } })
  on('classic.PreToolUse', () => { continuations++; return {} })
  stubToolCall(on, toolCalls)

  await $.tool.call({ tool: 'mcp__other-server__query', query: 'hello' })

  expect(calls).toEqual([])
  expect(continuations).toBe(1)
  expect(toolCalls).toHaveLength(1)
})

test('maps the Python deny reason', options, async ($, on) => {
  const calls: string[] = []
  const toolCalls: unknown[] = []
  const toasts: string[] = []
  let continuations = 0
  stubGo(on, JSON.stringify({ hookSpecificOutput: {
    hookEventName: 'PreToolUse', permissionDecision: 'deny', permissionDecisionReason: 'MALFORMED_TOOL_INPUT: tool_input must be an object',
  } }), calls, toasts)
  on('classic.PreToolUse', () => { continuations++; return {} })
  stubToolCall(on, toolCalls)

  const result = await $.tool.call({ tool: 'mcp__plugin_purser_codex-main__codex-reply', prompt })

  expect(observed(toasts)).toEqual({ deny: 'MALFORMED_TOOL_INPUT: tool_input must be an object' })
  expect(result.isError).toBe(true)
  expect(result.text).toBe('MALFORMED_TOOL_INPUT: tool_input must be an object')
  expect(continuations).toBe(1)
  expect(toolCalls).toEqual([])
})

test('maps rewritten input and the visible route line', options, async ($, on) => {
  const calls: string[] = []
  const toolCalls: unknown[] = []
  const toasts: string[] = []
  let continuations = 0
  stubGo(on, success, calls, toasts)
  on('classic.PreToolUse', () => { continuations++; return {} })
  stubToolCall(on, toolCalls)

  await $.tool.call({ tool: 'mcp__plugin_purser_codex-main__codex', prompt })

  expect(observed(toasts)).toEqual({
    updatedInput: { prompt: 'routed prompt', model: 'gpt-6-sol' },
  })
  expect(JSON.parse(calls[0])).toMatchObject({
    session_id: 'p66-session', cwd: 'C:/p66-repo', hook_event_name: 'PreToolUse',
    tool_name: 'mcp__plugin_purser_codex-main__codex', tool_input: { prompt },
  })
  expect(toolCalls[0]).toMatchObject({ prompt: 'routed prompt', model: 'gpt-6-sol' })
  expect(toasts).toContain('Purser: route=normal -> gpt-6-sol / medium')
  expect(continuations).toBe(1)
})

test('maps Python additional context and preserves context from another mod', options, async ($, on) => {
  const calls: string[] = []
  const toolCalls: unknown[] = []
  const toasts: string[] = []
  let continuations = 0
  stubGo(on, JSON.stringify({ hookSpecificOutput: {
    hookEventName: 'PreToolUse', additionalContext: bypassContext,
  } }), calls, toasts)
  on('classic.PreToolUse', () => { continuations++; return { additionalContext: ['prior context'] } })
  stubToolCall(on, toolCalls)

  await $.tool.call({ tool: 'mcp__plugin_purser_codex-main__codex', prompt })

  expect(observed(toasts)).toEqual({ additionalContext: ['prior context', bypassContext] })
  expect(toolCalls).toHaveLength(1)
  expect(continuations).toBe(1)
})

test('preserves a prior block from another mod and continues once', options, async ($, on) => {
  const calls: string[] = []
  const toolCalls: unknown[] = []
  const toasts: string[] = []
  let continuations = 0
  stubGo(on, success, calls, toasts)
  on('classic.PreToolUse', () => { continuations++; return { deny: 'blocked by another mod' } })
  stubToolCall(on, toolCalls)

  const result = await $.tool.call({ tool: 'mcp__plugin_purser_codex-main__codex', prompt })

  expect(observed(toasts)).toEqual({ deny: 'blocked by another mod' })
  expect(result.isError).toBe(true)
  expect(result.text).toBe('blocked by another mod')
  expect(calls).toEqual([])
  expect(toolCalls).toEqual([])
  expect(continuations).toBe(1)
})

test('detects Linux amd64 with uname when host environment variables are unset', options, async ($, on) => {
  const calls: string[] = []
  const toolCalls: unknown[] = []
  const toasts: string[] = []
  stubGo(on, success, calls, toasts, {}, { exitCode: 0, stdout: 'Linux x86_64' }, '/bin/purser-hook-linux-amd64')
  stubToolCall(on, toolCalls)

  await $.tool.call({ tool: 'mcp__plugin_purser_codex-main__codex', prompt })

  expect(calls).toHaveLength(1)
  expect(observed(toasts)).toEqual({ updatedInput: { prompt: 'routed prompt', model: 'gpt-6-sol' } })
  expect(toolCalls[0]).toMatchObject({ prompt: 'routed prompt', model: 'gpt-6-sol' })
})

test('detects Darwin arm64 with uname when host environment variables are unset', options, async ($, on) => {
  const calls: string[] = []
  const toolCalls: unknown[] = []
  const toasts: string[] = []
  stubGo(on, success, calls, toasts, {}, { exitCode: 0, stdout: 'Darwin arm64' }, '/bin/purser-hook-darwin-arm64')
  stubToolCall(on, toolCalls)

  await $.tool.call({ tool: 'mcp__plugin_purser_codex-main__codex', prompt })

  expect(calls).toHaveLength(1)
  expect(observed(toasts)).toEqual({ updatedInput: { prompt: 'routed prompt', model: 'gpt-6-sol' } })
  expect(toolCalls[0]).toMatchObject({ prompt: 'routed prompt', model: 'gpt-6-sol' })
})

test('fails open when uname platform detection fails', options, async ($, on) => {
  const calls: string[] = []
  const toolCalls: unknown[] = []
  const toasts: string[] = []
  stubGo(on, success, calls, toasts, {}, { exitCode: 1, stdout: '' })
  stubToolCall(on, toolCalls)

  await $.tool.call({ tool: 'mcp__plugin_purser_codex-main__codex', prompt })

  expect(calls).toEqual([])
  expect(observed(toasts)).toEqual({
    additionalContext: ['Orchestrator routing was NOT enforced for this Codex call because Go PreToolUse failed: platform detection failed. The call is allowed unmodified; FULL Claude review is required.'],
  })
  expect(toolCalls).toHaveLength(1)
})

test('adapter failures allow sync Codex with FULL context and deny codex_start', options, async ($, on) => {
  const failures = [
    { name: 'spawn failure', outcome: { deny: 'spawn failed' }, message: 'spawn failed' },
    { name: 'nonzero exit', outcome: { exitCode: 1 }, message: 'exited 1' },
    { name: 'timeout', outcome: { deny: 'timed out' }, message: 'timed out' },
    { name: 'malformed stdout', outcome: { stdout: '{' }, message: 'JSON Parse error' },
    { name: 'interruption after pending write', outcome: { deny: 'process killed after pending admission' }, message: 'process killed after pending admission' },
  ]
  const calls: string[] = []
  const toolCalls: unknown[] = []
  const toasts: string[] = []
  let currentFailure = failures[0]
  stubGo(on, success, calls, toasts, undefined, undefined, undefined, () => currentFailure.outcome)
  stubToolCall(on, toolCalls)

  for (const failure of failures) {
    currentFailure = failure
    for (const tool of ['mcp__plugin_purser_codex-main__codex', 'mcp__plugin_purser_codex-main__codex_start']) {
      toasts.length = 0
      const before = toolCalls.length
      const result = await $.tool.call({ tool, prompt })
      const mapped = observed(toasts)
      if (tool.endsWith('__codex_start')) {
        expect(mapped.deny).toContain('ASYNC_ENFORCEMENT_FAILED')
        expect(mapped.deny).toContain(failure.message)
        expect(result.isError).toBe(true)
        expect(toolCalls).toHaveLength(before)
      } else {
        expect(mapped.additionalContext.join(' ')).toContain(failure.message)
        expect(mapped.additionalContext.join(' ')).toContain('FULL Claude review is required')
        expect(result.isError).not.toBe(true)
        expect(toolCalls).toHaveLength(before + 1)
        expect(toolCalls.at(-1)).toMatchObject({ prompt })
      }
    }
  }
  expect(calls).toHaveLength(failures.length * 2)
})
