import assert from 'node:assert/strict'
import test from 'node:test'
import { readFileSync } from 'node:fs'
import { normalizeUpstreamSource } from './upstreamSource.ts'
import { collectAccountOperationResult } from './accountOperationResults.ts'

const source = (path) => readFileSync(new URL(path, import.meta.url), 'utf8')

test('recorded sources remain distinct and absent or legacy values stay unknown', () => {
  for (const value of ['bps', 'codex', 'other']) assert.equal(normalizeUpstreamSource(value), value)
  for (const value of [undefined, null, '', 'legacy', 'BPS']) assert.equal(normalizeUpstreamSource(value), 'unknown')
})

test('batch results retain backend source evidence only, including failures', () => {
  const results = new Map()
  collectAccountOperationResult(results, { type: 'start', action: 'batch_test' })
  for (const [index, upstream_source] of ['bps', 'codex', 'other', '', undefined].entries()) {
    collectAccountOperationResult(results, {
      type: 'progress', action: 'batch_test', account_id: index + 1,
      status: 'failed', error: 'upstream failed', upstream_source,
    })
    assert.equal(results.get(index + 1).upstream_source, upstream_source)
  }
  assert.equal('upstream_source' in results.get(5), false)
})

test('usage list, count, error summary and range stats share the source query builder', () => {
  const api = source('../api.ts')
  for (const method of ['getUsageLogs', 'getUsageLogsPaged', 'getUsageLogsErrorSummary', 'getUsageStats']) {
    const start = api.indexOf(`  ${method}:`)
    assert.notEqual(start, -1)
    assert.match(api.slice(start, start + 650), /buildUsageLogSearchParams\(/, method)
  }
  const usage = source('../pages/Usage.tsx')
  assert.match(usage, /upstream_source: filterUpstreamSource/)
  assert.match(usage, /setFilterUpstreamSource\(''\)/)
  assert.match(usage, /setFilterUpstreamSource\(value as UpstreamSourceFilter\); setPage\(1\)/)
  assert.match(usage, /upstreamSource: true/)
  assert.match(usage, /if \(key in parsed\) defaults\[key\] = Boolean\(parsed\[key\]\)/)
  assert.equal((usage.match(/source=\{log\.upstream_source\}/g) ?? []).length, 2)
  assert.match(usage, /const cumulativeRequests = stats\?\.total_requests \?\? 0/)
})

test('usage source defaults visible only when the stored preference is missing', () => {
  const usage = source('../pages/Usage.tsx')
  const defaultsBody = usage.match(/const DEFAULT_USAGE_VISIBLE_COLUMNS:[^=]+=(\s*\{[\s\S]*?\n\s*\})/)[1]
  const initializer = usage.slice(usage.indexOf('function getInitialUsageVisibleColumns()'), usage.indexOf('\nfunction persistUsageVisibleColumns'))
    .replace('function getInitialUsageVisibleColumns(): Record<UsageTableColumn, boolean>', 'function getInitialUsageVisibleColumns()')
    .replace('const defaults: Record<UsageTableColumn, boolean>', 'const defaults')
    .replace(' as UsageTableColumn[]', '')
  const readPreference = (stored) => Function('localStorage', 'USAGE_VISIBLE_COLUMNS_KEY', `const DEFAULT_USAGE_VISIBLE_COLUMNS = ${defaultsBody}; ${initializer}; return getInitialUsageVisibleColumns();`)({ getItem: () => stored }, 'codex2api:usage:visible-columns')
  assert.equal(readPreference(null).upstreamSource, true)
  assert.equal(readPreference('{}').upstreamSource, true)
  assert.equal(readPreference('{"model":false}').upstreamSource, true)
  assert.equal(readPreference('{"upstreamSource":false}').upstreamSource, false)
  assert.equal(readPreference('{"upstreamSource":true}').upstreamSource, true)
  assert.equal(readPreference('invalid json').upstreamSource, true)
  assert.equal(readPreference('{"upstreamSource":false,"model":false}').model, false)
  assert.doesNotMatch(usage, /localStorage\.(removeItem|clear)\(/)
})

test('single, batch and HTML quality test badges use response fields, not preferences', () => {
  const single = source('../components/TestConnectionModal.tsx')
  assert.match(single, /setUpstreamSource\(event\.upstream_source\)/)
  assert.match(single, /setUpstreamSource\(undefined\)/)
  assert.match(source('../components/OperationResultsModal.tsx'), /source=\{result\.upstream_source\}/)
  assert.match(source('../pages/QualityTest.tsx'), /source=\{run\.upstream_source\}/)
  assert.match(source('../pages/QualityTest.tsx'), /source=\{job\.upstream_source\}/)
  assert.doesNotMatch(source('../components/UpstreamSourceBadge.tsx'), /account|openai_excel_bps/)
})

test('account batch edits no longer hydrate or submit retired BPS preferences', () => {
  const accounts = source('../pages/Accounts.tsx')
  assert.doesNotMatch(accounts, /selectBPSBatchAccounts|batchBPSMode|openai_excel_bps|excelBpsBatchDone|BPSMasterContext/)
  assert.match(accounts, /buildBatchMetadataUpdate\(/)
  assert.match(accounts, /api\.batchUpdateAccounts\(metadata\)/)
})

test('historical upstream source strings survive across locales without runtime BPS controls', () => {
  const locales = ['en', 'zh', 'zh-TW'].map((locale) => JSON.parse(source(`../locales/${locale}.json`)))
  const keys = Object.keys(locales[0].upstreamSource).sort()
  for (const locale of locales) {
    assert.equal('bps' in locale, false)
    assert.deepEqual(Object.keys(locale.upstreamSource).sort(), keys)
    assert.equal(locale.upstreamSource.bps, 'BPS')
    for (const value of Object.values(locale.upstreamSource)) assert.equal(typeof value, 'string')
    assert.deepEqual(Object.keys(locale.accounts).filter(key => key.startsWith('excelBps')), [])
  }
})


test('upstream UA cleanup preserves the independent historical TurnState length audit', () => {
  const usage = source('../pages/Usage.tsx')
  const ua = usage.slice(usage.indexOf('function UserAgentCell('), usage.indexOf('function CyberPolicyDetailButton('))
  assert.doesNotMatch(ua, /injected_turn_state|upstream_turn_state|turnStateChip|turnStateRows|TurnStateCell/)
  assert.equal((usage.match(/<TurnStateLengthCell log=\{log\}/g) ?? []).length, 2)
  assert.match(usage, /visibleColumns\.turnState && <TableHead/)
  assert.match(usage, /turnState: true/)
  assert.match(usage, /const FULL_BLOOD_TURN_STATE_CHARS = 292/)
  const body = usage.match(/function turnStateLengthForLog\(log: UsageLog\): \{ length: number; source: 'injected' \| 'upstream' \} \| null \{([\s\S]*?)\n\s*\}/)
  assert.ok(body)
  const lengthForLog = Function('log', body[1])
  assert.equal(lengthForLog({}), null)
  assert.equal(lengthForLog({ injected_turn_state: ' ', upstream_turn_state: '' }), null)
  assert.deepEqual(lengthForLog({ injected_turn_state: 'x'.repeat(292), upstream_turn_state: 'old' }), { length: 292, source: 'injected' })
  assert.deepEqual(lengthForLog({ injected_turn_state: ' ', upstream_turn_state: ' old ' }), { length: 3, source: 'upstream' })
})
