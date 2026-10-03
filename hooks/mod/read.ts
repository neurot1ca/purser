export function platformBinary(goos: string, goarch: string): string {
  const os = goos.toLowerCase()
  const arch = goarch.toLowerCase()
  if (!['windows', 'linux', 'darwin'].includes(os) || !['amd64', 'arm64'].includes(arch)) {
    throw new Error(`unsupported platform: ${goos}/${goarch}`)
  }
  return `purser-hook-${os}-${arch}${os === 'windows' ? '.exe' : ''}`
}

export function buildReadEvent(
  toolEvent: Record<string, unknown>,
  sessionId: string,
  cwd: string,
): Record<string, unknown> {
  const { tool: _tool, tool_use_id: _id, agentId: _agent, transcript_path, ...tool_input } = toolEvent
  return {
    session_id: sessionId,
    cwd,
    ...(typeof transcript_path === 'string' ? { transcript_path } : {}),
    tool_name: 'Read',
    tool_input,
    hook_event_name: 'PreToolUse',
  }
}
