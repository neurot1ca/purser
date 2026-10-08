// At the end of a main-loop turn past the compaction advisor's threshold, compact
// the conversation instead of waiting for the user to type /compact. PreCompact
// refreshes the handoff note as it does for a typed /compact.

export const DEFAULT_THRESHOLD = 250_000
const RETRY_DELAYS_MS = [1_000, 3_000, 10_000]

let inFlight = false

export function thresholdFrom(value: string | undefined): number | undefined {
  if (value === '0' || value?.toLowerCase() === 'off') return undefined
  const n = Number(value)
  return Number.isInteger(n) && n > 0 ? n : DEFAULT_THRESHOLD
}

async function log($, text: string) {
  try { await $.ui.log(`Purser auto-compact: ${text}`) } catch { /* logging must not affect the turn */ }
}

// compact rejects while a turn runs; the turn that just completed may still be
// winding down, so wait briefly and retry a few times.
async function compactWhenIdle($, tokens: number) {
  try {
    for (const delay of RETRY_DELAYS_MS) {
      await $.clock.sleep(delay)
      try {
        const result = await $.session.compact()
        await log($, result?.skip ? `skipped at ${tokens} tokens: ${result.skip}` : `compacted at ${tokens} tokens`)
        return
      } catch (error) {
        await log($, `attempt failed at ${tokens} tokens: ${String(error)}`)
      }
    }
  } finally {
    inFlight = false
  }
}

export function registerAutoCompact(on) {
  on('turn.complete', async ($, e, next) => {
    const result = await next(e)
    if (e.agentId || e.isAborted || inFlight) return result
    try {
      const threshold = thresholdFrom(await $.env.get('PURSER_AUTO_COMPACT_TOKENS'))
      if (threshold === undefined) return result
      const tokens = (await $.session.usage()).context.tokens
      if (tokens === undefined || tokens < threshold) return result
      inFlight = true
      compactWhenIdle($, tokens).catch(() => { inFlight = false })
    } catch (error) {
      inFlight = false
      await log($, `check failed: ${String(error)}`)
    }
    return result
  })
}
