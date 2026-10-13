import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import { extractBalancedBody } from './sourceBoundary.mjs'

const page = readFileSync(new URL('../pages/AntigravityAccounts.tsx', import.meta.url), 'utf8')
const batch = readFileSync(new URL('../components/AntigravityBatchActions.tsx', import.meta.url), 'utf8')
const results = readFileSync(new URL('../components/OperationResultsModal.tsx', import.meta.url), 'utf8')

test('extracted page retains one minimal selection map and the fork batch controller', () => {
  assert.match(page, /useState<AntigravitySelection>/)
  assert.match(page, /\.filter\(isAntigravitySelectionAccount\)/)
  assert.match(page, /reconcileAntigravitySelection\(current, accounts, batchBusyRef\.current\)/)
  assert.match(page, /<AntigravityBatchActions/)
  assert.match(page, /hiddenSelectedCount=\{selectionSummary.hiddenSelectedCount\}/)
  assert.doesNotMatch(page, /useState<Set<number>>|knownAccountsRef|handleBatchDelete/)
})

test('new actions and existing actions share a synchronous operation fence', () => {
  for (const marker of ['const handleBatchUpdate', 'const handleBatchResetStatus', 'const handleBatchTest', 'const handleBatchSync', 'const handleCleanAbnormal']) {
    const start = page.indexOf(marker)
    assert.notEqual(start, -1)
    const arrow = page.indexOf('=>', start)
    assert.notEqual(arrow, -1)
    const body = extractBalancedBody(page.slice(arrow), '=>')
    assert.match(body, /onBatchBusyChange\(true\)/)
    assert.match(body, /onBatchBusyChange\(false\)/)
  }
  assert.match(page, /isExternalBusy=\{\(\) => batchBusyRef.current\}/)
  assert.match(batch, /externalBusy \|\| isExternalBusy\?\.\(\)/)
  assert.match(page, /setBatchModelsOpen\(false\); onBatchBusyChange\(false\)/)
})

test('historical source badges and cooldown audit survive component extraction', () => {
  assert.match(results, /accounts: readonly ResultAccount\[\]/)
  assert.match(results, /UpstreamSourceBadge source=\{result.upstream_source\}/)
  assert.match(page, /AntigravityCooldownDetails key=\{detailAccount.id\}/)
  const finish = extractBalancedBody(page, 'const finishBatch')
  assert.doesNotMatch(finish, /clearSelection\(/)
})
