import { expect, test } from 'claude-code/testing'
import { preRequest } from './enforcement.js'

const prompt = 'ORCH|task=t1|phase=implementation|route=normal'
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
  toastFailure = false,
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
    if (e.argv[1] === 'mcp-handoff') {
      expect(request.hook_event_name).toBe('PreToolUse')
      expect(e.init.timeoutMs).toBe(10000)
      calls.push(e.init.stdin)
      const result = outcome?.()
      if (result?.deny) return { deny: result.deny }
      return { value: { exitCode: result?.exitCode ?? 0, stdout: result?.stdout ?? '{"written":true}', stderr: result?.stderr ?? '' } }
    }
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
  on('ui.toast', (_$, e) => {
    toasts.push(e.text)
    if (toastFailure) throw new Error('toast failed')
    return { value: undefined }
  })
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

test('invalid Go Post JSON preserves the host result and requires FULL review', options, async ($, on) => {
  const calls: string[] = []
  const toasts: string[] = []
  stubGo(on, '{', calls, toasts)
  on('classic.PostToolUse', () => ({ additionalContext: ['host context'] }))
  on('classic.PostToolUseFailure', () => ({}))

  await expect($.classic.PostToolUse(postEvent('PostToolUse'))).resolves.toEqual({
    additionalContext: [
      'host context',
      'Purser PostToolUse enforcement failed for this tool call (mcp__plugin_purser_codex-main__codex): invalid helper output. FULL Claude review is required.',
    ],
  })
  expect(calls).toHaveLength(1)
})

test('an empty Go Post object, as after an acceptance, adds nothing', options, async ($, on) => {
  const calls: string[] = []
  stubGo(on, postOutput, calls, [], undefined, undefined, undefined, () => ({ stdout: '{}' }))
  on('classic.PostToolUse', () => ({}))

  const apply = { ...postEvent('PostToolUse'), tool_name: 'mcp__plugin_purser_codex-main__codex_apply' }
  await expect($.classic.PostToolUse(apply)).resolves.toEqual({})
  expect(calls).toHaveLength(1)
})

test('nonzero Go Post exit preserves the host result and requires FULL review', options, async ($, on) => {
  const calls: string[] = []
  const toasts: string[] = []
  stubGo(on, postOutput, calls, toasts, undefined, undefined, undefined, () => ({ exitCode: 7 }))
  on('classic.PostToolUse', () => ({}))
  on('classic.PostToolUseFailure', () => ({}))

  await expect($.classic.PostToolUse(postEvent('PostToolUse'))).resolves.toEqual({
    additionalContext: [
      'Purser PostToolUse enforcement failed for this tool call (mcp__plugin_purser_codex-main__codex): helper exited with code 7. FULL Claude review is required.',
    ],
  })
  expect(calls).toHaveLength(1)
})

test('invalid Go Post mapping preserves the host result and names the event', options, async ($, on) => {
  const calls: string[] = []
  const toasts: string[] = []
  stubGo(on, postOutput, calls, toasts, undefined, undefined, undefined, () => ({
    stdout: JSON.stringify({ hookSpecificOutput: { hookEventName: 'PostToolUseFailure' } }),
  }))
  on('classic.PostToolUse', () => ({}))
  on('classic.PostToolUseFailure', () => ({}))

  await expect($.classic.PostToolUse(postEvent('PostToolUse'))).resolves.toEqual({
    additionalContext: [
      'Purser PostToolUse enforcement failed for this tool call (mcp__plugin_purser_codex-main__codex): invalid helper output. FULL Claude review is required.',
    ],
  })
  expect(calls).toHaveLength(1)
})

test('Post toast rejection preserves mapped enforcement output', options, async ($, on) => {
  const calls: string[] = []
  const toasts: string[] = []
  stubGo(on, postOutput, calls, toasts, undefined, undefined, undefined, undefined, undefined, undefined, true)
  on('classic.PostToolUse', () => ({ additionalContext: ['host context'] }))
  on('classic.PostToolUseFailure', () => ({}))

  await expect($.classic.PostToolUse(postEvent('PostToolUse'))).resolves.toEqual({
    updatedToolOutput: [{ type: 'text', text: 'reviewed' }],
    additionalContext: ['host context', 'review required'],
  })
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

test('PostToolUse routes both identities of six Codex tools without an env gate and excludes non-Codex tools', options, async ($, on) => {
  const calls: string[] = []
  const toasts: string[] = []
  stubGo(on, postOutput, calls, toasts)
  on('classic.PostToolUse', () => ({}))
  on('classic.PostToolUseFailure', () => ({}))

  for (const tool of [
    'mcp__codex-main__codex', 'mcp__codex-main__codex-reply',
    'mcp__codex-main__codex_start', 'mcp__codex-main__codex_result',
    'mcp__codex-main__codex_cancel', 'mcp__codex-main__codex_apply',
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
  expect(calls).toHaveLength(12)
  expect(calls.map(call => JSON.parse(call).tool_name)).toEqual([
    'mcp__codex-main__codex', 'mcp__codex-main__codex-reply',
    'mcp__codex-main__codex_start', 'mcp__codex-main__codex_result',
    'mcp__codex-main__codex_cancel', 'mcp__codex-main__codex_apply',
    'mcp__plugin_purser_codex-main__codex', 'mcp__plugin_purser_codex-main__codex-reply',
    'mcp__plugin_purser_codex-main__codex_start', 'mcp__plugin_purser_codex-main__codex_result',
    'mcp__plugin_purser_codex-main__codex_cancel', 'mcp__plugin_purser_codex-main__codex_apply',
  ])
})

test('PostToolUseFailure routes both identities of five Codex tools without the Post gate and excludes codex_apply', options, async ($, on) => {
  const calls: string[] = []
  const toasts: string[] = []
  stubGo(on, JSON.stringify({ hookSpecificOutput: {
    hookEventName: 'PostToolUseFailure', additionalContext: 'failure handled',
  } }), calls, toasts)
  on('classic.PostToolUse', () => ({}))
  on('classic.PostToolUseFailure', () => ({}))

  for (const tool of [
    'mcp__codex-main__codex', 'mcp__codex-main__codex-reply',
    'mcp__codex-main__codex_start', 'mcp__codex-main__codex_result',
    'mcp__codex-main__codex_cancel',
    'mcp__plugin_purser_codex-main__codex', 'mcp__plugin_purser_codex-main__codex-reply',
    'mcp__plugin_purser_codex-main__codex_start', 'mcp__plugin_purser_codex-main__codex_result',
    'mcp__plugin_purser_codex-main__codex_cancel',
  ]) {
    const event = { ...postEvent('PostToolUseFailure'), tool_name: tool }
    await expect($.classic.PostToolUseFailure(event)).resolves.toEqual({ additionalContext: ['failure handled'] })
  }
  const excluded = { ...postEvent('PostToolUseFailure'), tool_name: 'mcp__plugin_purser_codex-main__codex_apply' }
  await expect($.classic.PostToolUseFailure(excluded)).resolves.toEqual({})
  expect(calls).toHaveLength(10)
  expect(calls.map(call => JSON.parse(call).tool_name)).toEqual([
    'mcp__codex-main__codex', 'mcp__codex-main__codex-reply',
    'mcp__codex-main__codex_start', 'mcp__codex-main__codex_result',
    'mcp__codex-main__codex_cancel',
    'mcp__plugin_purser_codex-main__codex', 'mcp__plugin_purser_codex-main__codex-reply',
    'mcp__plugin_purser_codex-main__codex_start', 'mcp__plugin_purser_codex-main__codex_result',
    'mcp__plugin_purser_codex-main__codex_cancel',
  ])
})



test('valid MCP handoff publishes identity and forwards original input unchanged', options, async ($, on) => {
  const calls: string[] = []
  const toasts: string[] = []
  const toolCalls: unknown[] = []
  stubGo(on, '', calls, toasts)
  stubToolCall(on, toolCalls)

  const input = { tool: 'mcp__plugin_purser_codex-main__codex', prompt }
  await $.tool.call(input)

  expect(calls).toHaveLength(1)
  expect(JSON.parse(calls[0])).toMatchObject({
    hook_event_name: 'PreToolUse',
    tool_name: input.tool,
    tool_input: { prompt },
    session_id: 'p66-session',
    cwd: 'C:/p66-repo',
  })
  expect(toolCalls).toHaveLength(1)
  expect(toolCalls[0]).toMatchObject({ tool: input.tool, prompt })
  expect(toolCalls[0]).not.toHaveProperty('model')
  expect(toasts).toContain('p66-result:{}')
})

test('handoff publication failure denies before the MCP call', options, async ($, on) => {
  const calls: string[] = []
  const toasts: string[] = []
  const toolCalls: unknown[] = []
  stubGo(on, '', calls, toasts, undefined, undefined, undefined, () => ({ exitCode: 1, stderr: 'sharing violation' }))
  stubToolCall(on, toolCalls)

  const result = await $.tool.call({ tool: 'mcp__plugin_purser_codex-main__codex', prompt })

  expect(result.isError).toBe(true)
  expect(result.text).toContain('MCP handoff publication failed')
  expect(result.text).toContain('sharing violation')
  expect(toolCalls).toEqual([])
})

test('non-Purser MCP calls continue without publication', options, async ($, on) => {
  const calls: string[] = []
  const toasts: string[] = []
  const toolCalls: unknown[] = []
  stubGo(on, '', calls, toasts)
  stubToolCall(on, toolCalls)

  await $.tool.call({ tool: 'mcp__other-server__query', query: 'hello' })

  expect(calls).toEqual([])
  expect(toolCalls).toHaveLength(1)
  expect(toolCalls[0]).toMatchObject({ tool: 'mcp__other-server__query', query: 'hello' })
})

test('handoff request carries subagent identity without changing arguments', () => {
  const event = {
    tool: 'mcp__plugin_purser_codex-main__codex_apply',
    tool_use_id: 'toolu-1',
    prompt: 'same',
    agentId: 'dispatcher-1',
  }
  expect(preRequest(event, 'session-1', 'C:/repo')).toEqual({
    session_id: 'session-1',
    cwd: 'C:/repo',
    hook_event_name: 'PreToolUse',
    tool_name: event.tool,
    tool_use_id: 'toolu-1',
    tool_input: { prompt: 'same' },
    agent_id: 'dispatcher-1',
  })
})
