import { expect, mock, test } from 'claude-code/testing'
import { renderJobBoard } from './board.js'

const runningId = 'a'.repeat(64)
const queuedId = 'b'.repeat(64)
const doneId = 'c'.repeat(64)
const files = {
  indexes: [
    { jobId: runningId, call: 'run', call_key: 'run-bundle' },
    { jobId: queuedId, call: 'queue' },
    { jobId: doneId, call: 'done' },
  ],
  jobs: [
    { jobId: runningId, call: 'run', state: 'RUNNING', label: 'api fix', model: 'gpt-6-sol', effort: 'high', route: 'normal', acceptedAt: 100, startedAt: 880, activeAgent: 'codex-main', liveness: { lastActivityAt: 997 }, threadId: 'thread-run', phase: 'implementation' },
    { jobId: queuedId, call: 'queue', state: 'QUEUED', label: 'test update', model: 'gpt-6-sol', effort: 'medium', acceptedAt: 970, threadId: 'thread-queue', phase: 'implementation' },
    { jobId: doneId, call: 'done', state: 'SUCCEEDED', label: 'docs', model: 'gpt-6-sol', effort: 'low', finishedAt: 990, appliedAt: 995, applyState: 'applied', boardDisplayCount: 0, threadId: 'thread-done', phase: 'implementation' },
  ],
  leases: { owners: {
    running: { state: 'ACTIVE', job_id: runningId, scope: ['src'] },
    queued: { state: 'QUEUED', job_id: queuedId, scope: ['src'] },
  } },
  bundles: { 'run-bundle': { reviewer1_result: 'PASS' } },
}

test('renders running, queued behind, and done/applied project jobs with the existing columns', () => {
  const board = renderJobBoard(files, 1000)
  expect(board).toBe([
    'Purser jobs:',
    '| ⚓ Task | Model | State | ETA | Review | Apply |',
    '| --- | --- | --- | --- | --- | --- |',
    '| api fix | gpt-6-sol (high) | running · codex-main active 3s ago | 2m elapsed, 3m left | reviewed | n/a |',
    '| test update | gpt-6-sol (medium) | queued behind api fix | waited 30s | — | — |',
    '| docs | gpt-6-sol (low) | done | — | — | applied |',
  ].join('\n'))
})

test('shows an overdue duration against the job ETA', () => {
  const job = { ...files.jobs[0], startedAt: 100, etaMinutes: 5, activeAgent: undefined, liveness: undefined }
  const board = renderJobBoard({ indexes: [files.indexes[0]], jobs: [job] }, 1000)
  expect(board).toContain('| api fix | gpt-6-sol (high) | running | 15m elapsed, overdue +10 min |')
})

test('omits activity text when the job has no activity or checkpoint timestamp', () => {
  const job = { ...files.jobs[0], activeAgent: undefined, liveness: undefined }
  const board = renderJobBoard({ indexes: [files.indexes[0]], jobs: [job] }, 1000)
  expect(board).toContain('| running |')
  expect(board).not.toContain('active')
})

test('draws fixture job records in AbovePrompt and keeps the prior mod drawing', async ($, on) => {
  mock.clock(on)
  const repoRoot = '/repo'
  mock.env(on, { OS: 'Linux', HOME: '/home/test', OSTYPE: 'linux-gnu', HOSTTYPE: 'x86_64' })
  const logs: string[] = []
  const now = Date.now() / 1000
  const digest = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(repoRoot))
  const projectKey = [...new Uint8Array(digest)].map(byte => byte.toString(16).padStart(2, '0')).join('').slice(0, 16)
  const records = [
    { ...files.jobs[0], acceptedAt: now - 60, startedAt: now - 60 },
    files.jobs[1],
    { ...files.jobs[2], finishedAt: now, appliedAt: now },
  ]
  const indexes = files.indexes.map(index => ({ ...index, project_key: projectKey }))
  const values: Record<string, string> = {
    ...Object.fromEntries(indexes.map(index => [
      `async_jobs/${index.jobId}.json`, JSON.stringify(index),
    ])),
    ...Object.fromEntries(records.map(job => [
      `codex_exec_mcp/jobs/${job.jobId}.json`, JSON.stringify(job),
    ])),
    [`projects/${projectKey}/scope_leases.json`]: JSON.stringify(files.leases),
    ['bundle_artifacts/run-bundle.json']: JSON.stringify(files.bundles['run-bundle']),
  }
  on('process.run', () => ({ value: { exitCode: 0, stdout: `${repoRoot}\n`, stderr: '' } }))
  on('session.cwd', () => ({ value: repoRoot }))
  on('session.id', () => ({ value: 'board-fixture' }))
  on('fs.list', () => ({
    value: indexes.map(index => ({ name: `${index.jobId}.json`, kind: 'file', size: 1, mtimeMs: now, isLink: false })),
  }))
  on('fs.read', (_$, e) => {
    const parts = e.path.split(/[\\/]/)
    const rootIndex = parts.lastIndexOf('purser')
    const relative = parts.slice(rootIndex + 1).join('/')
    return values[relative] === undefined ? { deny: 'unexpected fixture path' } : { value: values[relative] }
  })
  on('ui.log', (_$, e) => { logs.push(e.text); return { value: undefined } })
  on('ui.render', { component: 'AbovePrompt' }, ($, e) => {
    const { Text } = $.ui.resolve(e)
    return Text({ children: 'Other mod status' })
  })
  const ui = await $.ui.mount({
    plugin: 'board-plugin-test', surface: 'terminal', component: 'AbovePrompt',
    props: { hasSurvey: false, isWorking: false, maxRows: 12, bodyColumns: 100, scroll: { offset: 0, bodyRows: 12 }, view: {} },
  })
  expect(logs).toEqual([])
  const board = await ui.find({ type: 'Markdown' })
  expect(board).toBeDefined()
  expect(board?.text).toContain('queued behind api fix')
  expect((await ui.find({ type: 'Text', text: 'Other mod status' }))?.text).toBe('Other mod status')
  await ui.unmount()
})

test('returns no board when this project has no jobs', () => {
  expect(renderJobBoard({ indexes: [], jobs: [] })).toBeUndefined()
})

test('ignores malformed or mismatched job records', () => {
  expect(renderJobBoard({
    indexes: [{ jobId: runningId, call: 'expected' }],
    jobs: [{ jobId: runningId, call: 'other', state: 'RUNNING', label: 'wrong project job' }],
  })).toBeUndefined()
})

test('leaves another mod drawing intact when Purser has no board', async ($, on) => {
  mock.clock(on)
  on('ui.log', () => ({ value: undefined }))
  on('ui.render', { component: 'AbovePrompt' }, ($, e) => {
    const { Text } = $.ui.resolve(e)
    return Text({ children: 'Other mod status' })
  })
  const ui = await $.ui.mount({
    plugin: 'board-plugin-test', surface: 'terminal', component: 'AbovePrompt',
    props: { hasSurvey: false, isWorking: false, maxRows: 8, bodyColumns: 80, scroll: { offset: 0, bodyRows: 8 }, view: {} },
  })
  expect((await ui.find({ type: 'Text', text: 'Other mod status' }))?.text).toBe('Other mod status')
  await ui.unmount()
})

test('a missing async jobs directory is empty and does not log a refresh failure', async ($, on) => {
  mock.clock(on)
  const repoRoot = '/repo'
  mock.env(on, { OS: 'Linux', HOME: '/home/test', OSTYPE: 'linux-gnu', HOSTTYPE: 'x86_64' })
  const logs: string[] = []
  on('process.run', () => ({ value: { exitCode: 0, stdout: `${repoRoot}\n`, stderr: '' } }))
  on('session.cwd', () => ({ value: repoRoot }))
  on('session.id', () => ({ value: 'board-empty' }))
  on('fs.list', () => ({ deny: 'ENOENT: no such file or directory' }))
  on('ui.log', (_$, e) => { logs.push(e.text); return { value: undefined } })
  on('ui.render', { component: 'AbovePrompt' }, ($, e) => {
    const { Text } = $.ui.resolve(e)
    return Text({ children: 'Other mod status' })
  })
  const ui = await $.ui.mount({
    plugin: 'board-plugin-test', surface: 'terminal', component: 'AbovePrompt',
    props: { hasSurvey: false, isWorking: false, maxRows: 8, bodyColumns: 80, scroll: { offset: 0, bodyRows: 8 }, view: {} },
  })
  expect(logs).toEqual([])
  expect(await ui.find({ type: 'Markdown' })).toBeUndefined()
  expect((await ui.find({ type: 'Text', text: 'Other mod status' }))?.text).toBe('Other mod status')
  await ui.unmount()
})

test('board read errors fail silently, preserve the turn, and log once per session', async ($, on) => {
  const logs: string[] = []
  let logged: () => void = () => {}
  const didLog = new Promise<void>(resolve => { logged = resolve })
  on('env.get', (_$, e) => ({
    value: e.name === 'OS' ? 'Linux' : e.name === 'HOME' ? '/home/test' :
      e.name === 'OSTYPE' ? 'linux-gnu' : e.name === 'HOSTTYPE' ? 'x86_64' : undefined,
  }))
  on('process.run', (_$, e) => ({
    value: { exitCode: 0, stdout: e.init.stdin ? '' : '/repo\n', stderr: '' },
  }))
  mock.clock(on)
  on('session.cwd', () => ({ value: '/repo' }))
  on('session.id', () => ({ value: 'board-error' }))
  on('fs.list', () => ({ deny: 'fixture read failure' }))
  on('ui.log', (_$, e) => { logs.push(e.text); logged(); return { value: undefined } })
  on('ui.render', { component: 'AbovePrompt' }, ($, e) => {
    const { Text } = $.ui.resolve(e)
    return Text({ children: 'Other mod status' })
  })
  on('classic.UserPromptSubmit', () => ({ additionalContext: ['existing context'] }))

  const result = await $.classic.UserPromptSubmit({ session_id: 'board-error', cwd: '/repo', prompt: 'Continue' })
  const ui = await $.ui.mount({
    plugin: 'board-plugin-test', surface: 'terminal', component: 'AbovePrompt',
    props: { hasSurvey: false, isWorking: false, maxRows: 8, bodyColumns: 80, scroll: { offset: 0, bodyRows: 8 }, view: {} },
  })
  await didLog
  expect(result.additionalContext).toEqual(['existing context'])
  expect((await ui.find({ type: 'Text', text: 'Other mod status' }))?.text).toBe('Other mod status')
  expect(await ui.find({ text: /Purser jobs/ })).toBeUndefined()
  const nextUi = await $.ui.mount({
    plugin: 'board-plugin-test', surface: 'terminal', component: 'AbovePrompt',
    props: { hasSurvey: false, isWorking: false, maxRows: 8, bodyColumns: 80, scroll: { offset: 0, bodyRows: 8 }, view: {} },
  })
  await nextUi.unmount()
  expect(logs).toEqual(['Purser job board refresh failed; turn continued unchanged.'])
  await ui.unmount()
})
