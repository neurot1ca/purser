type ToolSchema = {
  name: string
  description: string
  inputSchema: {
    type: 'object'
    properties: Record<string, Record<string, unknown>>
    required: string[]
  }
}

const baseProperties = {
  prompt: { type: 'string' },
  label: { type: 'string', description: "Short human task description in the user's language (up to 80 characters)." },
  model: { type: 'string' },
  cwd: { type: 'string' },
  config: { type: 'object' },
  'developer-instructions': { type: 'string' },
  'base-instructions': { type: 'string' },
  'compact-prompt': { type: 'string' },
  sandbox: { type: 'string', enum: ['read-only', 'workspace-write', 'danger-full-access'] },
  'approval-policy': { type: 'string', enum: ['on-request', 'never'] },
}

export const CODEX_TOOL_SCHEMAS: ToolSchema[] = [
  { name: 'codex', description: 'Run a new Codex thread.', inputSchema: { type: 'object', properties: baseProperties, required: ['prompt'] } },
  { name: 'codex-reply', description: 'Continue a thread created by this server.', inputSchema: { type: 'object', properties: {
    threadId: { type: 'string' }, conversationId: { type: 'string', description: 'Deprecated alias for threadId.' },
    prompt: { type: 'string' }, label: { type: 'string', description: "Short human task description in the user's language (up to 80 characters); may be omitted to reuse the thread label." },
  }, required: ['prompt'] } },
  { name: 'codex_start', description: 'Start a durable Codex job.', inputSchema: { type: 'object', properties: {
    ...baseProperties, call: { type: 'string' }, mode: { type: 'string', enum: ['new', 'reply'] }, worktree: { type: 'boolean' },
    threadId: { type: 'string' }, timeoutSeconds: { type: 'number' }, taskId: { type: 'string' }, route: { type: 'string' },
    etaMinutes: { type: 'integer' }, jobModel: { type: 'string' }, jobEffort: { type: 'string' }, jobLabel: { type: 'string' },
    jobPhase: { type: 'string' }, readinessCriteria: { type: 'object' }, readinessGrounded: { type: 'boolean' },
    readinessAgent: { type: 'string' }, readinessModel: { type: 'string' },
  }, required: ['prompt'] } },
  { name: 'codex_result', description: 'Read a durable Codex job.', inputSchema: { type: 'object', properties: {
    jobId: { type: 'string' }, adopt: { type: 'boolean', description: 'After user consent, finalize a job from an earlier session in this project.' },
  }, required: ['jobId'] } },
  { name: 'codex_cancel', description: 'Cancel a durable Codex job.', inputSchema: { type: 'object', properties: {
    jobId: { type: 'string' }, reason: { type: 'string' },
  }, required: ['jobId'] } },
  { name: 'codex_apply', description: 'Apply a job worktree patch.', inputSchema: { type: 'object', properties: {
    jobId: { type: 'string' }, resolved: { type: 'boolean', description: 'Record a hand-resolved conflict after verifying the repository.' },
    reviewer1Valid: { type: 'integer', minimum: 0 }, reviewer1False: { type: 'integer', minimum: 0 },
  }, required: ['jobId'] } },
]

type LegacyMcpResult = {
  content: [{ type: 'text'; text: string }]
  structuredContent?: Record<string, unknown>
  isError: boolean
}

// Python json.dumps(..., ensure_ascii=False) uses spaces after separators.
function pythonJson(value: unknown): string {
  const compact = JSON.stringify(value)
  if (compact === undefined) throw new TypeError('value is not JSON serializable')
  let result = ''
  let quoted = false
  let escaped = false
  for (const char of compact) {
    if (quoted) {
      result += char
      if (escaped) escaped = false
      else if (char === '\\') escaped = true
      else if (char === '"') quoted = false
    } else if (char === '"') {
      quoted = true
      result += char
    } else {
      result += char === ',' || char === ':' ? `${char} ` : char
    }
  }
  return result
}

export function renderCodexToolResult(input: {
  thread_id: string | null
  content?: string | null
  error?: string | null
  usage?: Record<string, unknown> | null
}): LegacyMcpResult {
  const failed = input.error !== undefined && input.error !== null
  const value: Record<string, unknown> = failed
    ? { threadId: input.thread_id, error: input.error }
    : { threadId: input.thread_id, content: input.content ?? null }
  if (!failed && input.usage && Object.keys(input.usage).length) value.usage = input.usage
  return {
    content: [{ type: 'text', text: pythonJson(value) }],
    ...(failed ? {} : { structuredContent: value }),
    isError: failed,
  }
}

export function renderCodexJobResult(value: Record<string, unknown>): LegacyMcpResult {
  return {
    content: [{ type: 'text', text: pythonJson(value) }],
    structuredContent: value,
    isError: Object.hasOwn(value, 'errorCode'),
  }
}
