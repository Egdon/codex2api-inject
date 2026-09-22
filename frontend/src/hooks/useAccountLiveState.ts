import { useEffect, useMemo } from 'react'
import { api } from '../api'
import type { AccountLiveStateResponse, CodexTurnStateStatus } from '../types'

// Merge a live poll response into an account list. Rows whose live concurrency
// counters did not change keep their object identity, and a no-op poll returns the
// original array, so the 1s polling cadence cannot defeat row-level memoization.
export function mergeAccountLiveState<T extends {
  id: number
  codex_turn_state_status?: CodexTurnStateStatus
  active_requests?: number
  occupied_requests?: number
  session_slot_buffer_enabled?: boolean
}>(
  current: T[],
  response: AccountLiveStateResponse,
): T[] {
  let changed = false
  const next = current.map((account) => {
    const live = response.accounts[String(account.id)]
    const activeRequests = response.accounts[String(account.id)]?.active_requests ?? 0
    const occupiedRequests = response.accounts[String(account.id)]?.occupied_requests ?? activeRequests
    const slotBufferEnabled = response.session_slot_buffer_enabled === true
    if (
      (account.active_requests ?? 0) === activeRequests &&
      (account.occupied_requests ?? account.active_requests ?? 0) === occupiedRequests &&
      (account.session_slot_buffer_enabled ?? false) === slotBufferEnabled &&
      (!live || JSON.stringify(account.codex_turn_state_status) === JSON.stringify(live.codex_turn_state_status))
    ) return account
    changed = true
    return {
      ...account,
      active_requests: activeRequests,
      occupied_requests: occupiedRequests,
      session_slot_buffer_enabled: slotBufferEnabled,
      ...(live ? { codex_turn_state_status: live.codex_turn_state_status } : {}),
    }
  })
  return changed ? next : current
}

export function useAccountLiveState(
  ids: number[],
  apply: (response: AccountLiveStateResponse) => void,
  enabled = true,
) {
  const idsKey = useMemo(() => ids.join(','), [ids])

  useEffect(() => {
    if (!enabled || !idsKey) return undefined
    let stopped = false
    let timer: number | undefined
    let controller: AbortController | undefined

    const poll = async () => {
      if (stopped) return
      if (document.hidden) {
        timer = window.setTimeout(poll, 1000)
        return
      }
      controller?.abort()
      controller = new AbortController()
      try {
        const response = await api.getAccountLiveState(
          idsKey.split(',').map(Number),
          controller.signal,
        )
        if (!stopped && !controller.signal.aborted) apply(response)
      } catch {
        // The next tick retries; live counters must never disrupt the page.
      }
      if (!stopped) timer = window.setTimeout(poll, 1000)
    }

    void poll()
    return () => {
      stopped = true
      controller?.abort()
      if (timer !== undefined) window.clearTimeout(timer)
    }
  }, [apply, enabled, idsKey])
}
