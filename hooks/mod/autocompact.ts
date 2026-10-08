// At the end of a main-loop turn past the compaction advisor's threshold, compact
// the conversation instead of waiting for the user to type /compact. PreCompact
// refreshes the handoff note as it does for a typed /compact.

export const DEFAULT_THRESHOLD = 250_000
const RETRY_DELAYS_MS = [1_000, 3_000, 10_000]

const LOG_LINES = 40

let inFlight = false
let lines: string[] = []

export function thresholdFrom(value: string | undefined): number | undefined {
  if (value === '0' || value?.toLowerCase() === 'off') return undefined
  const n = Number(value)
  return Number.isInteger(n) && n > 0 ? n : DEFAULT_THRESHOLD
}

// The only durable trace: the last LOG_LINES decisions in ~/.claude/purser/auto-compact.log
// (fs.write replaces the file), because $.ui.log shows nothing when a hook runs late.
async function trace($, text: string) {
  try {
    const stamp = new Date(await $.clock.now()).toISOString()
    lines = [...lines, `${stamp} ${text}`].slice(-LOG_LINES)
    const os = await $.env.get('OS')
    const home = os === 'Windows_NT' ? await $.env.get('USERPROFILE') : await $.env.get('HOME')
    if (home) await $.fs.write(`${home}/.claude/purser/auto-compact.log`, `${lines.join('\n')}\n`)
  } catch { /* logging must not affect the turn */ }
}

// The engine's refusal when the host can only compact through the /compact command.
export function needsCompactPrompt(error: unknown): boolean {
  return String(error).includes('headless')
}

async function log($, text: string) {
  await trace($, text)
  try { await $.ui.log(`Purser auto-compact: ${text}`) } catch { /* as above */ }
}

export function resumeMessage(before: number, after: number | undefined): string {
  const saving = after !== undefined && after < before ? Math.round((1 - after / before) * 100) : undefined
  return [
    `> ⚓ **Context passed ${Math.floor(before / 1000)}K. Purser compacted the conversation.**`,
    ...(saving !== undefined ? [`> *Every later message is now **~${saving}%** cheaper.*`] : []),
    '> *The ship stays dry: decisions and the next step were carried over from the handoff Purser prepared. Nothing is lost.*',
    '> *Continue the task in progress; if nothing was in progress, reply with one line.*',
  ].join('\n')
}

// /compact leaves the session idle, so wake it: the next turn carries on with
// the work that was running.
async function resume($, before: number) {
  try {
    let after: number | undefined
    try { after = (await $.session.usage()).context.tokens } catch { /* message without the saving */ }
    await trace($, `submitting resume (${after ?? 'unknown'} tokens)`)
    await $.prompt.submit({ text: resumeMessage(before, after) })
    await trace($, `resumed after compact (${after ?? 'unknown'} tokens)`)
  } catch (error) {
    await log($, `resume failed: ${String(error)}`)
  }
}

// compact rejects while a turn runs; the turn that just completed may still be
// winding down, so retry a few times. Each attempt is a clock.after timer, not a
// sleep inside the hook: a timer outlives the turn.complete dispatch, a detached
// promise does not.
function compactWhenIdle($, tokens: number, attempt = 0) {
  $.clock.after(RETRY_DELAYS_MS[attempt]!, async () => {
    try {
      const result = await $.session.compact()
      await log($, result?.skip ? `skipped at ${tokens} tokens: ${result.skip}` : `compacted at ${tokens} tokens`)
      inFlight = false
    } catch (error) {
      // The headless refusal is the expected path on an SDK host, not a failure
      // to show the person; only the trace keeps it.
      const text = `attempt ${attempt + 1} failed at ${tokens} tokens: ${String(error)}`
      await (needsCompactPrompt(error) ? trace($, text) : log($, text))
      // An SDK host (the desktop Code tab, -p) compacts only through the /compact
      // command, so queue it (it runs once the session is idle) instead of
      // retrying a call that cannot succeed.
      if (needsCompactPrompt(error)) {
        $.command.run({ command: 'compact' }).then(
          async () => {
            await log($, `ran /compact at ${tokens} tokens`)
            await resume($, tokens)
          },
          (runError) => log($, `/compact failed at ${tokens} tokens: ${String(runError)}`),
        ).finally(() => { inFlight = false })
      } else if (attempt + 1 < RETRY_DELAYS_MS.length) compactWhenIdle($, tokens, attempt + 1)
      else inFlight = false
    }
  })
}

export function registerAutoCompact(on) {
  on('turn.complete', async ($, e, next) => {
    const result = await next(e)
    if (e.agentId || e.isAborted || inFlight) return result
    try {
      const threshold = thresholdFrom(await $.env.get('PURSER_AUTO_COMPACT_TOKENS'))
      if (threshold === undefined) return result
      const tokens = (await $.session.usage()).context.tokens
      const compacting = tokens !== undefined && tokens >= threshold
      await trace($, `turn ${e.turnId}: ${tokens ?? 'no'} tokens, threshold ${threshold}, ${compacting ? 'compacting' : 'below'}`)
      if (!compacting) return result
      inFlight = true
      compactWhenIdle($, tokens)
    } catch (error) {
      inFlight = false
      await log($, `check failed: ${String(error)}`)
    }
    return result
  })
}
