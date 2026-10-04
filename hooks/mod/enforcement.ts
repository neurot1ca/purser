export function register(on) {
  const codexTools = new Set([
    'mcp__codex-main__codex', 'mcp__codex-main__codex-reply',
    'mcp__plugin_purser_codex-main__codex', 'mcp__plugin_purser_codex-main__codex-reply',
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

  on('classic.PreToolUse', async ($, e, next) => {
    const prior = await next(e)
    if (!codexTools.has(e.tool) || prior.deny !== undefined) return prior

    try {
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
      const [sessionId, cwd] = await Promise.all([$.session.id(), $.session.cwd()])
      const { tool, tool_use_id, agentId: _agentId, ...tool_input } = e
      const result = await $.process.run([`${$.plugin.root}/bin/purser-hook-${osName}-${archName}${osName === 'windows' ? '.exe' : ''}`], {
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
      return {
        ...prior,
        additionalContext: [...(prior.additionalContext ?? []),
          `Orchestrator routing was NOT enforced for this Codex call because Go PreToolUse failed: ${reason}. The call is allowed unmodified; FULL Claude review is required.`],
      }
    }
  })
}
