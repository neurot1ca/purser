import { projectKeyFromRoot } from './project-key.js'
import { stateRoot } from './state-root.js'

const roots = new Map<string, string>()

export function registerLanguage(on) {
  on('prompt.compose', async ($, e, next) => {
    const prior = await next(e)
    try {
      const cwd = await $.session.cwd()
      let root = roots.get(cwd)
      if (!root) {
        const result = await $.process.run(['git', 'rev-parse', '--show-toplevel'], { cwd, timeoutMs: 1000 })
        if (result.exitCode !== 0 || !result.stdout.trim()) return prior
        root = result.stdout.trim()
        roots.set(cwd, root)
      }
      const os = await $.env.get('OS')
      const [purserState, claudeConfig, home] = await Promise.all([
        $.env.get('PURSER_STATE_DIR'),
        $.env.get('CLAUDE_CONFIG_DIR'),
        os === 'Windows_NT' ? $.env.get('USERPROFILE') : $.env.get('HOME'),
      ])
      const runtime = stateRoot({ purserState, claudeConfig, home })
      if (!runtime) return prior
      const key = await projectKeyFromRoot(root, os)
      const handoff = await $.fs.read(`${runtime}/projects/${key}/telemetry/purser-handoff.md`)
      const match = /^[ \t]*User language:[ \t]*([^\r\n]*)/im.exec(handoff)
      if (!match) return prior
      const language = match[1].trim()
      if (language.toLowerCase() === 'english') return prior
      const text = !language || language.startsWith('<')
        ? 'Reply in the language the user writes in.'
        : `Reply to the user in ${language}.`
      return { ...prior, sections: [...prior.sections, { id: 'purser:language', text, scope: 'session' }] }
    } catch {
      return prior
    }
  })
}
