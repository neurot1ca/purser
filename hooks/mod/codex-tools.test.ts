import { expect, test } from 'claude-code/testing'
import { CODEX_EXEC_WIRE_FIXTURES } from './fixtures/codex_exec_wire.js'
import { CODEX_TOOL_SCHEMAS, renderCodexJobResult, renderCodexToolResult } from './codex-tools.js'

test('exports the six Codex tool schemas byte-for-byte from the wire oracle', () => {
  expect(CODEX_TOOL_SCHEMAS.map(schema => schema.name)).toEqual([
    'codex', 'codex-reply', 'codex_start', 'codex_result', 'codex_cancel', 'codex_apply',
  ])
  for (const fixture of CODEX_EXEC_WIRE_FIXTURES.filter(item => item.input.function === 'schema')) {
    const schema = CODEX_TOOL_SCHEMAS.find(item => item.name === fixture.input.tool)
    expect(schema).toBeDefined()
    if (!schema) throw new Error(`missing schema for ${fixture.input.tool}`)
    const rendered = renderCodexJobResult(schema)
    expect(JSON.stringify(rendered.structuredContent)).toBe(JSON.stringify(JSON.parse(fixture.expected_text)))
    expect(rendered.content[0].text).toBe(fixture.expected_text)
  }
})

test('renders every sync tool result with Python MCP envelope fields and exact text', () => {
  for (const fixture of CODEX_EXEC_WIRE_FIXTURES.filter(item => item.input.function === '_tool_result')) {
    const rendered = renderCodexToolResult(fixture.input)
    expect(rendered.content[0].text).toBe(fixture.expected_text)
    const failed = fixture.input.error !== undefined
    expect(rendered.isError).toBe(failed)
    if (failed) expect('structuredContent' in rendered).toBe(false)
    else expect(rendered.structuredContent).toEqual(JSON.parse(fixture.expected_text))
  }
})

test('renders every durable job result with Python MCP envelope fields and exact text', () => {
  for (const fixture of CODEX_EXEC_WIRE_FIXTURES.filter(item => item.input.function === '_job_result')) {
    const rendered = renderCodexJobResult(fixture.input.value)
    expect(rendered.content[0].text).toBe(fixture.expected_text)
    expect(rendered.structuredContent).toEqual(fixture.input.value)
    expect(rendered.isError).toBe(Object.hasOwn(fixture.input.value, 'errorCode'))
  }
})
