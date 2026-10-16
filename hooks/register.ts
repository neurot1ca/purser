import { buildReadEvent, platformBinary } from './mod/read.js'
import { mapHookOutput } from './mod/lifecycle.js'
import type { LifecycleEvent } from './mod/lifecycle.js'
import { registerBoard } from './mod/board.js'
import { registerUsage } from './mod/usage.js'
import { register as registerEnforcement } from './mod/enforcement.js'
import { registerAutoCompact } from './mod/autocompact.js'
import { registerLanguage } from './mod/language.js'
import { registerQuotaWake } from './mod/quotawake.js'

const reported = new Set<string>()
const lifecycleReported = new Set<string>()
let binaryPath: Promise<string> | undefined

async function detectBinary($): Promise<string> {
  const os = await $.env.get('OS')
  const ostype = await $.env.get('OSTYPE')
  const arch = await $.env.get('PROCESSOR_ARCHITECTURE')
  const hosttype = await $.env.get('HOSTTYPE')
  let goos = os === 'Windows_NT' ? 'windows' :
    /darwin/i.test(ostype ?? '') ? 'darwin' : /linux/i.test(ostype ?? '') ? 'linux' : ''
  let goarch = /^(amd64|x86_64)$/i.test(arch ?? hosttype ?? '') ? 'amd64' :
    /^(arm64|aarch64)$/i.test(arch ?? hosttype ?? '') ? 'arm64' : ''
  if (!goos || !goarch) {
    const uname = await $.process.run(['uname', '-s', '-m'], { timeoutMs: 1000 })
    if (uname.exitCode !== 0) throw new Error('platform detection failed')
    const [system, machine] = uname.stdout.trim().split(/\s+/)
    goos ||= /darwin/i.test(system ?? '') ? 'darwin' : /linux/i.test(system ?? '') ? 'linux' : ''
    goarch ||= /^(amd64|x86_64)$/i.test(machine ?? '') ? 'amd64' :
      /^(arm64|aarch64)$/i.test(machine ?? '') ? 'arm64' : ''
  }
  return `${$.plugin.root}/bin/${platformBinary(goos, goarch)}`
}

async function lifecycle($, e, next, name: LifecycleEvent) {
  const prior = await next(e)
  if (name === 'UserPromptSubmit' || name === 'Stop') $.ui.invalidate('ui.render')
  let mapped: ReturnType<typeof mapHookOutput> = {}
  try {
    binaryPath ??= detectBinary($).catch(error => {
      binaryPath = undefined
      throw error
    })
    const result = await $.process.run([await binaryPath], {
      cwd: e.cwd,
      stdin: JSON.stringify(e),
      timeoutMs: 60000,
    })
    if (result.exitCode !== 0) throw new Error(`Go ${name} helper exited ${result.exitCode}`)
    mapped = mapHookOutput(name, result.stdout)
  } catch {
    const key = e.session_id || '<unknown>'
    if (!lifecycleReported.has(key)) {
      lifecycleReported.add(key)
      try { await $.ui.log('Go context lifecycle failed; event continued unchanged.') }
      catch { /* logging must not affect hooks */ }
    }
  }
  if (prior.block || (!mapped.block && !mapped.additionalContext)) return prior
  if (mapped.block) return { ...prior, block: mapped.block }
  return { ...prior, additionalContext: [...(prior.additionalContext ?? []), ...mapped.additionalContext!] }
}

export function register(on) {
  registerBoard(on)
  registerUsage(on)
  registerEnforcement(on)
  registerAutoCompact(on)
  registerLanguage(on)
  registerQuotaWake(on)

  on('tool.call', { tool: 'Read' }, async ($, e, next) => {
    let sessionId = ''
    try {
      const [currentSession, cwd] = await Promise.all([$.session.id(), $.session.cwd()])
      sessionId = currentSession
      binaryPath ??= detectBinary($).catch(error => {
        binaryPath = undefined
        throw error
      })
      const binary = await binaryPath
      const event = buildReadEvent(e, sessionId, cwd)
      const result = await $.process.run([binary], {
        cwd,
        stdin: JSON.stringify(event),
        timeoutMs: 5000,
      })
      if (result.exitCode !== 0) throw new Error(`Go Read helper exited ${result.exitCode}`)
    } catch {
      const key = sessionId || '<unknown>'
      if (!reported.has(key)) {
        reported.add(key)
        try { await $.ui.log('Go Read telemetry failed; Read continued unchanged.') }
        catch { /* telemetry must not affect Read */ }
      }
    }
    return next(e)
  })

  on('classic.UserPromptSubmit', ($, e, next) => lifecycle($, e, next, 'UserPromptSubmit'))
  on('classic.SessionStart', async ($, e, next) => {
    const prior = await next(e)
    return lifecycle($, e, () => prior, 'SessionStart')
  })
  on('classic.PreCompact', ($, e, next) => lifecycle($, e, next, 'PreCompact'))
  on('classic.Stop', ($, e, next) => lifecycle($, e, next, 'Stop'))
}
