import { platformBinary } from './read.js'

// When Codex work is held for quota, Purser records a wake request with the
// quota reset time; this wakes the idle session at that time so the held work
// resumes without a user message.

// The visible wake line is language-neutral (approved English master); the
// instructions ride along as hidden context.
export const QUOTA_WAKE_MARKER = '⚓ Codex quota renewed ✓'

export function quotaWakeContext(taskId: string | undefined): string {
  return `Purser submitted "${QUOTA_WAKE_MARKER}" because the Codex quota reset time has passed. Resume the Codex work that was held for quota${taskId ? ` (task ${taskId})` : ''} through normal Purser routing; do not repeat the marker line. If nothing is held, stop.`
}

type Wake = { pending: boolean; reset_at?: string; task_id?: string }

// $ cannot cross a module import, so the helper path is resolved here.
async function helper($): Promise<string> {
  const os = await $.env.get('OS')
  const ostype = (await $.env.get('OSTYPE')) ?? ''
  const arch = (await $.env.get('PROCESSOR_ARCHITECTURE')) ?? (await $.env.get('HOSTTYPE')) ?? ''
  const goos = os === 'Windows_NT' ? 'windows' : /darwin/i.test(ostype) ? 'darwin' : 'linux'
  const goarch = /^(arm64|aarch64)$/i.test(arch) ? 'arm64' : 'amd64'
  return `${$.plugin.root}/bin/${platformBinary(goos, goarch)}`
}

export async function readWake($, cwd: string): Promise<Wake | undefined> {
  const result = await $.process.run([await helper($), 'quota-wake', '--cwd', cwd], { cwd, timeoutMs: 10_000 })
  if (result.exitCode !== 0) return undefined
  try { return JSON.parse(result.stdout) as Wake } catch { return undefined }
}

// The reset time a timer is already set for, so each request is timed once.
let scheduledFor: string | undefined
let pendingTask: string | undefined | null = null

async function schedule($, cwd: string) {
  const wake = await readWake($, cwd)
  if (!wake?.pending || !wake.reset_at || wake.reset_at === scheduledFor) return
  const resetAt = Date.parse(wake.reset_at)
  if (!Number.isFinite(resetAt)) return
  scheduledFor = wake.reset_at
  // A minute of slack so the provider has really renewed the quota.
  const delay = Math.max(0, resetAt - (await $.clock.now()) + 60_000)
  $.clock.after(delay, async () => {
    try {
      const current = await readWake($, cwd)
      if (!current?.pending || current.reset_at !== wake.reset_at) return
      pendingTask = current.task_id
      await $.prompt.submit({ text: QUOTA_WAKE_MARKER })
    } catch (error) {
      try { await $.ui.log(`Purser quota wake failed: ${String(error)}`) } catch { /* no log surface */ }
    } finally {
      scheduledFor = undefined
    }
  })
}

// Matchers keep these hooks distinct from the auto-compact ones on the same events.
export function registerQuotaWake(on) {
  on('prompt.submit', { text: QUOTA_WAKE_MARKER }, async ($, e, next) => {
    if (pendingTask === null || e.origin?.kind !== 'plugin') return next(e)
    const context = quotaWakeContext(pendingTask)
    pendingTask = null
    return next({ ...e, context: [...(e.context ?? []), context] })
  })
  on('turn.complete', { isAborted: false }, async ($, e, next) => {
    const result = await next(e)
    if (!e.agentId) {
      try { await schedule($, await $.session.cwd()) } catch { /* no helper: nothing to wake */ }
    }
    return result
  })
}
