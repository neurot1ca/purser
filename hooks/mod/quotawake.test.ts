import { expect, mock, test } from 'claude-code/testing'
import { QUOTA_WAKE_MARKER, quotaWakeContext } from './quotawake.js'

const TURN = { answer: 'ok', durationMs: 10, isAborted: false, turnId: 't1', reason: 'answer' } as const

function engine(on, wakes: Record<string, unknown>[]) {
  const clock = mock.clock(on)
  mock.env(on, { OS: 'Linux', OSTYPE: 'linux-gnu', HOSTTYPE: 'x86_64', HOME: '/home' })
  const submitted: { text: string; context?: readonly string[] }[] = []
  let reads = 0
  on('session.cwd', () => ({ value: '/repo' }))
  on('turn.complete', async () => ({ text: 'ok' }))
  on('process.run', (_$, e) => {
    if (e.argv[1] !== 'quota-wake') return { value: { exitCode: 0, stdout: '', stderr: '' } }
    const wake = wakes[Math.min(reads++, wakes.length - 1)]
    return { value: { exitCode: 0, stdout: JSON.stringify(wake), stderr: '' } }
  })
  on('prompt.submit', (_$, e) => {
    submitted.push({ text: e.text, context: e.context })
    return { text: e.text, context: e.context }
  })
  return { clock, submitted }
}

test('wakes the session once, after the quota reset', async ($, on) => {
  const wakes: Record<string, unknown>[] = []
  const { clock, submitted } = engine(on, wakes)
  wakes.push({ pending: true, reset_at: new Date(clock.now() + 3_600_000).toISOString(), task_id: 'held-task' })
  await $.turn.complete(TURN)
  await $.turn.complete(TURN)
  await clock.advance(3_600_000)
  expect(submitted.length).toBe(0)
  await clock.advance(61_000)
  expect(submitted.length).toBe(1)
  expect(submitted[0]!.text).toBe(QUOTA_WAKE_MARKER)
})

test('the hidden context names the held task and forbids repeating the marker', () => {
  expect(quotaWakeContext('held-task')).toContain('(task held-task)')
  expect(quotaWakeContext(undefined)).not.toContain('(task')
  expect(quotaWakeContext(undefined)).toContain('do not repeat the marker line')
})

test('does not wake when the request was cleared before the reset', async ($, on) => {
  const wakes: Record<string, unknown>[] = []
  const { clock, submitted } = engine(on, wakes)
  wakes.push({ pending: true, reset_at: new Date(clock.now() + 60_000).toISOString() }, { pending: false })
  await $.turn.complete(TURN)
  await clock.advance(200_000)
  expect(submitted.length).toBe(0)
})

test('nothing pending sets no timer', async ($, on) => {
  const { clock, submitted } = engine(on, [{ pending: false }])
  await $.turn.complete(TURN)
  await clock.advance(10_000_000)
  expect(submitted.length).toBe(0)
})
