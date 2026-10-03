export type LifecycleEvent = 'UserPromptSubmit' | 'SessionStart' | 'PreCompact' | 'Stop'

export function mapHookOutput(event: LifecycleEvent, stdout: string): {
  additionalContext?: string[]
  block?: string
} {
  if (!stdout.trim()) return {}
  const output = JSON.parse(stdout)
  if (event === 'Stop' && output?.decision === 'block' && typeof output.reason === 'string' && output.reason) {
    return { block: output.reason }
  }
  if ((event === 'UserPromptSubmit' || event === 'SessionStart') &&
      output?.hookSpecificOutput?.hookEventName === event &&
      typeof output.hookSpecificOutput.additionalContext === 'string' &&
      output.hookSpecificOutput.additionalContext) {
    return { additionalContext: [output.hookSpecificOutput.additionalContext] }
  }
  throw new Error(`invalid Go ${event} output`)
}
