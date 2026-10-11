import { expect, test } from 'claude-code/testing'
import { stateRoot } from './state-root.js'

test('PURSER_STATE_DIR wins over the other state root settings', () => {
  expect(stateRoot({ purserState: '/state', claudeConfig: '/claude', home: '/home' })).toBe('/state')
})

test('uses CLAUDE_CONFIG_DIR/purser when PURSER_STATE_DIR is unset', () => {
  expect(stateRoot({ claudeConfig: '/claude', home: '/home' })).toBe('/claude/purser')
})

test('falls back to home/.claude/purser', () => {
  expect(stateRoot({ home: '/home' })).toBe('/home/.claude/purser')
})

test('expands a tilde using home', () => {
  expect(stateRoot({ purserState: '~/state', home: '/home' })).toBe('/home/state')
})

test('returns undefined when no state directory or home is available', () => {
  expect(stateRoot({})).toBe(undefined)
})
