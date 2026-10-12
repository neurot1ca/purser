// At the end of a main-loop turn past the compaction advisor's threshold, compact
// the conversation instead of waiting for the user to type /compact. PreCompact
// refreshes the handoff note as it does for a typed /compact.

import { stateRoot } from './state-root.js'

async function runtimeStateRoot($): Promise<string | undefined> {
  const os = await $.env.get('OS')
  const [purserState, claudeConfig, home] = await Promise.all([
    $.env.get('PURSER_STATE_DIR'),
    $.env.get('CLAUDE_CONFIG_DIR'),
    os === 'Windows_NT' ? $.env.get('USERPROFILE') : $.env.get('HOME'),
  ])
  return stateRoot({ purserState, claudeConfig, home })
}

export const DEFAULT_THRESHOLD = 250_000
const RETRY_DELAYS_MS = [1_000, 3_000, 10_000]

const LOG_LINES = 40

let inFlight = false
let lines: string[] = []
// The size the last main-conversation compaction left, from its own result:
// usage() cannot report it right after a /compact command.
let compactedTokens: number | undefined

export function thresholdFrom(value: string | undefined): number | undefined {
  if (value === '0' || value?.toLowerCase() === 'off') return undefined
  const n = Number(value)
  return Number.isInteger(n) && n > 0 ? n : DEFAULT_THRESHOLD
}

// The only durable trace: the last LOG_LINES decisions in the state root's auto-compact.log
// (fs.write replaces the file), because $.ui.log shows nothing when a hook runs late.
async function trace($, text: string) {
  try {
    const stamp = new Date(await $.clock.now()).toISOString()
    lines = [...lines, `${stamp} ${text}`].slice(-LOG_LINES)
    const root = await runtimeStateRoot($)
    if (root) {
      await $.fs.write(`${root.replace(/[\\/]+$/, '')}/auto-compact.log`, `${lines.join('\n')}\n`)
    }
  } catch { /* logging must not affect the turn */ }
}

// Off unless /purser:init recorded the user's yes in the shared state root.
export async function optedIn($): Promise<boolean> {
  try {
    const root = await runtimeStateRoot($)
    return root !== undefined && JSON.parse(await $.fs.read(`${root.replace(/[\\/]+$/, '')}/auto-compact.json`))?.enabled === true
  } catch {
    return false
  }
}

// The engine's refusal when the host can only compact through the /compact command.
export function needsCompactPrompt(error: unknown): boolean {
  return String(error).includes('headless')
}

async function log($, text: string) {
  await trace($, text)
  try { await $.ui.log(`Purser auto-compact: ${text}`) } catch { /* as above */ }
}

// The visible wake line is language-neutral; the explanation rides along as
// hidden context, and the model renders the notice in the user's language.
export const WAKE_MARKER = '⚓ /compact ✓'

// The English master of the notice the woken turn writes, plus what to do next.
export function compactNotice(before: number, after: number | undefined): string {
  const saving = after !== undefined && after < before ? Math.round((1 - after / before) * 100) : undefined
  return `> ⚓ **Context passed ${Math.floor(before / 1000)}K, so Purser ran /compact.** *${saving !== undefined ? `Every later message is now ~${saving}% cheaper; d` : 'D'}ecisions and the next step were carried over from the handoff Purser prepared. Nothing is lost.*`
}

export const RESUME_PROMPT = 'Continue the task in progress; if nothing was in progress, stop after that line.'

export function wakeContext(notice: string): string {
  return `Purser's automatic /compact just ran and submitted "${WAKE_MARKER}" to wake this session. Tell the user in the user's language exactly one line, translated from: "${notice}" ${RESUME_PROMPT}`
}

// The context for the next wake marker Purser submits.
let pendingWake: string | undefined

// /compact leaves the session idle, so wake it: the next turn carries on with
// the work that was running.
async function resume($, before: number) {
  try {
    let after = compactedTokens
    compactedTokens = undefined
    if (after === undefined) {
      try { after = (await $.session.usage()).context.tokens } catch { /* message without the saving */ }
    }
    await trace($, `submitting resume (${after ?? 'unknown'} tokens)`)
    pendingWake = wakeContext(compactNotice(before, after))
    await $.prompt.submit({ text: WAKE_MARKER })
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
  on('prompt.submit', async ($, e, next) => {
    if (pendingWake === undefined || e.text !== WAKE_MARKER || e.origin?.kind !== 'plugin') return next(e)
    const context = pendingWake
    pendingWake = undefined
    return next({ ...e, context: [...(e.context ?? []), context] })
  })
  on('session.compact', async ($, e, next) => {
    const result = await next(e)
    if (!e.agentId && e.trigger !== 'precompute' && result && 'tokensAfter' in result) compactedTokens = result.tokensAfter
    return result
  })
  on('turn.complete', async ($, e, next) => {
    const result = await next(e)
    if (e.agentId || e.isAborted || inFlight) return result
    try {
      const threshold = thresholdFrom(await $.env.get('PURSER_AUTO_COMPACT_TOKENS'))
      if (threshold === undefined || !(await optedIn($))) return result
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
