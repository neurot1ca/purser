export type LifecycleEvent = 'UserPromptSubmit' | 'SessionStart' | 'PreCompact' | 'Stop'

export function mapHookOutput(event: LifecycleEvent, stdout: string): {
  additionalContext?: string[]
  block?: string
  systemMessage?: string
} {
  if (!stdout.trim()) return {}
  const output = JSON.parse(stdout)
  if (event === 'Stop' && output?.decision === 'block' && typeof output.reason === 'string' && output.reason) {
    return { block: output.reason }
  }
  if ((event === 'UserPromptSubmit' || event === 'SessionStart') &&
      output?.hookSpecificOutput?.hookEventName === event &&
      typeof output.hookSpecificOutput.additionalContext === 'string' &&
      (output.hookSpecificOutput.additionalContext || (typeof output.systemMessage === 'string' && output.systemMessage))) {
    return {
      ...(output.hookSpecificOutput.additionalContext ? { additionalContext: [output.hookSpecificOutput.additionalContext] } : {}),
      ...(typeof output.systemMessage === 'string' && output.systemMessage ? { systemMessage: output.systemMessage } : {}),
    }
  }
  throw new Error(`invalid Go ${event} output`)
}
