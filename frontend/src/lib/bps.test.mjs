import assert from 'node:assert/strict'
import test from 'node:test'
import { readFileSync } from 'node:fs'
import { isBPSAccountEligible, buildBPSAccountPatch, selectBPSBatchAccounts } from './bps.ts'
import { buildBatchMetadataUpdate } from './accountBatchUpdate.ts'
import { formStateFromAccount, buildQuickConfigSavePayload } from './accountQuickConfig.ts'

const source = (path) => readFileSync(new URL(path, import.meta.url), 'utf8')
const oauth = { id: 1, account_type: 'oauth' }
const batch = {
  ids: [1, 2], updateTags: false, tags: ['keep'], updateGroups: false, groupIds: [9],
  updateScoreBias: false, scoreBias: null, updateBaseConcurrency: false, baseConcurrency: null,
  updateSchedulerPriority: false, schedulerPriority: null,
}
const flags = {
  on: { openai_excel_bps: true, openai_excel_bps_opt_out: false },
  off: { openai_excel_bps: false, openai_excel_bps_opt_out: true },
  inherit: { openai_excel_bps: false, openai_excel_bps_opt_out: false },
}

test('BPS global default starts disabled and account mode starts inherited', () => {
  const settings = source('../pages/Settings.tsx')
  assert.match(settings, /codex_basispoints_enabled: false/)
  assert.match(settings, /codex_basispoints_enabled: cacheNormalized\.codex_basispoints_enabled \?\? false/)
  assert.doesNotMatch(settings, /openai_excel_bps_enabled|bps\.master/)
  assert.equal(formStateFromAccount(oauth).excelBpsMode, 'inherit')
})

test('BPS eligibility requires positive ordinary OAuth identity and excludes every alternate provider', () => {
  assert.equal(isBPSAccountEligible(oauth), true)
  for (const flag of ['openai_responses_api', 'grok_api', 'claude_api', 'antigravity_api', 'agent_identity']) {
    assert.equal(isBPSAccountEligible({ ...oauth, [flag]: true }), false, flag)
  }
  for (const account of [undefined, null, { id: 1 }, { id: 1, account_type: 'api_key' }]) {
    assert.equal(isBPSAccountEligible(account), false)
  }
})

test('BPS patches omit unchanged flags, serialize tri-state pairs, and allow clearing invalid flags', () => {
  assert.deepEqual(buildBPSAccountPatch(oauth, 'inherit'), {})
  assert.deepEqual(buildBPSAccountPatch(oauth, 'on'), flags.on)
  assert.deepEqual(buildBPSAccountPatch(oauth, 'off'), flags.off)
  assert.deepEqual(buildBPSAccountPatch({ ...oauth, ...flags.on }, 'inherit'), flags.inherit)
  assert.deepEqual(buildBPSAccountPatch({ ...oauth, ...flags.off }, 'on'), flags.on)
  const invalid = { ...oauth, agent_identity: true, openai_excel_bps: true }
  assert.deepEqual(buildBPSAccountPatch(invalid, 'on'), {})
  assert.deepEqual(buildBPSAccountPatch(invalid, 'off'), flags.off)
  assert.deepEqual(buildBPSAccountPatch(invalid, 'inherit'), {})
  assert.deepEqual(buildBPSAccountPatch({ ...invalid, openai_excel_bps: false }, 'on'), {})
  assert.deepEqual(buildBPSAccountPatch({ id: 7, openai_excel_bps: true }, 'off'), flags.off)
})

test('batch BPS four-state changes write both flags and never overwrite unselected metadata', () => {
  assert.deepEqual(buildBatchMetadataUpdate(batch), { ids: [1, 2] })
  assert.deepEqual(buildBatchMetadataUpdate({ ...batch, bpsMode: 'unchanged' }), { ids: [1, 2] })
  for (const mode of ['on', 'off', 'inherit']) {
    assert.deepEqual(buildBatchMetadataUpdate({ ...batch, bpsMode: mode }), { ids: [1, 2], ...flags[mode] })
    assert.deepEqual(buildBatchMetadataUpdate({ ...batch, bpsMode: mode, updateTags: true }), {
      ids: [1, 2], tags: ['keep'], ...flags[mode],
    })
  }
})

test('batch eligibility handles off-page selected IDs without inferring absent detail', () => {
  const summaries = [
    oauth, { ...oauth, id: 20 },
    { ...oauth, id: 3, agent_identity: true, openai_excel_bps: true },
    { id: 4 }, { id: 6, openai_excel_bps: true },
    { id: 7, account_type: 'api_key', openai_excel_bps_opt_out: true },
    { id: 8, openai_excel_bps_opt_out: true }, { id: 9, account_type: 'api_key' },
  ]
  const ids = [1, 20, 3, 4, 5, 6, 7, 8, 9, 20]
  for (const mode of ['on', 'inherit']) {
    assert.deepEqual(selectBPSBatchAccounts(ids, summaries, mode), {
      eligibleIDs: [1, 20], unsupportedIDs: [3, 7, 9], unknownIDs: [4, 5, 6, 8],
    })
  }
  assert.deepEqual(selectBPSBatchAccounts(ids, summaries, 'off'), {
    eligibleIDs: [1, 20, 3, 6, 7, 8], unsupportedIDs: [9], unknownIDs: [4, 5],
  })
  assert.deepEqual(selectBPSBatchAccounts(ids, summaries, 'unchanged'), {
    eligibleIDs: [], unsupportedIDs: [], unknownIDs: [],
  })
})

test('quick config preserves other metadata and sends BPS only after an explicit change', () => {
  const form = formStateFromAccount({ ...oauth, tags: ['saved'], custom_headers: { 'X-Test': 'keep' }, group_ids: [7] })
  const unchanged = buildQuickConfigSavePayload(form, true)
  assert.equal(unchanged.ok, true)
  assert.equal('openai_excel_bps' in unchanged.payload, false)
  assert.equal('openai_excel_bps_opt_out' in unchanged.payload, false)
  for (const mode of ['on', 'off']) {
    const changed = buildQuickConfigSavePayload({ ...form, excelBpsMode: mode }, true)
    assert.deepEqual(changed.payload, { ...unchanged.payload, ...flags[mode] })
  }
  const invalid = formStateFromAccount({ ...oauth, openai_responses_api: true, openai_excel_bps: true })
  assert.equal(invalid.excelBpsMode, null)
  assert.equal('openai_excel_bps' in buildQuickConfigSavePayload(invalid, true).payload, false)
  assert.equal(buildQuickConfigSavePayload(form, false).ok, false)
})

test('upstream controls, health and badges replace the old master/two-state UI', () => {
  const accounts = source('../pages/Accounts.tsx')
  const quick = source('../components/AccountQuickConfigSheet.tsx')
  const settings = source('../pages/Settings.tsx')
  for (const text of [accounts, quick]) assert.doesNotMatch(text, /BPSAccountControl|BPSMasterContext|BPSPreferenceBadge|editBPSEnabled|bpsEnabled/)
  assert.match(accounts, /buildBPSAccountPatch\(editingAccount, editBPSMode\)/)
  assert.match(accounts, /<ExcelBpsStatus account=\{account\}/)
  assert.match(quick, /form\.excelBpsMode/)
  assert.match(quick, /clearAccountExcelBpsPause/)
  for (const field of ['codex_basispoints_enabled', 'codex_basispoints_models', 'codex_basispoints_403_auto_pause', 'codex_basispoints_403_probe_interval_minutes', 'codex_basispoints_429_cooldown_seconds', 'codex_basispoints_cache_creation_as_input']) {
    assert.ok(settings.includes(field), field)
  }
  assert.match(settings, /options=\{excelBpsModelOptions\(modelList\)\}/)
})
