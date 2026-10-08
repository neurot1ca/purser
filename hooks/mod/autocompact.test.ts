import { expect, mock, test } from 'claude-code/testing'
import { DEFAULT_THRESHOLD, needsCompactPrompt, thresholdFrom } from './autocompact.js'

const TURN = { answer: 'ok', durationMs: 10, isAborted: false, turnId: 't1', reason: 'answer' } as const

function engine(on, tokens: number, env: Record<string, string> = {}, failures = 0) {
  const calls = { compacts: 0 }
  const clock = mock.clock(on)
  mock.env(on, env)
  on('turn.complete', async () => ({ text: 'ok' }))
  on('session.usage', async () => ({ value: { startedAt: 0, context: { tokens, window: 1_000_000 }, rateLimits: [] } }))
  on('session.compact', async (_$, e) => {
    calls.compacts++
    if (calls.compacts <= failures) throw new Error('a turn is running')
    return { messages: e.messages }
  })
  return { calls, clock }
}

test('reads the threshold override and the off switch', () => {
  expect(thresholdFrom(undefined)).toBe(DEFAULT_THRESHOLD)
  expect(thresholdFrom('120000')).toBe(120_000)
  expect(thresholdFrom('junk')).toBe(DEFAULT_THRESHOLD)
  expect(thresholdFrom('0')).toBe(undefined)
  expect(thresholdFrom('off')).toBe(undefined)
})

test('compacts once after a main turn past the threshold', async ($, on) => {
  const { calls, clock } = engine(on, 260_000)
  await $.turn.complete(TURN)
  expect(calls.compacts).toBe(0)
  await clock.advance(1_000)
  expect(calls.compacts).toBe(1)
})

test('leaves turns under the threshold alone', async ($, on) => {
  const { calls, clock } = engine(on, 240_000)
  await $.turn.complete(TURN)
  await clock.advance(20_000)
  expect(calls.compacts).toBe(0)
})

test('the off switch disables it', async ($, on) => {
  const { calls, clock } = engine(on, 900_000, { PURSER_AUTO_COMPACT_TOKENS: '0' })
  await $.turn.complete(TURN)
  await clock.advance(20_000)
  expect(calls.compacts).toBe(0)
})

test('retries a compact rejected while the turn winds down', async ($, on) => {
  const { calls, clock } = engine(on, 260_000, {}, 2)
  await $.turn.complete(TURN)
  await clock.advance(1_000)
  expect(calls.compacts).toBe(1)
  await clock.advance(3_000)
  expect(calls.compacts).toBe(2)
  await clock.advance(10_000)
  expect(calls.compacts).toBe(3)
})

test('keeps trying on later turns after every retry failed', async ($, on) => {
  const { calls, clock } = engine(on, 260_000, {}, 3)
  await $.turn.complete(TURN)
  await clock.advance(14_000)
  expect(calls.compacts).toBe(3)
  await $.turn.complete(TURN)
  await clock.advance(1_000)
  expect(calls.compacts).toBe(4)
})

test('writes each main turn decision to the auto-compact log', async ($, on) => {
  const { clock } = engine(on, 260_000, { USERPROFILE: 'C:/home', OS: 'Windows_NT' })
  const writes: Record<string, string> = {}
  on('fs.write', async (_$, e) => {
    writes[e.path] = e.text
    return { value: undefined }
  })
  await $.turn.complete(TURN)
  await clock.advance(1_000)
  const log = Object.entries(writes).find(([path]) => path.endsWith('auto-compact.log'))?.[1]
  expect(log).toContain('260000 tokens, threshold 250000, compacting')
  expect(log).toContain('at 260000 tokens')
})

test('recognises the SDK-host refusal that needs a /compact prompt', () => {
  expect(needsCompactPrompt(new Error('HooksError: purser: $.session.compact: not available in a headless (-p / SDK) session yet'))).toBe(true)
  expect(needsCompactPrompt(new Error('a turn is running'))).toBe(false)
})
