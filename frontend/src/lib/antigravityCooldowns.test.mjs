import assert from 'node:assert/strict'
import test from 'node:test'
import { readFileSync } from 'node:fs'
import {
  antigravityCooldownReasonKey,
  antigravityCooldownSummary,
  cooldownRemainingSeconds,
  cooldownTimestamp,
  projectAntigravityCooldowns,
  safeCooldownReason,
} from './antigravityCooldowns.ts'

const now = Date.parse('2026-09-18T12:00:00Z')

test('detail projection keeps model identity, uses absolute reset, and drops credentials', () => {
  const projected = projectAntigravityCooldowns({
    access_token: 'never-retain-this',
    credentials: { refresh_token: 'never-retain-this-either' },
    model_cooldowns: [{
      model: 'gemini-3.7-flash-high',
      reason: 'quota_exhausted',
      reset_at: '2026-09-18T12:02:00Z',
      remaining_seconds: 900,
      secret: 'also-drop-extra-model-fields',
    }],
  }, now)
  assert.deepEqual(projected, [{
    model: 'gemini-3.7-flash-high',
    reason: 'quota_exhausted',
    resetAt: '2026-09-18T12:02:00Z',
    deadline: now + 120_000,
  }])
  assert.equal(cooldownRemainingSeconds(projected[0].deadline, now), 120)
  assert.equal(cooldownRemainingSeconds(projected[0].deadline, now + 119_001), 1)
  assert.equal(cooldownRemainingSeconds(projected[0].deadline, now + 121_000), 0)
})

test('missing timestamps anchor remaining_seconds once at receipt, without inventing a reset date', () => {
  const row = projectAntigravityCooldowns({ model_cooldowns: [{
    model: 'gemini-family', reason: 'rate_limited_model', reset_at: 'invalid', remaining_seconds: 10,
  }] }, now)[0]
  assert.equal(row.resetAt, null)
  assert.equal(cooldownRemainingSeconds(row.deadline, now + 4_000), 6)
  for (const invalid of [-1, NaN, Infinity, undefined]) {
    const invalidRow = projectAntigravityCooldowns({ model_cooldowns: [{
      model: 'unchanged-model', reason: '', reset_at: '', remaining_seconds: invalid,
    }] }, now)[0]
    assert.equal(invalidRow.deadline, null)
    assert.equal(cooldownRemainingSeconds(invalidRow.deadline, now), null)
  }
  assert.equal(cooldownTimestamp('bad date'), null)
  assert.equal(cooldownTimestamp(undefined), null)
  assert.deepEqual(projectAntigravityCooldowns({}, now), [])
})

test('expired absolute timestamps never restart from a stale remaining_seconds snapshot', () => {
  const row = projectAntigravityCooldowns({ model_cooldowns: [{
    model: 'gemini', reason: 'quota_exhausted', reset_at: '2026-09-18T11:59:00Z', remaining_seconds: 500,
  }] }, now)[0]
  assert.equal(cooldownRemainingSeconds(row.deadline, now), 0)
})

test('list summary only reads existing account-level cooldown fields', () => {
  const account = {
    cooldown_until: '2026-09-18T12:02:00Z',
    cooldown_reason: 'rate_limited',
    get model_cooldowns() { throw new Error('list must not inspect model details') },
  }
  assert.deepEqual(antigravityCooldownSummary(account, now), {
    resetAt: '2026-09-18T12:02:00Z', reason: 'rate_limited',
  })
  assert.equal(antigravityCooldownSummary(account, now + 120_000), null)
  assert.equal(antigravityCooldownSummary({}, now), null)
  assert.deepEqual(antigravityCooldownSummary({ cooldown_reason: 'unauthorized', cooldown_until: 'bad' }, now), {
    resetAt: null, reason: 'unauthorized',
  })
})

test('reason localization uses known keys and bounds unknown plain text safely', () => {
  assert.equal(antigravityCooldownReasonKey(' QUOTA_EXHAUSTED '), 'antigravity.cooldowns.reasonQuotaExhausted')
  assert.equal(antigravityCooldownReasonKey('rate_limited_model'), 'antigravity.cooldowns.reasonModelRateLimit')
  assert.equal(antigravityCooldownReasonKey('unauthorized'), 'status.unauthorized')
  assert.equal(antigravityCooldownReasonKey('__proto__'), null)
  assert.equal(antigravityCooldownReasonKey('toString'), null)
  assert.equal(antigravityCooldownReasonKey('future_reason'), null)
  assert.equal(safeCooldownReason('\u202efuture\n_reason\u0000'), 'future_reason')
  assert.equal(safeCooldownReason('x'.repeat(200)).length, 160)
})

test('component preserves account isolation, request fencing, and shared timer guardrails', () => {
  const source = readFileSync(new URL('../components/AntigravityCooldownDetails.tsx', import.meta.url), 'utf8')
  const summary = source.slice(source.indexOf('export function AntigravityCooldownSummary'), source.indexOf('export interface AntigravityCooldownDetailsProps'))
  assert.doesNotMatch(summary, /api\.|setInterval|useEffect/)
  assert.match(source, /CooldownDetailsSession key=\{props\.accountId\}/)
  assert.match(source, /controller\.current\?\.abort\(\)/)
  assert.match(source, /id === requestId\.current/)
  assert.match(source, /generation === lifetime\.current/)
  assert.match(source, /mutationInFlight\.current/)
  assert.match(source, /api\.getAccount\(accountId, requestController\.signal\)/)
  assert.match(source, /projectAntigravityCooldowns\(account, receivedAt\)/)
  assert.match(source, /await reload\(\)[\s\S]*?if \(!isCurrent\(\)\) return[\s\S]*?onChangedRef\.current\?\.\(\)/)
  assert.equal((source.match(/setInterval\(/g) ?? []).length, 1)
  assert.match(source, /window\.clearInterval\(timer\)/)
  assert.match(source, /<Modal/)
  assert.match(source, /confirmAll/)
  assert.match(source, /confirmModel/)
  assert.doesNotMatch(source, /console\.|localStorage|sessionStorage|dangerouslySetInnerHTML/)
})
