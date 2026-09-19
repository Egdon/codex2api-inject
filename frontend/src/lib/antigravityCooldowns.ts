import type { AccountRow } from '../types'

export interface AntigravityModelCooldown {
  model: string
  reason: string
  resetAt: string | null
  deadline: number | null
}

export function cooldownTimestamp(value: string | null | undefined): number | null {
  if (!value?.trim()) return null
  const timestamp = Date.parse(value)
  return Number.isFinite(timestamp) ? timestamp : null
}

/** Project only cooldown fields: never retain account credentials in component state. */
export function projectAntigravityCooldowns(
  account: Pick<AccountRow, 'model_cooldowns'>,
  receivedAt: number,
): AntigravityModelCooldown[] {
  return (account.model_cooldowns ?? []).map((entry) => {
    const reset = cooldownTimestamp(entry.reset_at)
    const seconds = entry.remaining_seconds
    const fallback = Number.isFinite(seconds) && seconds >= 0
      ? receivedAt + seconds * 1000
      : null
    return {
      model: entry.model,
      reason: entry.reason,
      resetAt: reset === null ? null : entry.reset_at,
      deadline: reset ?? (fallback !== null && Number.isFinite(fallback) ? fallback : null),
    }
  })
}

export function cooldownRemainingSeconds(deadline: number | null, now: number): number | null {
  return deadline === null ? null : Math.max(0, Math.ceil((deadline - now) / 1000))
}

/** Uses only the summary fields already present in the list response. */
export function antigravityCooldownSummary(
  account: Pick<AccountRow, 'cooldown_until' | 'cooldown_reason'>,
  now: number,
): { resetAt: string | null; reason: string } | null {
  const reset = cooldownTimestamp(account.cooldown_until)
  const reason = account.cooldown_reason?.trim() ?? ''
  if (reset !== null && reset <= now) return null
  if (reset === null && !reason) return null
  return { resetAt: reset === null ? null : account.cooldown_until!, reason }
}

const reasonKeys: Record<string, string> = {
  rate_limited: 'status.rate_limited',
  rate_limited_model: 'antigravity.cooldowns.reasonModelRateLimit',
  quota_exhausted: 'antigravity.cooldowns.reasonQuotaExhausted',
  resource_exhausted: 'antigravity.cooldowns.reasonQuotaExhausted',
  unauthorized: 'status.unauthorized',
  cooldown: 'status.cooldown',
  overload_paused: 'status.overload_paused',
  rate_limited_5h: 'status.rate_limited_5h',
  rate_limited_7d: 'status.rate_limited_7d',
  quota_paused: 'status.quota_paused',
  usage_exhausted: 'status.usage_exhausted',
  usage_limited: 'status.usage_limited',
}

export function antigravityCooldownReasonKey(reason: string): string | null {
  const normalized = reason.trim().toLowerCase()
  return Object.prototype.hasOwnProperty.call(reasonKeys, normalized) ? reasonKeys[normalized] : null
}

/** Unknown server reason codes are bounded, plain text, never HTML. */
export function safeCooldownReason(reason: string): string {
  return reason.replace(/[\u0000-\u001f\u007f-\u009f\u202a-\u202e\u2066-\u2069]/g, '').trim().slice(0, 160)
}
