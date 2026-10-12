import { stateRoot } from './state-root.js'
import { projectKeyFromRoot } from './project-key.js'

type Job = Record<string, any>

type BoardFiles = {
  indexes: Job[]
  jobs: Job[]
  leases?: Job
  bundles?: Record<string, Job>
}

type SummaryPart = { text: string; color?: string; dimColor?: boolean }
type JobBoard = { summary: SummaryPart[]; detail: string }

const routeMinutes: Record<string, number> = {
  mechanical: 3, subtle: 5, normal: 5, consequential: 8,
  escalated: 12, operation: 3, plan_standard: 8,
}

const clean = (value: unknown) => String(value ?? '—').replace(/\|/g, '\\|').replace(/[\r\n]/g, ' ')
const finiteTime = (value: unknown): value is number => typeof value === 'number' && Number.isFinite(value)
const duration = (seconds: number) => {
  const whole = Math.floor(Math.max(0, seconds))
  const minutes = Math.floor(whole / 60)
  const remainder = whole % 60
  if (!minutes) return `${remainder}s`
  return remainder ? `${minutes}m ${remainder}s` : `${minutes}m`
}

export function renderJobBoard(files: BoardFiles, now = Date.now() / 1000, windows = false): JobBoard | undefined {
  const jobsById = new Map(files.jobs.filter(job => typeof job.jobId === 'string').map(job => [job.jobId, job]))
  const indexed = files.indexes.flatMap(index => {
    const jobId = index.jobId
    const job = typeof jobId === 'string' ? jobsById.get(jobId) : undefined
    return job && job.jobId === jobId && job.call === index.call ? [{ job, index }] : []
  })
  const labels = new Map(indexed.filter(({ job }) => typeof job.label === 'string').map(({ job }) => [job.jobId, job.label]))
  const queuedBehind = new Map<string, string>()
  const owners = files.leases?.owners
  if (owners && typeof owners === 'object') {
    const entries = Object.values(owners) as Job[]
    for (const queued of entries) {
      if (queued?.state !== 'QUEUED' || typeof queued.job_id !== 'string') continue
      for (const active of entries) {
        const overlap = Array.isArray(queued.scope) && Array.isArray(active?.scope) &&
          queued.scope.some((left: unknown) => active.scope.some((right: unknown) => {
            if (typeof left !== 'string' || typeof right !== 'string') return false
            const [a, b] = windows ? [left.toLowerCase(), right.toLowerCase()] : [left, right]
            return a === b || a.startsWith(b + '/') || b.startsWith(a + '/')
          }))
        if (active?.state === 'ACTIVE' && overlap && labels.has(active.job_id)) {
          queuedBehind.set(queued.job_id, labels.get(active.job_id)!)
          break
        }
      }
    }
  }

  const latest = new Map<string, { job: Job; index: Job }>()
  for (const item of indexed) {
    const key = typeof item.job.threadId === 'string' && item.job.threadId ? item.job.threadId : item.job.jobId
    const old = latest.get(key)
    if (!old || (Number(item.job.acceptedAt) || 0) > (Number(old.job.acceptedAt) || 0) ||
        (item.job.acceptedAt === old.job.acceptedAt && item.job.jobId > old.job.jobId)) latest.set(key, item)
  }

  const visible = [...latest.values()].filter(({ job }) => {
    if (typeof job.label !== 'string' || !job.label.trim()) return false
    if (job.state === 'QUEUED' || job.state === 'RUNNING') return true
    if (!Number.isInteger(job.boardDisplayCount) || job.boardDisplayCount >= 2) return false
    const finished = job.applyState === 'applied' && finiteTime(job.appliedAt) ? job.appliedAt : job.finishedAt
    return !finiteTime(finished) || now - finished <= 3600
  })
  const active = visible.filter(({ job }) => job.state === 'QUEUED' || job.state === 'RUNNING')
  const finished = visible.filter(({ job }) => job.state !== 'QUEUED' && job.state !== 'RUNNING')
  if (!active.length && !finished.some(({ job }) => job.boardDisplayCount === 0)) return undefined

  const applyOf = (job: Job, bundle: Job) => {
    const paths = Array.isArray(job.changed_paths) ? job.changed_paths : bundle.changed_paths
    const threadApplyCoversDelta = typeof job.threadId === 'string' && Array.isArray(paths) && paths.length > 0 &&
      paths.every((path: unknown) => typeof path === 'string') &&
      indexed.some(({ job: other }) => other.threadId === job.threadId && other.applyState === 'applied' &&
        other.applyResult?.applied === true && Array.isArray(other.applyResult.paths) &&
        other.applyResult.paths.every((path: unknown) => typeof path === 'string') &&
        paths.every((path: string) => other.applyResult.paths.includes(path)))
    let apply = job.applyState === 'applied' || threadApplyCoversDelta ? 'applied' :
      job.applyState === 'conflict' ? 'conflict' : '—'
    if (apply === '—' && (['operation', 'plan'].includes(job.phase ?? bundle.phase) || (Object.keys(bundle).length && bundle.apply !== 'codex_apply'))) apply = 'n/a'
    if (apply === '—' && !['QUEUED', 'RUNNING', 'UNKNOWN_OR_IN_PROGRESS'].includes(job.state) && Array.isArray(paths) && paths.length === 0) apply = 'no change'
    return apply
  }
  // A succeeded job is only done once Claude has taken its result and applied
  // (or had nothing to apply); before that it is still moving through review.
  const stageOf = (job: Job, bundle: Job, apply: string) => {
    if (job.state !== 'SUCCEEDED') return undefined
    if (!Object.keys(bundle).length) return apply === 'applied' ? 'done' : 'fetching'
    if (['applied', 'n/a', 'no change'].includes(apply)) return 'done'
    return job.acceptance ? 'accepted' : 'review'
  }
  const counts = { coding: 0, queued: 0, fetching: 0, review: 0, accepted: 0, done: 0, failed: 0, cancelled: 0 }
  let nearestEta: number | undefined
  let quietSeconds = 0
  for (const { job, index } of visible) {
    if (job.state === 'RUNNING' || job.state === 'UNKNOWN_OR_IN_PROGRESS') {
      counts.coding++
      const started = finiteTime(job.startedAt) ? job.startedAt : job.acceptedAt
      const expected = Number.isInteger(job.etaMinutes) ? job.etaMinutes : routeMinutes[job.route]
      if (finiteTime(started) && Number.isInteger(expected)) {
        const elapsed = Math.max(0, now - started)
        if (elapsed <= expected * 60) nearestEta = Math.min(nearestEta ?? Infinity, expected * 60 - elapsed)
      }
      const lastActivity = [job.liveness?.lastActivityAt, job.lastActivityAt, job.liveness?.checkpointAt, job.checkpointAt]
        .find(finiteTime)
      if (finiteTime(lastActivity)) quietSeconds = Math.max(quietSeconds, now - lastActivity)
    } else if (job.state === 'QUEUED') counts.queued++
    else if (job.state === 'SUCCEEDED') {
      const bundle = files.bundles?.[index.call_key] ?? {}
      counts[stageOf(job, bundle, applyOf(job, bundle))!]++
    }
    else if (job.state === 'CANCELLED') counts.cancelled++
    else counts.failed++
  }
  const summary: SummaryPart[] = [{ text: '⚓', color: 'cyan' }]
  let firstCount = true
  const addCount = (count: number, glyph: string, label: string, color: string) => {
    if (!count) return
    summary.push({ text: firstCount ? ' ' : ' · ', dimColor: true }, { text: `${glyph} ${count}`, color }, { text: ` ${label}`, dimColor: true })
    firstCount = false
  }
  addCount(counts.coding, '●', 'coding', 'cyan')
  addCount(counts.queued, '◌', 'queued', 'yellow')
  addCount(counts.fetching, '◐', 'fetching result', 'cyan')
  addCount(counts.review, '◑', 'in review', 'magenta')
  addCount(counts.accepted, '◕', 'accepted', 'green')
  addCount(counts.done, '✓', 'done', 'green')
  addCount(counts.failed, '!', 'failed', 'red')
  addCount(counts.cancelled, '−', 'cancelled', 'gray')
  if (nearestEta !== undefined) summary.push({ text: ' · ', dimColor: true }, { text: `next ${duration(nearestEta)}`, dimColor: true })
  if (quietSeconds >= 300) summary.push({ text: ' · ', dimColor: true }, { text: `quiet ${duration(quietSeconds)}`, dimColor: true })

  const rows = visible.map(({ job, index }) => {
    const raw = job.state
    const bundle = files.bundles?.[index.call_key] ?? {}
    const apply = applyOf(job, bundle)
    let state = ({ QUEUED: 'queued', RUNNING: 'coding', SUCCEEDED: 'done', FAILED: 'failed',
      TIMED_OUT: 'failed', CANCELLED: 'cancelled', UNKNOWN_OR_IN_PROGRESS: 'coding' } as Record<string, string>)[raw] ?? 'failed'
    const stage = stageOf(job, bundle, apply)
    state = ({ fetching: 'coded · fetching result', review: 'in review', accepted: 'accepted', done: 'done' } as Record<string, string>)[stage ?? ''] ?? state
    if (raw === 'QUEUED' && queuedBehind.has(job.jobId)) state = `queued behind ${queuedBehind.get(job.jobId)}`
    if (raw === 'RUNNING' && job.errorCode === 'STALLED') state = 'stalled'
    if (raw === 'RUNNING') {
      const lastActivity = [job.liveness?.lastActivityAt, job.lastActivityAt, job.liveness?.checkpointAt, job.checkpointAt]
        .find(finiteTime)
      if (finiteTime(lastActivity)) {
        const agent = typeof job.activeAgent === 'string' ? ` ${clean(job.activeAgent)}` : ''
        state += ` ·${agent} active ${Math.floor(Math.max(0, now - lastActivity))}s ago`
      }
    }

    let review = '—'
    if (bundle.reviewer1_result) review = bundle.reviewer1_result === 'REVIEWER1_UNAVAILABLE' ? 'failed' : 'reviewed'
    else if (bundle.reviewer1_skipped_reason) review = bundle.reviewer1_skipped_reason === 'REVIEWER1_UNAVAILABLE' ? 'failed' : 'skipped'
    else if (['operation', 'plan'].includes(job.phase ?? bundle.phase)) review = 'skipped'
    else if (['FULL', 'AUDIT'].includes(bundle.claude_review_mode) || ['FAILED', 'TIMED_OUT'].includes(raw)) review = 'failed'

    let eta = '—'
    if (raw === 'QUEUED' && finiteTime(job.acceptedAt)) {
      eta = `waited ${duration(now - job.acceptedAt)}`
    } else if (raw === 'RUNNING') {
      const started = finiteTime(job.startedAt) ? job.startedAt : job.acceptedAt
      if (finiteTime(started)) {
        const elapsed = Math.max(0, now - started)
        const expected = Number.isInteger(job.etaMinutes) ? job.etaMinutes : routeMinutes[job.route]
        eta = Number.isInteger(expected) && elapsed <= expected * 60
          ? `${duration(elapsed)} elapsed, ${duration(expected * 60 - elapsed)} left`
          : `${duration(elapsed)} elapsed`
      }
    }
    const model = typeof job.model === 'string' && typeof job.effort === 'string'
      ? `${clean(job.model)} (${clean(job.effort)})` : '—'
    const line = `| ${clean(job.label)} | ${model} | ${state} | ${eta} | ${review} | ${apply} |`
    const timestamp = raw === 'QUEUED' || raw === 'RUNNING' ? job.acceptedAt :
      job.applyState === 'applied' && finiteTime(job.appliedAt) ? job.appliedAt : job.finishedAt
    // Coding and post-coding stages lead, queued jobs follow, finished jobs last.
    const rank = raw === 'RUNNING' ? 0 : stage && stage !== 'done' ? 1 : raw === 'QUEUED' ? 2 : 3
    return { line, rank, timestamp: finiteTime(timestamp) ? timestamp : 0 }
  }).sort((a, b) => a.rank - b.rank || b.timestamp - a.timestamp)
  return {
    summary,
    detail: ['| ⚓ Task | Model | State | ETA | Review | Apply |',
      '| --- | --- | --- | --- | --- | --- |', ...rows.map(row => row.line)].join('\n'),
  }
}

const projectKeys = new Map<string, string>()

async function readBoard($, cwd: string): Promise<JobBoard | undefined> {
  const os = await $.env.get('OS')
  const [purserState, claudeConfig, home] = await Promise.all([
    $.env.get('PURSER_STATE_DIR'),
    $.env.get('CLAUDE_CONFIG_DIR'),
    os === 'Windows_NT' ? $.env.get('USERPROFILE') : $.env.get('HOME'),
  ])
  const runtime = stateRoot({ purserState, claudeConfig, home })
  if (!runtime) throw new Error('home directory unavailable')
  let key = projectKeys.get(cwd)
  if (key === undefined) {
    // A busy machine can take seconds to start git; the key is cached so it runs once per folder.
    const rootResult = await $.process.run(['git', 'rev-parse', '--show-toplevel'], { cwd, timeoutMs: 10_000 })
    if (rootResult.exitCode !== 0) throw new Error('project root unavailable')
    key = await projectKeyFromRoot(rootResult.stdout.trim(), os)
    projectKeys.set(cwd, key)
  }
  let entries
  try { entries = await $.fs.list(`${runtime}/async_jobs`) }
  catch (error) {
    const failure = error as { code?: unknown; message?: unknown }
    if (failure?.code === 'ENOENT' ||
        (typeof failure?.message === 'string' && /\b(?:ENOENT|no such file or directory)\b/i.test(failure.message))) {
      return undefined
    }
    throw error
  }
  const indexes: Job[] = []
  const jobs: Job[] = []
  for (const entry of entries) {
    if (entry.kind !== 'file' || !/^[0-9a-f]{64}\.json$/.test(entry.name)) continue
    try {
      const index = JSON.parse(await $.fs.read(`${runtime}/async_jobs/${entry.name}`))
      if (index?.project_key !== key || typeof index.jobId !== 'string' || !/^[0-9a-f]{64}$/.test(index.jobId)) continue
      const job = JSON.parse(await $.fs.read(`${runtime}/codex_exec_mcp/jobs/${index.jobId}.json`))
      if (job?.jobId !== index.jobId || job.call !== index.call) continue
      indexes.push(index)
      jobs.push(job)
    } catch { /* One missing or malformed record does not hide the other jobs. */ }
  }
  let leases: Job | undefined
  try { leases = JSON.parse(await $.fs.read(`${runtime}/projects/${key}/scope_leases.json`)) }
  catch { /* Queued jobs remain visible without lease details. */ }
  const bundles: Record<string, Job> = {}
  for (const index of indexes) {
    if (typeof index.call_key !== 'string') continue
    try { bundles[index.call_key] = JSON.parse(await $.fs.read(`${runtime}/bundle_artifacts/${index.call_key}.json`)) }
    catch { /* Review and apply fall back to the job record. */ }
  }
  return renderJobBoard({ indexes, jobs, leases, bundles }, Date.now() / 1000, os === 'Windows_NT')
}

let timerStarted = false
const reported = new Set<string>()
const detailsPaneId = 'purser-job-board'
let latestBoard: JobBoard | undefined

async function reportOnce($, message: string) {
  const sessionId = await $.session.id().catch(() => '<unknown>')
  if (reported.has(sessionId)) return
  reported.add(sessionId)
  try { await $.ui.log(message) } catch { /* Observability must not block the hook. */ }
}

export function registerBoard(on) {
  on('ui.render', { component: 'AbovePrompt' }, async ($, e, next) => {
    const prior = await next(e)
    if (e.props.hasSurvey) return prior
    try {
      if (!timerStarted) {
        timerStarted = true
        $.clock.every(5000, () => $.ui.invalidate('ui.render'))
      }
      const target = await $.session.cwd()
      let board: JobBoard | undefined
      try {
        board = await readBoard($, target)
        latestBoard = board
      } catch {
        // A failed refresh keeps the last board on screen instead of hiding it.
        await reportOnce($, 'Purser job board refresh failed; turn continued unchanged.')
        board = latestBoard
      }
      if (!board) return prior
      const { Box, Text, Button } = $.ui.resolve(e)
      const content = board.summary.map((part, index) => Text({
        key: `purser-summary-${index}`, children: part.text, color: part.color,
        dimColor: part.dimColor, wrap: 'truncate-end',
      }))
      content.push(Text({ key: 'purser-summary-details-sep', children: ' · ', dimColor: true }))
      content.push(Button({
        key: 'purser-board-details', label: 'details', plain: true, dimColor: true,
        onPress: async () => { await $.ui.open({ id: detailsPaneId, title: 'Jobs' }) },
      }))
      if (!prior) return Box({ width: e.props.bodyColumns, flexDirection: 'row', flexWrap: 'nowrap', children: content })
      // The engine refuses its own node under a Box with a width, so the outer row stays unsized.
      // It wraps so a narrow window moves the board below the band instead of drawing over it.
      return Box({
        flexDirection: 'row', flexWrap: 'wrap',
        children: [prior, Text({ children: ' · ', dimColor: true }),
          Box({ flexDirection: 'row', flexWrap: 'nowrap', flexShrink: 1, children: content })],
      })
    } catch {
      await reportOnce($, 'Purser job board refresh failed; turn continued unchanged.')
      return prior
    }
  })
  on('ui.render', { component: 'Pane' }, async ($, e, next) => {
    if (e.requestId !== detailsPaneId) return next(e)
    const prior = await next(e)
    if (!latestBoard) return prior
    const { Box, Markdown } = $.ui.resolve(e)
    const detail = Markdown({ text: latestBoard.detail })
    return Box({ flexDirection: 'column', children: prior ? [prior, detail] : [detail] })
  })
}
