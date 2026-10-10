import { expect, mock, test } from 'claude-code/testing'
import { projectKeyFromRoot } from './project-key.js'

const facts = { model: 'claude-sonnet-4-6', promptModel: 'claude-sonnet-4-6', outputStyle: null, tools: [], traits: [], surfaces: [] }
const prior = { sections: [
  { id: 'fixture:shared', text: 'Shared context', scope: 'shared' },
  { id: 'fixture:session', text: 'Session context', scope: 'session' },
] }

function fixture(on, cwd: string, language: string, root = '/repo', os = 'Linux') {
  const state = { language, gitCalls: 0, reads: [] as string[], readFailure: false, gitFailure: false }
  mock.env(on, { OS: os, PURSER_STATE_DIR: '/state', HOME: '/home', USERPROFILE: 'C:/Users/test' })
  on('session.cwd', () => ({ value: cwd }))
  on('process.run', (_$, e) => {
    expect(e.argv).toEqual(['git', 'rev-parse', '--show-toplevel'])
    expect(e.init.cwd).toBe(cwd)
    expect(e.init.timeoutMs).toBe(1000)
    state.gitCalls++
    if (state.gitFailure && state.gitCalls === 1) return { value: { exitCode: 1, stdout: '', stderr: '' } }
    return { value: { exitCode: 0, stdout: `${root}\n`, stderr: '' } }
  })
  on('fs.read', (_$, e) => {
    state.reads.push(e.path.replace(/\\/g, '/').replace(/^[A-Za-z]:/, ''))
    if (state.readFailure) return { deny: 'fixture read failure' }
    return { value: `# Purser handoff\r\n## Goal\r\nUser language: ${state.language}\r\n## State\r\nContinue\r\n` }
  })
  on('prompt.compose', () => prior)
  return state
}

test('appends the Turkish session instruction after every prior section', async ($, on) => {
  const state = fixture(on, '/language-turkish', 'Turkish')
  expect(await $.prompt.compose(facts)).toEqual({ sections: [...prior.sections,
    { id: 'purser:language', text: 'Reply to the user in Turkish.', scope: 'session' },
  ] })
  expect(state.reads).toEqual(['/state/projects/816fc349d3faebf8/telemetry/purser-handoff.md'])
})

test('English leaves the composed prompt unchanged', async ($, on) => {
  fixture(on, '/language-english', 'eNgLiSh')
  expect(await $.prompt.compose(facts)).toEqual(prior)
})

test('placeholder and empty values use the user-language fallback', async ($, on) => {
  const state = fixture(on, '/language-placeholder', '<the language the user writes in>')
  for (const language of [state.language, '']) {
    state.language = language
    expect(await $.prompt.compose(facts)).toEqual({ sections: [...prior.sections,
      { id: 'purser:language', text: 'Reply in the language the user writes in.', scope: 'session' },
    ] })
  }
})

test('reads a changed language on the next request and caches the git root', async ($, on) => {
  const state = fixture(on, '/language-change', 'Turkish')
  expect((await $.prompt.compose(facts)).sections.at(-1)?.text).toBe('Reply to the user in Turkish.')
  state.language = 'German'
  expect((await $.prompt.compose(facts)).sections.at(-1)?.text).toBe('Reply to the user in German.')
  expect(state.gitCalls).toBe(1)
  expect(state.reads).toHaveLength(2)
})

test('Windows git roots use the lowercase backslash project key', async ($, on) => {
  const state = fixture(on, 'C:/Repo/X', 'Turkish', 'C:/Repo/X', 'Windows_NT')
  expect((await $.prompt.compose(facts)).sections.at(-1)?.id).toBe('purser:language')
  expect(state.reads).toEqual(['/state/projects/9b13f1fe3041fb36/telemetry/purser-handoff.md'])
})

test('a handoff read failure leaves every prior section unchanged', async ($, on) => {
  const state = fixture(on, '/language-read-failure', 'Turkish')
  state.readFailure = true
  expect(await $.prompt.compose(facts)).toEqual(prior)
})

test('a failed git root lookup is retried on the next request', async ($, on) => {
  const state = fixture(on, '/language-git-failure', 'Turkish')
  state.gitFailure = true
  expect(await $.prompt.compose(facts)).toEqual(prior)
  expect((await $.prompt.compose(facts)).sections.at(-1)?.text).toBe('Reply to the user in Turkish.')
  expect(state.gitCalls).toBe(2)
})

test('projectKeyFromRoot hashes plain roots and normalizes only Windows roots', async () => {
  expect(await projectKeyFromRoot('/repo', undefined)).toBe('816fc349d3faebf8')
  expect(await projectKeyFromRoot('C:/Repo/X', 'Windows_NT')).toBe('9b13f1fe3041fb36')
  expect(await projectKeyFromRoot('C:/Repo/X', 'Linux')).toBe('1cdec20396bae5ec')
})
