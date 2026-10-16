type PostEvent = 'PostToolUse' | 'PostToolUseFailure'
const postToolUseFailureTools = new Set([
  'mcp__codex-main__codex', 'mcp__codex-main__codex-reply',
  'mcp__codex-main__codex_start', 'mcp__codex-main__codex_result',
  'mcp__codex-main__codex_cancel',
  'mcp__plugin_purser_codex-main__codex', 'mcp__plugin_purser_codex-main__codex-reply',
  'mcp__plugin_purser_codex-main__codex_start', 'mcp__plugin_purser_codex-main__codex_result',
  'mcp__plugin_purser_codex-main__codex_cancel',
])
const postToolUseTools = new Set([
  'mcp__codex-main__codex', 'mcp__codex-main__codex-reply',
  'mcp__codex-main__codex_start', 'mcp__codex-main__codex_result',
  'mcp__codex-main__codex_cancel', 'mcp__codex-main__codex_apply',
  'mcp__plugin_purser_codex-main__codex', 'mcp__plugin_purser_codex-main__codex-reply',
  'mcp__plugin_purser_codex-main__codex_start', 'mcp__plugin_purser_codex-main__codex_result',
  'mcp__plugin_purser_codex-main__codex_cancel', 'mcp__plugin_purser_codex-main__codex_apply',
])

// The Go PreToolUse request. A subagent's call carries agent_id, so Go can gate
// its codex_apply on main Claude's recorded acceptance.
export function preRequest(e: Record<string, unknown>, sessionId: string, cwd: string) {
  const { tool, tool_use_id, agentId, ...tool_input } = e
  return {
    session_id: sessionId, cwd, hook_event_name: 'PreToolUse',
    tool_name: tool, tool_use_id, tool_input,
    agent_id: typeof agentId === 'string' ? agentId : '',
  }
}

function mapPostToolUse(event: PostEvent, stdout: string): {
  updatedToolOutput?: unknown
  additionalContext?: string[]
  systemMessage?: string
} {
  if (!stdout.trim()) return {}
  const output = JSON.parse(stdout)
  if (output !== null && typeof output === 'object' && !Array.isArray(output) && Object.keys(output).length === 0) return {}
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
  if (event === 'PostToolUse' && !postToolUseTools.has(e.tool_name)) return prior
  if (event === 'PostToolUseFailure' && !postToolUseFailureTools.has(e.tool_name)) return prior
  if (e.hookSpecificOutput?.hookEventName === event ||
      e.tool_response?.hookSpecificOutput?.hookEventName === event) return prior
  let failureReason = 'helper could not run'
  try {
    const binary = await goBinary($)
    const result = await $.process.run([binary, 'enforce'], {
      cwd: e.cwd,
      stdin: JSON.stringify(e),
      timeoutMs: 300000,
    })
    if (result.exitCode !== 0) {
      failureReason = `helper exited with code ${result.exitCode}`
      throw new Error(failureReason)
    }
    let mapped: ReturnType<typeof mapPostToolUse>
    try {
      mapped = mapPostToolUse(event, result.stdout)
    } catch {
      failureReason = 'invalid helper output'
      throw new Error(failureReason)
    }
    if (mapped.systemMessage) {
      try { await $.ui.toast(mapped.systemMessage) }
      catch { /* Toast delivery must not discard a valid enforcement result. */ }
    }
    if (!mapped.updatedToolOutput && !mapped.additionalContext && !mapped.systemMessage) return prior
    return {
      ...prior,
      ...(mapped.updatedToolOutput ? { updatedToolOutput: mapped.updatedToolOutput } : {}),
      ...(mapped.additionalContext ? { additionalContext: [...(prior.additionalContext ?? []), ...mapped.additionalContext] } : {}),
    }
  } catch {
    return {
      ...prior,
      additionalContext: [...(prior.additionalContext ?? []),
        `Purser ${event} enforcement failed for this tool call (${e.tool_name ?? 'unknown tool'}): ${failureReason}. FULL Claude review is required.`],
    }
  }
}

export function register(on) {
  on("classic.PostToolUse", ($, e, next) => handlePostToolUse('PostToolUse', $, e, next))
  on("classic.PostToolUseFailure", ($, e, next) => handlePostToolUse('PostToolUseFailure', $, e, next))

  on('classic.PreToolUse', async ($, e, next) => {
    if (!postToolUseTools.has(e.tool) && !postToolUseFailureTools.has(e.tool)) return next(e)
    try {
      const binary = await goBinary($)
      const sessionId = await $.session.id()
      const cwd = await $.session.cwd()
      const result = await $.process.run([binary, 'mcp-handoff'], {
        cwd,
        stdin: JSON.stringify(preRequest(e, sessionId, cwd)),
        timeoutMs: 10000,
      })
      if (result.exitCode !== 0) return { deny: `MCP handoff publication failed: ${result.stderr || `exit ${result.exitCode}`}` }
      return next(e)
    } catch (error) {
      return { deny: `MCP handoff publication failed: ${error instanceof Error ? error.message : String(error)}` }
    }
  })
}
