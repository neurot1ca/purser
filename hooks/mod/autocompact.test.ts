import { expect, mock, test } from 'claude-code/testing'
import { DEFAULT_THRESHOLD, thresholdFrom } from './autocompact.js'

const TURN = { answer: 'ok', durationMs: 10, isAborted: false, turnId: 't1', reason: 'answer' } as const

function engine(on, tokens: number, env: Record<string, string> = {}) {
  const calls = { compacts: 0 }
  const clock = mock.clock(on)
  mock.env(on, env)
  on('turn.complete', async () => ({ text: 'ok' }))
  on('session.usage', async () => ({ value: { startedAt: 0, context: { tokens, window: 1_000_000 }, rateLimits: [] } }))
  on('session.compact', async (_$, e) => {
    calls.compacts++
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
