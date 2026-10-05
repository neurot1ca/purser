import { expect, test } from 'claude-code/testing'

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
] }

function stubGo(
  on, stdout: string, calls: string[], toasts: string[],
  env: Record<string, string> = { OS: 'Windows_NT', PROCESSOR_ARCHITECTURE: 'AMD64' },
  uname = { exitCode: 1, stdout: '' },
  binary = '/bin/purser-hook-windows-amd64.exe',
  outcome?: () => { deny?: string; exitCode?: number; stdout?: string } | undefined,
  markerWrites: string[] = [],
  markerOutcome?: () => { deny?: string; exitCode?: number; stdout?: string } | undefined,
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
    expect(e.argv[0].replace(/\\/g, '/').endsWith(binary)).toBe(true)
    const request = JSON.parse(e.init.stdin)
    if (request.hook_event_name === 'PostToolUse' || request.hook_event_name === 'PostToolUseFailure') {
      expect(e.argv[1]).toBe('enforce')
      expect(e.init.timeoutMs).toBe(300000)
    } else if (request.hook_event_name === 'WriteUndeliveredMarker') {
      expect(e.init.timeoutMs).toBe(1000)
    } else {
      expect(e.init.timeoutMs).toBe(60000)
    }
    if (request.hook_event_name === 'WriteUndeliveredMarker') {
      markerWrites.push(e.init.stdin)
      expect(e.init.timeoutMs).toBe(1000)
      const result = markerOutcome?.()
      if (result?.deny) return { deny: result.deny }
      return { value: { exitCode: result?.exitCode ?? 0, stdout: result?.stdout ?? '{"written":true}', stderr: '' } }
    }
    calls.push(e.init.stdin)
    const result = outcome?.()
    if (result?.deny) return { deny: result.deny }
    return { value: { exitCode: result?.exitCode ?? 0, stdout: result?.stdout ?? stdout, stderr: '' } }
  })
  on('ui.toast', (_$, e) => { toasts.push(e.text); return { value: undefined } })
}

function stubToolCall(on, calls: unknown[]) {
  on('tool.call', (_$, e) => { calls.push(e); return { result: JSON.stringify(e) } })
}

const postOutput = JSON.stringify({
  hookSpecificOutput: {
    hookEventName: 'PostToolUse', updatedToolOutput: [{ type: 'text', text: 'reviewed' }],
    additionalContext: 'review required',
  },
  systemMessage: 'Purser post review complete',
})

function postEvent(hook_event_name: 'PostToolUse' | 'PostToolUseFailure') {
  return {
    hook_event_name, session_id: 'p66-session', cwd: 'C:/p66-repo',
    tool_name: 'mcp__plugin_purser_codex-main__codex', tool_use_id: 'post-call',
    tool_response: { content: 'done' },
  }
}

test('maps Go Post outputs for both hook events', options, async ($, on) => {
  const calls: string[] = []
  const toasts: string[] = []
  const failureOutput = JSON.stringify({
    hookSpecificOutput: { hookEventName: 'PostToolUseFailure', additionalContext: 'outcome unknown' },
    systemMessage: 'Purser failure recorded',
  })
  let invocation = 0
  stubGo(on, postOutput, calls, toasts, undefined, undefined, undefined, () => ({ stdout: invocation++ === 0 ? postOutput : failureOutput }))
  on('classic.PostToolUse', () => ({}))
  on('classic.PostToolUseFailure', () => ({}))

  const success = await $.classic.PostToolUse(postEvent('PostToolUse'))
  expect(success).toEqual({
    updatedToolOutput: [{ type: 'text', text: 'reviewed' }],
    additionalContext: ['review required'],
  })
  expect(toasts).toContain('Purser post review complete')
  expect(JSON.parse(calls[0])).toMatchObject({ hook_event_name: 'PostToolUse', tool_use_id: 'post-call' })

  const failure = await $.classic.PostToolUseFailure(postEvent('PostToolUseFailure'))
  expect(failure).toEqual({ additionalContext: ['outcome unknown'] })
  expect(toasts).toContain('Purser failure recorded')
  expect(JSON.parse(calls[1])).toMatchObject({ hook_event_name: 'PostToolUseFailure', tool_use_id: 'post-call' })
})

test('empty Go Post response passes through', options, async ($, on) => {
  const calls: string[] = []
  const toasts: string[] = []
  stubGo(on, '', calls, toasts)
  on('classic.PostToolUse', () => ({}))
  on('classic.PostToolUseFailure', () => ({}))

  await expect($.classic.PostToolUse(postEvent('PostToolUse'))).resolves.toEqual({})
  expect(calls).toHaveLength(1)
})

test('invalid Go Post JSON passes through without throwing', options, async ($, on) => {
  const calls: string[] = []
  const toasts: string[] = []
  stubGo(on, '{', calls, toasts)
  on('classic.PostToolUse', () => ({}))
  on('classic.PostToolUseFailure', () => ({}))

  await expect($.classic.PostToolUse(postEvent('PostToolUse'))).resolves.toEqual({})
  expect(calls).toHaveLength(1)
})

test('Post mod skips an event already handled by an earlier hook', options, async ($, on) => {
  const calls: string[] = []
  const toasts: string[] = []
  stubGo(on, postOutput, calls, toasts)
  on('classic.PostToolUse', () => ({}))
  on('classic.PostToolUseFailure', () => ({}))

  const event = postEvent('PostToolUse')
  Object.assign(event, { hookSpecificOutput: { hookEventName: 'PostToolUse' } })
  await expect($.classic.PostToolUse(event)).resolves.toEqual({})
  expect(calls).toEqual([])
})

test('PostToolUse routes six registered Codex tools without an env gate and excludes non-Codex tools', options, async ($, on) => {
  const calls: string[] = []
  const toasts: string[] = []
  stubGo(on, postOutput, calls, toasts)
  on('classic.PostToolUse', () => ({}))
  on('classic.PostToolUseFailure', () => ({}))

  for (const tool of [
    'mcp__plugin_purser_codex-main__codex', 'mcp__plugin_purser_codex-main__codex-reply',
    'mcp__plugin_purser_codex-main__codex_start', 'mcp__plugin_purser_codex-main__codex_result',
    'mcp__plugin_purser_codex-main__codex_cancel', 'mcp__plugin_purser_codex-main__codex_apply',
  ]) {
    const event = { ...postEvent('PostToolUse'), tool_name: tool }
    await expect($.classic.PostToolUse(event)).resolves.toEqual({
      updatedToolOutput: [{ type: 'text', text: 'reviewed' }], additionalContext: ['review required'],
    })
  }
  const excluded = { ...postEvent('PostToolUse'), tool_name: 'mcp__plugin_purser_codex-main__other' }
  await expect($.classic.PostToolUse(excluded)).resolves.toEqual({})
  expect(calls).toHaveLength(6)
  expect(calls.map(call => JSON.parse(call).tool_name)).toEqual([
    'mcp__plugin_purser_codex-main__codex', 'mcp__plugin_purser_codex-main__codex-reply',
    'mcp__plugin_purser_codex-main__codex_start', 'mcp__plugin_purser_codex-main__codex_result',
    'mcp__plugin_purser_codex-main__codex_cancel', 'mcp__plugin_purser_codex-main__codex_apply',
  ])
})

test('PostToolUseFailure routes five Codex tools without the Post gate and excludes codex_apply', options, async ($, on) => {
  const calls: string[] = []
  const toasts: string[] = []
  stubGo(on, JSON.stringify({ hookSpecificOutput: {
    hookEventName: 'PostToolUseFailure', additionalContext: 'failure handled',
  } }), calls, toasts)
  on('classic.PostToolUse', () => ({}))
  on('classic.PostToolUseFailure', () => ({}))

  for (const tool of [
    'mcp__plugin_purser_codex-main__codex', 'mcp__plugin_purser_codex-main__codex-reply',
    'mcp__plugin_purser_codex-main__codex_start', 'mcp__plugin_purser_codex-main__codex_result',
    'mcp__plugin_purser_codex-main__codex_cancel',
  ]) {
    const event = { ...postEvent('PostToolUseFailure'), tool_name: tool }
    await expect($.classic.PostToolUseFailure(event)).resolves.toEqual({ additionalContext: ['failure handled'] })
  }
  const excluded = { ...postEvent('PostToolUseFailure'), tool_name: 'mcp__plugin_purser_codex-main__codex_apply' }
  await expect($.classic.PostToolUseFailure(excluded)).resolves.toEqual({})
  expect(calls).toHaveLength(5)
  expect(calls.map(call => JSON.parse(call).tool_name)).toEqual([
    'mcp__plugin_purser_codex-main__codex', 'mcp__plugin_purser_codex-main__codex-reply',
    'mcp__plugin_purser_codex-main__codex_start', 'mcp__plugin_purser_codex-main__codex_result',
    'mcp__plugin_purser_codex-main__codex_cancel',
  ])
})

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

test('all Codex PreToolUse identities reach Go while non-Codex tools do not', options, async ($, on) => {
  const calls: string[] = []
  const toolCalls: unknown[] = []
  const toasts: string[] = []
  stubGo(on, success, calls, toasts)
  stubToolCall(on, toolCalls)

  const codexTools = [
    'mcp__codex-main__codex', 'mcp__codex-main__codex-reply',
    'mcp__codex-main__codex_start', 'mcp__codex-main__codex_result', 'mcp__codex-main__codex_cancel',
    'mcp__plugin_purser_codex-main__codex', 'mcp__plugin_purser_codex-main__codex-reply',
    'mcp__plugin_purser_codex-main__codex_start', 'mcp__plugin_purser_codex-main__codex_result',
    'mcp__plugin_purser_codex-main__codex_cancel',
  ]
  for (const tool of codexTools) {
    await $.tool.call({ tool, task_id: 'origin-task' })
  }
  await $.tool.call({ tool: 'mcp__other-server__query', query: 'hello' })

  expect(calls).toHaveLength(10)
  expect(calls.map(stdin => JSON.parse(stdin).tool_name)).toEqual(codexTools)
  expect(toolCalls).toHaveLength(11)
})

test('maps Go follow-up deny and passes allowed follow-up input unchanged', options, async ($, on) => {
  const calls: string[] = []
  const toolCalls: unknown[] = []
  const toasts: string[] = []
  let response = JSON.stringify({ hookSpecificOutput: {
    hookEventName: 'PreToolUse', permissionDecision: 'deny', permissionDecisionReason: 'ASYNC_STILL_RUNNING: still running; wait for the completion signal',
  } })
  stubGo(on, success, calls, toasts, undefined, undefined, undefined, () => ({ stdout: response }))
  stubToolCall(on, toolCalls)

  const denied = await $.tool.call({ tool: 'mcp__plugin_purser_codex-main__codex_result', task_id: 'origin-task' })
  expect(observed(toasts)).toEqual({ deny: 'ASYNC_STILL_RUNNING: still running; wait for the completion signal' })
  expect(denied.isError).toBe(true)
  expect(toolCalls).toEqual([])

  response = JSON.stringify({ hookSpecificOutput: { hookEventName: 'PreToolUse' } })
  toasts.length = 0
  const input = { tool: 'mcp__plugin_purser_codex-main__codex_cancel', task_id: 'origin-task', reason: 'stop' }
  await $.tool.call(input)
  expect(JSON.parse(calls[1])).toMatchObject({ tool_name: input.tool, tool_input: { task_id: 'origin-task', reason: 'stop' } })
  expect(observed(toasts)).toEqual({})
  expect(toolCalls).toHaveLength(1)
  expect(toolCalls[0]).toMatchObject({ task_id: 'origin-task', reason: 'stop' })
})

test('empty Go Pre output passes codex_result through unchanged', options, async ($, on) => {
  const calls: string[] = []
  const toolCalls: unknown[] = []
  const toasts: string[] = []
  let stdout = ''
  stubGo(on, '', calls, toasts, undefined, undefined, undefined, () => ({ stdout }))
  on('classic.PreToolUse', () => ({ additionalContext: ['prior context'] }))
  stubToolCall(on, toolCalls)

  for (const output of ['{}\n', '']) {
    stdout = output
    await $.tool.call({ tool: 'mcp__plugin_purser_codex-main__codex_result', task_id: 'origin-task' })
    expect(observed(toasts)).toEqual({ additionalContext: ['prior context'] })
  }

  expect(calls).toHaveLength(2)
  expect(toolCalls).toHaveLength(2)
})

test('malformed Go Pre output adds the follow-up bypass context', options, async ($, on) => {
  const calls: string[] = []
  const toolCalls: unknown[] = []
  const toasts: string[] = []
  stubGo(on, '{', calls, toasts)
  on('classic.PreToolUse', () => ({ additionalContext: ['prior context'] }))
  stubToolCall(on, toolCalls)

  await $.tool.call({ tool: 'mcp__plugin_purser_codex-main__codex_result', task_id: 'origin-task' })

  const context = observed(toasts).additionalContext.join(' ')
  expect(context).toContain('prior context')
  expect(context).toContain('Orchestrator routing was NOT enforced')
  expect(toolCalls).toHaveLength(1)
  expect(calls).toHaveLength(1)
})

test('follow-up adapter failure allows unchanged with Python internal-error context and no marker', options, async ($, on) => {
  const calls: string[] = []
  const markerWrites: string[] = []
  const toolCalls: unknown[] = []
  const toasts: string[] = []
  stubGo(on, success, calls, toasts, undefined, undefined, undefined, () => ({ deny: 'Go helper unavailable' }), markerWrites)
  stubToolCall(on, toolCalls)

  await $.tool.call({ tool: 'mcp__plugin_purser_codex-main__codex_result', task_id: 'origin-task' })

  expect(observed(toasts).additionalContext[0]).toContain('because of an internal error: ')
  expect(observed(toasts).additionalContext[0]).toContain('Go helper unavailable')
  expect(observed(toasts).additionalContext[0]).toContain('The call is allowed unmodified.')
  expect(observed(toasts).additionalContext[0]).toContain('The bypass record could not be persisted')
  expect(markerWrites).toEqual([])
  expect(toolCalls).toHaveLength(1)
  expect(toolCalls[0]).toMatchObject({ task_id: 'origin-task' })
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
    { name: 'invalid mapped output', outcome: { stdout: JSON.stringify({ hookSpecificOutput: { hookEventName: 'PreToolUse', permissionDecision: 'allow' } }) }, message: 'invalid Go PreToolUse decision' },
    { name: 'interruption after pending write', outcome: { deny: 'process killed after pending admission' }, message: 'process killed after pending admission' },
  ]
  const calls: string[] = []
  const markerWrites: string[] = []
  const toolCalls: unknown[] = []
  const toasts: string[] = []
  let currentFailure = failures[0]
  stubGo(on, success, calls, toasts, undefined, undefined, undefined, () => currentFailure.outcome, markerWrites)
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
  expect(markerWrites).toHaveLength(failures.length * 2)
  for (let index = 0; index < markerWrites.length; index++) {
    const request = JSON.parse(markerWrites[index])
    const pre = JSON.parse(calls[index])
    expect(request).toMatchObject({
      hook_event_name: 'WriteUndeliveredMarker', session_id: pre.session_id,
      tool_use_id: pre.tool_use_id, tool_name: pre.tool_name,
    })
    expect(request.reason).toBeTruthy()
  }
})

test('marker write failure preserves sync context and codex_start deny', options, async ($, on) => {
  const calls: string[] = []
  const markerWrites: string[] = []
  const toolCalls: unknown[] = []
  const toasts: string[] = []
  stubGo(on, success, calls, toasts, undefined, undefined, undefined,
    () => ({ exitCode: 1 }), markerWrites, () => ({ deny: 'marker write failed' }))
  stubToolCall(on, toolCalls)

  for (const tool of ['mcp__plugin_purser_codex-main__codex', 'mcp__plugin_purser_codex-main__codex_start']) {
    toasts.length = 0
    const before = toolCalls.length
    const result = await $.tool.call({ tool, prompt })
    const mapped = observed(toasts)
    if (tool.endsWith('__codex_start')) {
      expect(mapped.deny).toContain('ASYNC_ENFORCEMENT_FAILED')
      expect(result.isError).toBe(true)
      expect(toolCalls).toHaveLength(before)
    } else {
      expect(mapped.additionalContext.join(' ')).toContain('FULL Claude review is required')
      expect(result.isError).not.toBe(true)
      expect(toolCalls).toHaveLength(before + 1)
    }
  }
  expect(markerWrites).toHaveLength(2)
})
