import assert from 'node:assert/strict'
import test from 'node:test'
import {
  aiCredits,
  detectCredentialInput,
  mergeAntigravityImportResponses,
  planAntigravityImportBatches,
  isQuotaStale,
  lowestModelRemaining,
  modelQuotaEntries,
  normalizeFraction,
  parseModelList,
  parseRefreshTokenLines,
  quotaWindowKey,
  shortQuotaGroupLabel,
  summarizeQuotaGroups,
  tierEntries,
} from './antigravityQuota.ts'

const snapshot = {
  models: [
    { model_id: 'claude-opus-5-5-high', remaining_fraction: 0.9992 },
    { model_id: 'gemini-3-flash', remaining_fraction: 1 },
    { model_id: 'gpt-oss-120b-medium', remaining_fraction: 0.4 },
  ],
  quota_groups: [
    {
      display_name: 'Gemini Models',
      description: 'Models within this group: Gemini Flash, Gemini Pro',
      buckets: [
        { bucket_id: 'gemini-weekly', window: 'weekly', remaining_fraction: 0.98907405, reset_time: '2026-10-10T09:10:46Z' },
        { bucket_id: 'gemini-5h', window: '5h', remaining_fraction: 1, reset_time: '2026-10-08T13:24:39Z' },
      ],
    },
    {
      display_name: 'Claude and GPT models',
      buckets: [
        { bucket_id: '3p-weekly', window: 'weekly', remaining_fraction: 0.25 },
        { bucket_id: '3p-5h', window: '5h', remaining_fraction: 0.6 },
      ],
    },
  ],
  ai_credits: { credits: 0 },
  updated_at: '2026-10-08T13:24:42Z',
}

test('quota groups summarise 5h before weekly with the tightest bucket as lowest', () => {
  const groups = summarizeQuotaGroups(snapshot)
  assert.equal(groups.length, 2)
  assert.deepEqual(groups.map((group) => group.label), ['Gemini', 'Claude / GPT'])
  assert.deepEqual(groups[0].windows.map((window) => window.window), ['5h', 'weekly'])
  assert.equal(groups[0].windows[1].resetAt, '2026-10-10T09:10:46Z')
  assert.equal(groups[1].lowest, 0.25)
  assert.match(groups[0].description, /Gemini Flash/)
})

test('legacy groups field and camelCase buckets are still read', () => {
  const groups = summarizeQuotaGroups({
    models: [],
    groups: [{ displayName: 'Weekly', buckets: [{ bucketId: 'x', window: '7d', remainingFraction: 50 }] }],
  })
  assert.equal(groups.length, 1)
  assert.equal(groups[0].windows[0].window, 'weekly')
  assert.equal(groups[0].windows[0].fraction, 0.5)
})

test('group labels are shortened and empty names fall back to an index', () => {
  assert.equal(shortQuotaGroupLabel('Gemini Models', 0), 'Gemini')
  assert.equal(shortQuotaGroupLabel('Claude and GPT models', 1), 'Claude / GPT')
  assert.equal(shortQuotaGroupLabel('  ', 2), '#3')
})

test('window keys recognise common spellings', () => {
  assert.equal(quotaWindowKey('5h'), '5h')
  assert.equal(quotaWindowKey('WEEKLY'), 'weekly')
  assert.equal(quotaWindowKey('daily'), 'other')
})

test('model entries accept array and object shapes and report the lowest remaining fraction', () => {
  assert.deepEqual(modelQuotaEntries(snapshot).map(([model]) => model), [
    'claude-opus-5-5-high',
    'gemini-3-flash',
    'gpt-oss-120b-medium',
  ])
  assert.equal(lowestModelRemaining(snapshot), 0.4)
  assert.deepEqual(
    modelQuotaEntries({ models: { 'gemini-2.5-pro': { remaining_fraction: 0.75 } } }).map(([model]) => model),
    ['gemini-2.5-pro'],
  )
  assert.equal(lowestModelRemaining(undefined), null)
})

test('fractions accept percentages and clamp out-of-range values', () => {
  assert.equal(normalizeFraction(75), 0.75)
  assert.equal(normalizeFraction(-1), 0)
  assert.equal(normalizeFraction('1'), null)
})

test('ai credits are only reported when the snapshot carries a numeric balance', () => {
  assert.deepEqual(aiCredits(snapshot), { credits: 0, expiryDate: '' })
  assert.equal(aiCredits({ models: [] }), null)
})

test('quota snapshots older than six hours are stale', () => {
  const now = Date.parse('2026-10-08T20:00:00Z')
  assert.equal(isQuotaStale('2026-10-08T13:24:42Z', now), true)
  assert.equal(isQuotaStale('2026-10-08T18:00:00Z', now), false)
  assert.equal(isQuotaStale('', now), false)
})

test('model and refresh token inputs are split, trimmed and de-duplicated', () => {
  assert.deepEqual(parseModelList('a, b\n\na\nc'), ['a', 'b', 'c'])
  assert.deepEqual(parseRefreshTokenLines('1//rt-a\r\n\n# note\n1//rt-b\n1//rt-a'), ['1//rt-a', '1//rt-b'])
})

test('same-name tiers keep their ids and default marker, exact duplicates collapse', () => {
  const entries = tierEntries([
    { id: 'free-tier', name: 'Antigravity', is_default: true },
    { id: 'standard-tier', name: 'Antigravity' },
    { id: 'standard-tier', name: 'Antigravity' },
    'Legacy',
  ])
  assert.deepEqual(
    entries.map(({ name, id, isDefault }) => ({ name, id, isDefault })),
    [
      { name: 'Antigravity', id: 'free-tier', isDefault: true },
      { name: 'Antigravity', id: 'standard-tier', isDefault: false },
      { name: 'Legacy', id: '', isDefault: false },
    ],
  )
  assert.deepEqual(tierEntries(undefined), [])
})

test('single credential input is classified for live feedback', () => {
  assert.equal(detectCredentialInput('  '), 'empty')
  assert.equal(detectCredentialInput('{"refresh_token":"1//abc"}'), 'json')
  assert.equal(detectCredentialInput('[{"a":1}]'), 'json')
  assert.equal(detectCredentialInput('{"refresh_token":'), 'json_invalid')
  assert.equal(detectCredentialInput('1//0gAbC-d_e.f'), 'refresh_token')
  assert.equal(detectCredentialInput('ya29.something'), 'unknown')
})

test('import batches keep small selections intact and split large exported arrays', () => {
  const small = ['{"type":"antigravity"}', '[{"type":"antigravity"},{"type":"antigravity"}]']
  assert.deepEqual(planAntigravityImportBatches(small, 5), [small])
  assert.deepEqual(planAntigravityImportBatches([], 5), [])

  const exported = JSON.stringify(Array.from({ length: 7 }, (_, index) => ({ type: 'antigravity', refresh_token: `rt-${index}` })))
  const wrapped = JSON.stringify({ accounts: [{ refresh_token: 'rt-w' }] })
  const batches = planAntigravityImportBatches([exported, wrapped, '1//raw-token', '{broken'], 3)
  assert.deepEqual(batches.map((batch) => batch.length), [3, 3, 3, 1])
  assert.equal(JSON.parse(batches[0][0]).refresh_token, 'rt-0')
  assert.equal(JSON.parse(batches[2][1]).refresh_token, 'rt-w')
  assert.equal(batches[2][2], '1//raw-token')
  assert.equal(batches[3][0], '{broken')
})

test('batched import results add up and item positions become global', () => {
  const merged = mergeAntigravityImportResponses([
    { offset: 0, response: { total: 2, imported: 2, synced: 1, degraded: 1, failed: 0, items: [{ index: 2, ok: true, synced: false, warning: 'w' }] } },
    { offset: 500, response: { total: 1, imported: 0, failed: 1, items: [{ index: 1, ok: false, error: 'bad' }], proxies_imported: 1, proxies_skipped: 0 } },
  ])
  assert.equal(merged.total, 3)
  assert.equal(merged.imported, 2)
  assert.equal(merged.synced, 1)
  assert.equal(merged.degraded, 1)
  assert.equal(merged.failed, 1)
  assert.deepEqual(merged.items.map((item) => item.index), [2, 501])
  assert.equal(merged.proxies_imported, 1)
})
