import { useCallback, useEffect, useRef, useState } from 'react'
import { api } from '../api'
import type { SystemUpdateInfo, UpdateSource } from '../types'

// Plans stay in memory, isolated by source; never reuse a persisted installation token.
const cache = new Map<UpdateSource, { info: SystemUpdateInfo; checkedAt: number }>()
const sequences: Record<UpdateSource, number> = { patched: 0, official: 0 }
const CACHE_TTL = 60_000

export function useVersionCheck(triggerKey?: string, source: UpdateSource = 'patched', enabled = true) {
  const [state, setState] = useState<{ source: UpdateSource; info: SystemUpdateInfo | null }>({ source, info: null })
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState(false)
  const requestRef = useRef(0)
  const lastTriggerRef = useRef(triggerKey)
  const check = useCallback(async (forceNetwork = false) => {
    if (!enabled) return
    const request = ++requestRef.current
    setLoading(true)
    setError(false)
    try {
      const cached = cache.get(source)
      let info: SystemUpdateInfo
      if (!forceNetwork && cached && Date.now() - cached.checkedAt < CACHE_TTL) {
        info = cached.info
      } else {
        const sequence = ++sequences[source]
        info = await api.getSystemUpdate(source)
        if (info.source !== source) throw new Error('Unexpected update source')
        if (sequence === sequences[source]) cache.set(source, { info, checkedAt: Date.now() })
      }
      if (request === requestRef.current) setState({ source, info })
    } catch {
      cache.delete(source)
      if (request === requestRef.current) {
        setState({ source, info: null })
        setError(true)
      }
    } finally {
      if (request === requestRef.current) setLoading(false)
    }
  }, [source, enabled])

  useEffect(() => {
    void check()
    const timer = setInterval(() => void check(), 30 * 60_000)
    return () => { clearInterval(timer); requestRef.current += 1 }
  }, [check])
  useEffect(() => {
    if (lastTriggerRef.current === triggerKey) return
    lastTriggerRef.current = triggerKey
    void check(true)
  }, [check, triggerKey])

  const updateInfo = state.source === source ? state.info : null
  const latest = updateInfo?.latest_version
  return {
    hasUpdate: updateInfo?.status === 'available' && updateInfo.has_update,
    latestVersion: latest ? (/^v/i.test(latest) ? latest : `v${latest}`) : null,
    updateInfo, refreshVersion: check, loading, error,
  }
}
