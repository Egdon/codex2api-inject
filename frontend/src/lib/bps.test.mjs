import assert from 'node:assert/strict'
import test from 'node:test'
import { isBPSAccountEligible, isBPSMasterEnabled, buildBPSAccountPatch, selectBPSBatchAccounts } from './bps.ts'
import { buildBatchMetadataUpdate } from './accountBatchUpdate.ts'
import { formStateFromAccount, buildQuickConfigSavePayload } from './accountQuickConfig.ts'

const oauth = { id: 1, account_type: 'oauth' }
const batch = {
  ids: [1, 2], updateTags: false, tags: ['keep'], updateGroups: false, groupIds: [9],
  updateScoreBias: false, scoreBias: null, updateBaseConcurrency: false, baseConcurrency: null,
  updateSchedulerPriority: false, schedulerPriority: null,
}

test('BPS master defaults enabled but account preference defaults off', () => {
  assert.equal(isBPSMasterEnabled(undefined), true)
  assert.equal(isBPSMasterEnabled(true), true)
  assert.equal(isBPSMasterEnabled(false), false)
  assert.equal(formStateFromAccount(oauth).bpsEnabled, false)
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

test('BPS patches are isolated, omit unchanged fields, and allow clearing invalid flags', () => {
  assert.deepEqual(buildBPSAccountPatch(oauth, false), {})
  assert.deepEqual(buildBPSAccountPatch(oauth, true), { openai_excel_bps: true })
  const invalid = { ...oauth, agent_identity: true, openai_excel_bps: true }
  assert.deepEqual(buildBPSAccountPatch(invalid, true), {})
  assert.deepEqual(buildBPSAccountPatch(invalid, false), { openai_excel_bps: false })
  assert.deepEqual(buildBPSAccountPatch({ ...invalid, openai_excel_bps: false }, true), {})
})

test('batch BPS tri-state never overwrites unselected metadata', () => {
  assert.deepEqual(buildBatchMetadataUpdate({ ...batch, bpsMode: 'unchanged' }), { ids: [1, 2] })
  assert.deepEqual(buildBatchMetadataUpdate({ ...batch, bpsMode: 'on' }), { ids: [1, 2], openai_excel_bps: true })
  assert.deepEqual(buildBatchMetadataUpdate({ ...batch, bpsMode: 'off' }), { ids: [1, 2], openai_excel_bps: false })
  assert.deepEqual(buildBatchMetadataUpdate({ ...batch, bpsMode: 'on', updateTags: true }), {
    ids: [1, 2], tags: ['keep'], openai_excel_bps: true,
  })
})

test('batch eligibility handles off-page selected IDs without inferring absent detail', () => {
  const summaries = [oauth, { ...oauth, id: 20 }, { ...oauth, id: 3, agent_identity: true, openai_excel_bps: true }, { id: 4 }, { id: 6, openai_excel_bps: true }]
  const ids = [1, 20, 3, 4, 5, 6, 20]
  assert.deepEqual(selectBPSBatchAccounts(ids, summaries, 'on'), {
    eligibleIDs: [1, 20], unsupportedIDs: [3], unknownIDs: [4, 5, 6],
  })
  assert.deepEqual(selectBPSBatchAccounts(ids, summaries, 'off'), {
    eligibleIDs: [1, 20, 3, 6], unsupportedIDs: [], unknownIDs: [4, 5],
  })
})

test('quick config preserves other metadata and sends BPS only after an explicit change', () => {
  const form = formStateFromAccount({ ...oauth, tags: ['saved'], custom_headers: { 'X-Test': 'keep' }, group_ids: [7] })
  const unchanged = buildQuickConfigSavePayload(form, true)
  assert.equal(unchanged.ok, true)
  assert.equal('openai_excel_bps' in unchanged.payload, false)
  const changed = buildQuickConfigSavePayload({ ...form, bpsEnabled: true }, true)
  assert.deepEqual(changed.payload, { ...unchanged.payload, openai_excel_bps: true })
  const invalid = formStateFromAccount({ ...oauth, openai_responses_api: true, openai_excel_bps: true })
  assert.equal(buildQuickConfigSavePayload({ ...invalid, bpsEnabled: false }, true).payload.openai_excel_bps, false)
  assert.equal(buildQuickConfigSavePayload(form, false).ok, false)
})
