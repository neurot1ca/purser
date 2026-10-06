type Window = { percent: number; resetAt: number; windowMs: number }
type Quota = { fiveHour?: Window; sevenDay?: Window }
type SessionData = { quota: Quota; contextFill?: number; cost?: number }
type Pill = { id: string; label: string; value: string; detail: string; icon: string; color: string; percent?: number; tick?: number; countdown?: string }

const finite = (value: unknown): value is number => typeof value === 'number' && Number.isFinite(value)

export function formatUsageNumber(value: number): string {
  if (value >= 1_000_000) return `${(value / 1_000_000).toFixed(value >= 10_000_000 ? 1 : 2).replace(/\.?0+$/, '')}M`
  if (value >= 1_000) return `${(value / 1_000).toFixed(1).replace(/\.?0+$/, '')}k`
  return String(value)
}

export function barColor(percent: number): string {
  return percent >= 90 ? '#dc4c4c' : percent >= 70 ? '#d6a900' : '#279b69'
}

export function timeElapsedTick(timeLeft: number, windowLength: number): number {
  return Math.max(0, Math.min(1, 1 - timeLeft / windowLength))
}

const remaining = (resetAt: number, now: number) => {
  const minutes = Math.max(0, Math.ceil((resetAt - now) / 60_000))
  const days = Math.floor(minutes / 1440)
  const hours = Math.floor((minutes % 1440) / 60)
  const mins = minutes % 60
  return days ? `${days}d ${hours}h` : hours ? `${hours}h ${mins}m` : `${mins}m`
}

function rateLimit(rateLimits: any, kind: 'five_hour' | 'seven_day', windowMs: number, now: number): Window | undefined {
  const limit = Array.isArray(rateLimits) ? rateLimits.find(item => item?.kind === kind) : undefined
  const percent = limit?.percentUsed
  const resetAt = Date.parse(limit?.resetsAt)
  if (!finite(percent) || percent < 0 || percent > 100 || !Number.isFinite(resetAt) || resetAt <= now) return undefined
  return { percent, resetAt, windowMs }
}

export function mapSessionUsage(value: any, now = Date.now()): SessionData {
  const contextFill = finite(value?.context?.percent) ? value.context.percent : undefined
  const cost = finite(value?.cost?.usd) && value.cost.usd >= 0 ? value.cost.usd : undefined
  return {
    quota: {
      fiveHour: rateLimit(value?.rateLimits, 'five_hour', 5 * 60 * 60_000, now),
      sevenDay: rateLimit(value?.rateLimits, 'seven_day', 7 * 24 * 60 * 60_000, now),
    },
    contextFill,
    cost,
  }
}

export function readQuotaSnapshot(records: unknown[], now = Date.now()): Quota {
  for (const value of records.slice(0, 20)) {
    let item = value
    try { if (typeof value === 'string') item = JSON.parse(value) } catch { continue }
    const quota = (item as any)?.codex_quota
    if (!quota || typeof quota !== 'object') continue
    const window = (name: '5h' | '7d', duration: number): Window | undefined => {
      const percent = quota[`${name}_used_pct`]
      const resetAt = Date.parse(quota[`${name}_resets_at`])
      if (!finite(percent) || percent < 0 || percent > 100 || !Number.isFinite(resetAt) || resetAt <= now) return undefined
      return { percent, resetAt, windowMs: duration }
    }
    return { fiveHour: window('5h', 5 * 60 * 60_000), sevenDay: window('7d', 7 * 24 * 60 * 60_000) }
  }
  return {}
}

function quotaPills(quota: Quota, prefix: string, now: number, contextFill?: number): Pill[] {
  const pills: Pill[] = []
  for (const [name, window, icon] of [
    ['5h', quota.fiveHour, '◴'], ['7d', quota.sevenDay, '▦'],
  ] as const) {
    if (!window) continue
    const timeLeft = window.resetAt - now
    const context = contextFill === undefined ? '' : `; context ${contextFill.toFixed(1)}%`
    pills.push({
      id: `${prefix}-${name}`, label: prefix === 'codex' ? `codex ${name}` : `${icon} ${name}`,
      value: `${Math.round(window.percent)}%`,
      detail: `${prefix} ${name} quota ${window.percent.toFixed(1)}% used; resets in ${remaining(window.resetAt, now)}${context}`,
      icon: '◷', color: name === '5h' ? '#2a9d82' : '#8e63c7', percent: window.percent,
      tick: timeElapsedTick(timeLeft, window.windowMs), countdown: remaining(window.resetAt, now),
    })
  }
  return pills
}

export function usageGroups(codex: Quota, session: SessionData, now = Date.now()): Pill[][] {
  const claude = quotaPills(session.quota, 'claude', now, session.contextFill)
  if (session.contextFill !== undefined) {
    claude.push({
      id: 'claude-context', label: '◧ ctx', value: `${Math.round(session.contextFill)}%`,
      detail: `claude context ${session.contextFill.toFixed(1)}% of the window used`,
      icon: '◧', color: '#4a7fc1', percent: session.contextFill,
    })
  }
  const codexPills = quotaPills(codex, 'codex', now, session.contextFill)
  const groups: Pill[][] = [claude, codexPills]
  if (session.cost !== undefined) {
    const context = session.contextFill === undefined ? '' : `; context ${session.contextFill.toFixed(1)}%`
    groups.push([{
      id: 'cost', label: '$ session', value: `$${session.cost.toFixed(2)}`,
      detail: `$${session.cost.toFixed(2)} session cost, summed over priced API responses${context}`,
      icon: '$', color: '#bd922e',
    }])
  }
  return groups.filter(group => group.length > 0)
}

const escapeXml = (value: string) => value.replace(/[&<>"']/g, character => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&apos;' })[character]!)

// Monospace advance per character at the given font size; lays each part out after the previous one.
const textWidth = (value: string, size: number) => Math.ceil([...value].length * size * 0.62)

export function pillSvg(pill: Pill): string {
  const style = `<style>text{font-family:ui-monospace,SFMono-Regular,monospace;fill:#27313b}.pill{fill:${pill.color}22}@media(prefers-color-scheme:dark){text{fill:#f3f5f7}.pill{fill:${pill.color}55}}</style>`
  const height = 24
  const baseline = 16
  let x = 10
  let inner = `<text x="${x}" y="${baseline}" font-size="12">${escapeXml(pill.label)}</text>`
  x += textWidth(pill.label, 12) + 8
  if (pill.percent !== undefined) {
    const barWidth = 46
    inner += `<rect x="${x}" y="10" width="${barWidth}" height="4" rx="2" fill="#8c96a0" opacity=".35"/><rect x="${x}" y="10" width="${barWidth * pill.percent / 100}" height="4" rx="2" fill="${barColor(pill.percent)}"/>${pill.tick === undefined ? '' : `<path d="M${x + barWidth * pill.tick} 7v10" stroke="#252b32" stroke-width="1.4"/>`}`
    x += barWidth + 8
  }
  inner += `<text x="${x}" y="${baseline}" font-size="12" font-weight="700">${escapeXml(pill.value)}</text>`
  x += textWidth(pill.value, 12)
  if (pill.countdown) {
    x += 7
    inner += `<path d="M${x} 5v14" stroke="#66717c" opacity=".5"/>`
    x += 7
    inner += `<text x="${x}" y="${baseline}" font-size="11">${escapeXml(pill.icon)} ${escapeXml(pill.countdown)}</text>`
    x += textWidth(`${pill.icon} ${pill.countdown}`, 11)
  }
  const width = x + 10
  return `<svg xmlns="http://www.w3.org/2000/svg" width="${width}" height="${height}" viewBox="0 0 ${width} ${height}">${style}<title>${escapeXml(pill.detail)}</title><rect class="pill" x=".5" y=".5" width="${width - 1}" height="${height - 1}" rx="${(height - 1) / 2}"/>${inner}</svg>`
}

async function readCodexQuota($, home: string, now: number): Promise<Quota> {
  try {
    const dir = `${home}/.claude/purser/bundle_artifacts`
    const files = (await $.fs.list(dir)).filter(item => item.kind === 'file' && /^[0-9a-f]{64}\.json$/.test(item.name))
      .sort((a, b) => b.mtimeMs - a.mtimeMs).slice(0, 20)
    const records = []
    for (const file of files) {
      const content = await $.fs.read(`${dir}/${file.name}`)
      records.push(content)
      try { if (JSON.parse(content)?.codex_quota) break } catch { /* Try the next artifact. */ }
    }
    return readQuotaSnapshot(records, now)
  } catch { return {} }
}

export function terminalBar(pill: Pill): string | undefined {
  if (pill.percent === undefined) return undefined
  const width = 10
  const full = Math.round(width * pill.percent / 100)
  const tick = pill.tick === undefined ? -1 : Math.round(width * pill.tick)
  return Array.from({ length: width }, (_, i) => i === tick ? '│' : i < full ? '█' : '░').join('')
}

function terminalPill(Text, pill: Pill) {
  const bar = terminalBar(pill)
  return Text({ color: pill.color, children: [pill.label, ' ', ...(bar ? [bar, ' '] : []), pill.value, ...(pill.countdown ? [' │ ◷ ', pill.countdown] : [])] })
}

let timerStarted = false

export function registerUsage(on) {
  on('session.measure', async ($, e, next) => {
    const result = await next(e)
    $.ui.invalidate('ui.render')
    return result
  })
  on('ui.render', { component: 'AbovePrompt' }, async ($, e, next) => {
    if (e.props.hasSurvey) return next(e)
    const prior = await next(e)
    try {
      if (!timerStarted) {
        timerStarted = true
        $.clock.every(5000, () => $.ui.invalidate('ui.render'))
      }
      const [os, usage] = await Promise.all([$.env.get('OS'), $.session.usage()])
      const home = os === 'Windows_NT' ? await $.env.get('USERPROFILE') : await $.env.get('HOME')
      const session = mapSessionUsage(usage)
      const codex = home ? await readCodexQuota($, home, Date.now()) : {}
      const groups = usageGroups(codex, session)
      if (!groups.length) return prior
      const { Box, Text, Svg } = $.ui.resolve(e)
      const terminal = e.surface === 'terminal' || !Svg
      const rendered = groups.map((group, groupIndex) => Box({
        key: `purser-usage-group-${groupIndex}`,
        flexDirection: 'row', gap: 1,
        children: group.map(pill => terminal ? terminalPill(Text, pill) : Svg({ key: pill.id, source: pillSvg(pill), alt: pill.detail })),
      }))
      const band = Box({ flexDirection: 'row', flexWrap: 'wrap', gap: terminal ? 3 : 1, children: rendered })
      return Box({ children: prior ? [prior, band] : [band] })
    } catch { return prior }
  })
}
