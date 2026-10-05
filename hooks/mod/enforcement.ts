type PostEvent = 'PostToolUse' | 'PostToolUseFailure'
const postToolUseFailureTools = new Set([
  'mcp__plugin_purser_codex-main__codex', 'mcp__plugin_purser_codex-main__codex-reply',
  'mcp__plugin_purser_codex-main__codex_start', 'mcp__plugin_purser_codex-main__codex_result',
  'mcp__plugin_purser_codex-main__codex_cancel',
])

function mapPostToolUse(event: PostEvent, stdout: string): {
  updatedToolOutput?: unknown
  additionalContext?: string[]
  systemMessage?: string
} {
  if (!stdout.trim()) return {}
  const output = JSON.parse(stdout)
  const specific = output?.hookSpecificOutput
  if (specific?.hookEventName !== event) throw new Error(`invalid Go ${event} output`)
  if (specific.updatedToolOutput !== undefined && !Array.isArray(specific.updatedToolOutput)) {
    throw new Error(`invalid Go ${event} updatedToolOutput`)
  }
  if (specific.additionalContext !== undefined && typeof specific.additionalContext !== 'string') {
    throw new Error(`invalid Go ${event} additionalContext`)
  }
  if (output.systemMessage !== undefined && typeof output.systemMessage !== 'string') {
    throw new Error(`invalid Go ${event} systemMessage`)
  }
  return {
    ...(specific.updatedToolOutput !== undefined ? { updatedToolOutput: specific.updatedToolOutput } : {}),
    ...(specific.additionalContext ? { additionalContext: [specific.additionalContext] } : {}),
    ...(output.systemMessage ? { systemMessage: output.systemMessage } : {}),
  }
}

async function goBinary($): Promise<string> {
  const os = await $.env.get('OS')
  const ostype = await $.env.get('OSTYPE')
  const architecture = await $.env.get('PROCESSOR_ARCHITECTURE')
  const hosttype = await $.env.get('HOSTTYPE')
  let goos = os === 'Windows_NT' ? 'windows' :
    /darwin/i.test(ostype ?? '') ? 'darwin' : /linux/i.test(ostype ?? '') ? 'linux' : ''
  let goarch = /^(amd64|x86_64)$/i.test(architecture ?? hosttype ?? '') ? 'amd64' :
    /^(arm64|aarch64)$/i.test(architecture ?? hosttype ?? '') ? 'arm64' : ''
  if (!goos || !goarch) {
    const uname = await $.process.run(['uname', '-s', '-m'], { timeoutMs: 1000 })
    if (uname.exitCode !== 0) throw new Error('platform detection failed')
    const [system, machine] = uname.stdout.trim().split(/\s+/)
    goos ||= /darwin/i.test(system ?? '') ? 'darwin' : /linux/i.test(system ?? '') ? 'linux' : ''
    goarch ||= /^(amd64|x86_64)$/i.test(machine ?? '') ? 'amd64' :
      /^(arm64|aarch64)$/i.test(machine ?? '') ? 'arm64' : ''
  }
  const osName = goos.toLowerCase()
  const archName = goarch.toLowerCase()
  if (!['windows/amd64', 'windows/arm64', 'linux/amd64', 'linux/arm64', 'darwin/amd64', 'darwin/arm64'].includes(`${osName}/${archName}`)) {
    throw new Error(`unsupported platform: ${goos}/${goarch}`)
  }
  return `${$.plugin.root}/bin/purser-hook-${osName}-${archName}${osName === 'windows' ? '.exe' : ''}`
}

async function handlePostToolUse(event: PostEvent, $, e, next) {
  const prior = await next(e)
  if (event === 'PostToolUse' && await $.env.get('PURSER_MOD_POST') !== '1') return prior
  if (event === 'PostToolUseFailure' && !postToolUseFailureTools.has(e.tool_name)) return prior
  if (e.hookSpecificOutput?.hookEventName === event ||
      e.tool_response?.hookSpecificOutput?.hookEventName === event) return prior
  try {
    const binary = await goBinary($)
    const result = await $.process.run([binary], {
      cwd: e.cwd,
      stdin: JSON.stringify(e),
      timeoutMs: 60000,
    })
    if (result.exitCode !== 0) throw new Error(`Go ${event} helper exited ${result.exitCode}`)
    const mapped = mapPostToolUse(event, result.stdout)
    if (mapped.systemMessage) await $.ui.toast(mapped.systemMessage)
    if (!mapped.updatedToolOutput && !mapped.additionalContext && !mapped.systemMessage) return prior
    return {
      ...prior,
      ...(mapped.updatedToolOutput ? { updatedToolOutput: mapped.updatedToolOutput } : {}),
      ...(mapped.additionalContext ? { additionalContext: [...(prior.additionalContext ?? []), ...mapped.additionalContext] } : {}),
    }
  } catch {
    return prior
  }
}

export function register(on) {
  const syncTools = new Set([
    'mcp__codex-main__codex', 'mcp__codex-main__codex-reply',
    'mcp__plugin_purser_codex-main__codex', 'mcp__plugin_purser_codex-main__codex-reply',
  ])
  // Async start and follow-up tools use the Go adapter for both identities.
  const startTools = new Set(['mcp__codex-main__codex_start', 'mcp__plugin_purser_codex-main__codex_start'])
  const followupTools = new Set([
    'mcp__codex-main__codex_result', 'mcp__codex-main__codex_cancel',
    'mcp__plugin_purser_codex-main__codex_result', 'mcp__plugin_purser_codex-main__codex_cancel',
  ])
  function mapPreToolUse(stdout: string): {
    deny?: string
    updatedInput?: Record<string, unknown>
    additionalContext?: string[]
    systemMessage?: string
  } {
    const output = JSON.parse(stdout)
    const specific = output?.hookSpecificOutput
    if (specific?.hookEventName !== 'PreToolUse') throw new Error('invalid Go PreToolUse output')
    if (specific.permissionDecision === 'deny' && typeof specific.permissionDecisionReason === 'string') {
      return { deny: specific.permissionDecisionReason }
    }
    if (specific.permissionDecision !== undefined) throw new Error('invalid Go PreToolUse decision')
    if (specific.updatedInput !== undefined &&
        (specific.updatedInput === null || typeof specific.updatedInput !== 'object' || Array.isArray(specific.updatedInput))) {
      throw new Error('invalid Go PreToolUse updatedInput')
    }
    if (specific.additionalContext !== undefined && typeof specific.additionalContext !== 'string') {
      throw new Error('invalid Go PreToolUse additionalContext')
    }
    if (output.systemMessage !== undefined && typeof output.systemMessage !== 'string') {
      throw new Error('invalid Go PreToolUse systemMessage')
    }
    return {
      ...(specific.updatedInput ? { updatedInput: specific.updatedInput } : {}),
      ...(specific.additionalContext ? { additionalContext: [specific.additionalContext] } : {}),
      ...(output.systemMessage ? { systemMessage: output.systemMessage } : {}),
    }
  }

  on("classic.PostToolUse", ($, e, next) => handlePostToolUse('PostToolUse', $, e, next))
  on("classic.PostToolUseFailure", ($, e, next) => handlePostToolUse('PostToolUseFailure', $, e, next))

  on('classic.PreToolUse', async ($, e, next) => {
    const prior = await next(e)
    if (prior.deny !== undefined) return prior
    if (!syncTools.has(e.tool) && !startTools.has(e.tool) && !followupTools.has(e.tool)) return prior

    let helperBinary = ''
    let helperCwd = ''
    let helperSessionId = ''
    try {
      const binary = await goBinary($)
      const sessionId = await $.session.id()
      helperBinary = binary
      helperSessionId = sessionId
      const cwd = await $.session.cwd()
      helperCwd = cwd
      const { tool, tool_use_id, agentId: _agentId, ...tool_input } = e
      const result = await $.process.run([helperBinary], {
        cwd,
        stdin: JSON.stringify({
          session_id: sessionId, cwd, hook_event_name: 'PreToolUse',
          tool_name: tool, tool_use_id, tool_input,
        }),
        timeoutMs: 60000,
      })
      if (result.exitCode !== 0) throw new Error(`Go PreToolUse helper exited ${result.exitCode}`)
      const mapped = mapPreToolUse(result.stdout)
      if (mapped.systemMessage) await $.ui.toast(mapped.systemMessage)
      if (!mapped.deny && !mapped.updatedInput && !mapped.additionalContext) return prior
      return {
        ...prior,
        ...(mapped.deny ? { deny: mapped.deny } : {}),
        ...(mapped.updatedInput ? { updatedInput: mapped.updatedInput } : {}),
        ...(mapped.additionalContext ? { additionalContext: [...(prior.additionalContext ?? []), ...mapped.additionalContext] } : {}),
      }
    } catch (error) {
      const reason = error instanceof Error ? error.message : String(error)
      if (!followupTools.has(e.tool) && helperBinary && typeof e.tool_use_id === 'string') {
        try {
          await $.process.run([helperBinary], {
            cwd: helperCwd,
            stdin: JSON.stringify({
              hook_event_name: 'WriteUndeliveredMarker', session_id: helperSessionId,
              tool_use_id: e.tool_use_id, tool_name: e.tool, reason,
            }),
            timeoutMs: 1000,
          })
        } catch { /* Marker persistence is best-effort; preserve the existing deny/FULL context. */ }
      }
      if (e.tool.endsWith('__codex_start')) {
        return { ...prior, deny: `ASYNC_ENFORCEMENT_FAILED: Go PreToolUse failed: ${reason}. Async start lacks a durable route/baseline; FULL review is required.` }
      }
      if (followupTools.has(e.tool)) {
        return {
          ...prior,
          additionalContext: [...(prior.additionalContext ?? []),
            `Orchestrator routing was NOT enforced for this Codex call because of an internal error: ${reason}. The call is allowed unmodified. The bypass record could not be persisted; this PreToolUse context is the only warning.`],
        }
      }
      return {
        ...prior,
        additionalContext: [...(prior.additionalContext ?? []),
          `Orchestrator routing was NOT enforced for this Codex call because Go PreToolUse failed: ${reason}. The call is allowed unmodified; FULL Claude review is required.`],
      }
    }
  })
}
