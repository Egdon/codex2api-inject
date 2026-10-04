import assert from 'node:assert/strict'
import test from 'node:test'
import { existsSync, readFileSync, readdirSync } from 'node:fs'
import { buildBatchMetadataUpdate } from './accountBatchUpdate.ts'
import { formStateFromAccount, buildQuickConfigSavePayload } from './accountQuickConfig.ts'

const source = path => readFileSync(new URL(path, import.meta.url), 'utf8')

// Historical source attribution survives; no runtime settings/flags may return.
test('frontend runtime source no longer references retired BPS fields or controls', () => {
  const root = new URL('../', import.meta.url)
  const retired = /openai_excel_bps|codex_basispoints|excelBps|BPSBatchMode|buildBPSAccountPatch|selectBPSBatchAccounts|isBPSAccountEligible/
  for (const path of readdirSync(root, { recursive: true })) {
    if (!/\.(ts|tsx|json)$/.test(path)) continue
    assert.doesNotMatch(readFileSync(new URL(path, root), 'utf8'), retired, path)
  }
  for (const path of ['./bps.ts', './excelBpsModels.ts', './excelBpsStatus.ts', '../components/ExcelBpsBadge.tsx']) {
    assert.equal(existsSync(new URL(path, import.meta.url)), false, path)
  }
})

test('stale BPS account and form fields never reappear in quick-config payloads', () => {
  const account = { id: 1, account_type: 'oauth', tags: ['saved'], custom_headers: { 'X-Test': 'keep' }, group_ids: [7] }
  const form = formStateFromAccount(account)
  const stale = formStateFromAccount({ ...account, openai_excel_bps: true, openai_excel_bps_opt_out: true })
  assert.deepEqual(stale, form)
  const result = buildQuickConfigSavePayload({ ...stale, excelBpsMode: 'on' }, true)
  assert.equal(result.ok, true)
  assert.deepEqual(result, buildQuickConfigSavePayload(form, true))
  assert.equal('openai_excel_bps' in result.payload, false)
  assert.equal('openai_excel_bps_opt_out' in result.payload, false)
  assert.deepEqual(result.payload.custom_headers, account.custom_headers)
  assert.deepEqual(result.payload.group_ids, [7])
})

test('batch metadata ignores retired modes without losing independent selected fields', () => {
  const batch = {
    ids: [1, 2], updateTags: true, tags: ['keep'], updateGroups: true, groupIds: [9],
    updateScoreBias: false, scoreBias: null, updateBaseConcurrency: false, baseConcurrency: null,
    updateSchedulerPriority: false, schedulerPriority: null,
    updateCodexFingerprintMode: true, codexFingerprintMode: 'single_machine_multi_window',
    updateTimezone: true, timezone: ' UTC ',
  }
  const expected = { ids: [1, 2], tags: ['keep'], group_ids: [9], codex_fingerprint_mode: 'single_machine_multi_window', timezone: 'UTC' }
  for (const bpsMode of ['unchanged', 'inherit', 'on', 'off']) {
    assert.deepEqual(buildBatchMetadataUpdate({ ...batch, bpsMode }), expected)
  }
})

test('historical BPS attribution stays labeled and filterable in all locales', () => {
  for (const locale of ['en', 'zh', 'zh-TW']) {
    const messages = JSON.parse(source(`../locales/${locale}.json`))
    assert.equal(messages.upstreamSource.bps, 'BPS')
    assert.equal(Object.keys(messages.accounts).some(key => /bps|basispoints/i.test(key)), false)
  }
  assert.match(source('../pages/Usage.tsx'), /\['', 'bps', 'codex', 'other', 'unknown'\]/)
  assert.match(source('./upstreamSource.ts'), /bps/)
})
