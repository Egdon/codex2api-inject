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
  assert.match(usage, /upstreamSource: false/)
  assert.equal((usage.match(/source=\{log\.upstream_source\}/g) ?? []).length, 2)
  assert.match(usage, /const cumulativeRequests = stats\?\.total_requests \?\? 0/)
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

test('BPS batch hydration uses one lite list, not current-page inference or credential state', () => {
  const accounts = source('../pages/Accounts.tsx')
  assert.match(accounts, /api\.getAccounts\(\{ channel: 'codex', view: 'lite' \}\)/)
  assert.match(accounts, /selectBPSBatchAccounts\(ids, response\.accounts \?\? \[\], batchBPSMode\)/)
  assert.match(accounts, /delete sharedMetadata\.openai_excel_bps/)
})

test('all core BPS and upstream source strings exist across supported locales', () => {
  const locales = ['en', 'zh', 'zh-TW'].map((locale) => JSON.parse(source(`../locales/${locale}.json`)))
  for (const namespace of ['bps', 'upstreamSource']) {
    const keys = Object.keys(locales[0][namespace]).sort()
    for (const locale of locales) {
      assert.deepEqual(Object.keys(locale[namespace]).sort(), keys)
      for (const value of Object.values(locale[namespace])) assert.equal(typeof value, 'string')
    }
  }
})
