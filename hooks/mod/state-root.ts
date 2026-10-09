export function stateRoot(config: { purserState?: string; claudeConfig?: string; home?: string }): string | undefined {
  const { purserState, claudeConfig, home } = config
  const expand = (path: string) => path === '~' || /^~[\\/]/.test(path)
    ? `${home ?? ''}${path.slice(1)}`
    : path
  const join = (base: string, child: string) => `${base.replace(/[\\/]+$/, '')}/${child}`

  if (purserState) return expand(purserState)
  if (claudeConfig) return join(expand(claudeConfig), 'purser')
  return home ? join(join(home, '.claude'), 'purser') : undefined
}
