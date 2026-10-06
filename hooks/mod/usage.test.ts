import { expect, mock, test } from 'claude-code/testing'
import { barColor, formatUsageNumber, mapSessionUsage, pillSvg, readCodexModel, readQuotaSnapshot, registerUsage, terminalBar, timeElapsedTick, usageGroups } from './usage.js'

const now = Date.parse('2026-10-04T12:00:00Z')
const quotaRecord = (five = 69, seven = 90) => ({ codex_quota: {
  '5h_used_pct': five, '5h_resets_at': '2026-10-04T14:30:00Z',
  '7d_used_pct': seven, '7d_resets_at': '2026-10-05T19:00:00Z', snapshot_at: '2026-10-04T11:00:00Z',
} })
const usage = {
  startedAt: now - 60_000,
  context: { tokens: 15_600, window: 200_000, percent: 42.5 },
  rateLimits: [
    { kind: 'five_hour', percentUsed: 20, resetsAt: '2026-10-04T14:40:00Z' },
    { kind: 'seven_day', percentUsed: 30, resetsAt: '2026-10-05T19:00:00Z' },
  ],
  cost: { usd: 4.32 },
}

test('formats compact counts and applies all quota bar thresholds', () => {
  expect(formatUsageNumber(15_600)).toBe('15.6k')
  expect(formatUsageNumber(954_200)).toBe('954.2k')
  expect(formatUsageNumber(1_250_000)).toBe('1.25M')
  expect([69, 70, 89, 90].map(barColor)).toEqual(['#279b69', '#d6a900', '#d6a900', '#dc4c4c'])
})

test('places the time-elapsed marker from window elapsed time', () => {
  expect(timeElapsedTick(2.5 * 60 * 60_000, 5 * 60 * 60_000)).toBe(0.5)
  expect(timeElapsedTick(10, 5)).toBe(0)
  expect(timeElapsedTick(-5, 5)).toBe(1)
})

test('maps session usage fields and keeps Claude, Codex, then cost groups', () => {
  const session = mapSessionUsage(usage, now)
  const groups = usageGroups(readQuotaSnapshot([quotaRecord()], now), session, now)
  expect(session.contextFill).toBe(42.5)
  expect(session.cost).toBe(4.32)
  expect(groups.map(group => group.map(pill => pill.id))).toEqual([
    ['claude-5h', 'claude-7d', 'claude-context'], ['codex-5h', 'codex-7d'], ['cost'],
  ])
  expect(groups[0][0].detail).toContain('context 42.5%')
  expect(groups[0][2]).toMatchObject({ value: '43%', percent: 42.5 })
  expect(terminalBar(groups[0][2])).toBe('████░░░░░░')
  expect(groups[2][0].detail).toContain('summed over priced API responses')
  expect(groups.flat().some(pill => ['input', 'output', 'cache'].includes(pill.id))).toBe(false)
})

test('hides cost and Claude quota pills when session usage omits their sources', () => {
  const session = mapSessionUsage({ context: { window: 200_000 }, rateLimits: [] }, now)
  expect(session.cost).toBeUndefined()
  expect(session.contextFill).toBeUndefined()
  expect(session.quota).toEqual({ fiveHour: undefined, sevenDay: undefined })
  expect(usageGroups({}, session, now)).toEqual([])
})

test('selects the newest Codex artifact with quota and stops at twenty files', () => {
  const found = readQuotaSnapshot([{}, JSON.stringify(quotaRecord(55, 65)), quotaRecord(75, 85)], now)
  expect(found.fiveHour?.percent).toBe(55)
  expect(readQuotaSnapshot(Array.from({ length: 20 }, () => ({})).concat([quotaRecord()]), now)).toEqual({})
})

test('uses a dark theme media query and an interactive title in each SVG', () => {
  const pill = usageGroups({}, mapSessionUsage(usage, now), now)[0][0]
  const svg = pillSvg(pill)
  expect(svg).toContain('@media(prefers-color-scheme:dark)')
  expect(svg).toContain('<title>')
  expect(svg).toContain('2h 40m')
  expect(svg).toContain('stroke-width="1.4"')
})

test('steps aside for surveys before reading usage or artifacts', async ($, on) => {
  mock.clock(on)
  let read = false
  on('session.usage', () => { read = true; return { deny: 'unexpected usage read' } })
  on('fs.list', () => { read = true; return { deny: 'unexpected read' } })
  on('ui.render', { component: 'AbovePrompt' }, ($, e) => {
    const { Text } = $.ui.resolve(e)
    return Text({ children: 'base prompt' })
  })
  registerUsage(on)
  const ui = await $.ui.mount({
    plugin: 'usage-survey-test', surface: 'terminal', component: 'AbovePrompt',
    props: { hasSurvey: true, isWorking: false, maxRows: 8, bodyColumns: 80, scroll: { offset: 0, bodyRows: 8 }, view: {} },
  })
  expect(read).toBe(false)
  await ui.unmount()
})

test('draws valid Svg pills on desktop and keeps another mod drawing', async ($, on) => {
  mock.clock(on)
  mock.env(on, { OS: 'Linux', HOME: '/home/test' })
  on('session.usage', () => ({ value: usage }))
  on('fs.list', () => ({ deny: 'ENOENT' }))
  on('ui.render', { component: 'AbovePrompt' }, ($, e) => {
    const { Text } = $.ui.resolve(e)
    return Text({ children: 'Other mod status' })
  })
  registerUsage(on)
  const ui = await $.ui.mount({
    plugin: 'usage-desktop-test', surface: 'desktop', component: 'AbovePrompt',
    props: { hasSurvey: false, isWorking: false, maxRows: 8, bodyColumns: 100, scroll: { offset: 0, bodyRows: 8 }, view: {} },
  })
  expect((await ui.find({ type: 'Text', text: 'Other mod status' }))?.text).toBe('Other mod status')
  const svg = await ui.find({ type: 'Svg' })
  expect(svg?.props.source.startsWith('<svg')).toBe(true)
  expect(svg?.props.alt.length > 0).toBe(true)
  await ui.unmount()
})

test('keeps another mod drawing and uses the terminal fallback', async ($, on) => {
  mock.clock(on)
  mock.env(on, { OS: 'Linux', HOME: '/home/test' })
  on('session.usage', () => ({ value: usage }))
  on('fs.list', () => ({ deny: 'ENOENT' }))
  on('ui.render', { component: 'AbovePrompt' }, ($, e) => {
    const { Text } = $.ui.resolve(e)
    return Text({ children: 'Other mod status' })
  })
  registerUsage(on)
  const ui = await $.ui.mount({
    plugin: 'usage-terminal-test', surface: 'terminal', component: 'AbovePrompt',
    props: { hasSurvey: false, isWorking: false, maxRows: 8, bodyColumns: 100, scroll: { offset: 0, bodyRows: 8 }, view: {} },
  })
  expect((await ui.find({ type: 'Text', text: 'Other mod status' }))?.text).toBe('Other mod status')
  expect(await ui.find({ type: 'Svg' })).toBeUndefined()
  expect(await ui.find({ type: 'Text', text: /5h/ })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /\$4.32/ })).toBeDefined()
  const claude = usageGroups({}, mapSessionUsage(usage, now), now)[0][0]
  expect(terminalBar(claude)).toContain('│')
  expect(terminalBar(claude)).toContain('█')
  expect(terminalBar(claude)).toContain('░')
  await ui.unmount()
})

test('reads Codex quota from the newest artifact that contains it', async ($, on) => {
  mock.clock(on)
  mock.env(on, { OS: 'Linux', HOME: '/home/test' })
  on('session.usage', () => ({ value: { context: { window: 200_000 }, rateLimits: [] } }))
  const newest = `${'a'.repeat(64)}.json`
  const prior = `${'b'.repeat(64)}.json`
  const reads: string[] = []
  on('fs.list', () => ({ value: [
    { name: newest, kind: 'file', size: 50, mtimeMs: 20, isLink: false },
    { name: prior, kind: 'file', size: 50, mtimeMs: 10, isLink: false },
  ] }))
  on('fs.read', (_$, e) => {
    reads.push(e.path)
    return { value: e.path.endsWith(newest) ? '{"incomplete":true}' : JSON.stringify(quotaRecord(55, 65)) }
  })
  on('ui.render', { component: 'AbovePrompt' }, ($, e) => {
    const { Text } = $.ui.resolve(e)
    return Text({ children: 'base prompt' })
  })
  registerUsage(on)
  const ui = await $.ui.mount({
    plugin: 'usage-codex-test', surface: 'terminal', component: 'AbovePrompt',
    props: { hasSurvey: false, isWorking: false, maxRows: 8, bodyColumns: 100, scroll: { offset: 0, bodyRows: 8 }, view: {} },
  })
  expect(reads.map(path => path.split(/[\\/]/).at(-1))).toEqual([newest, prior])
  expect(await ui.find({ type: 'Text', text: /codex 5h/ })).toBeDefined()
  await ui.unmount()
})

test('shows the latest Codex model and effort as the first Codex pill', () => {
  const records = [JSON.stringify({ model: 'gpt-6-luna', effort: 'high' }), quotaRecord()]
  expect(readCodexModel(records)).toEqual({ model: 'gpt-6-luna', effort: 'high' })
  expect(readCodexModel(['not json', { model: '' }])).toEqual({})
  const groups = usageGroups({ ...readQuotaSnapshot([quotaRecord()], now), ...readCodexModel(records) }, mapSessionUsage(usage, now), now)
  expect(groups[1][0]).toMatchObject({ id: 'codex-model', value: 'gpt-6-luna · high' })
  expect(terminalBar(groups[1][0])).toBeUndefined()
})
