import { useCallback, useEffect, useRef, useState } from 'react'
import { api } from '../api'
import type { SystemUpdateInfo, UpdateSource } from '../types'
import { resolveBuildVersions, versionLabel } from '../lib/versionCheck'

// Plans stay in memory, isolated by source; never reuse a persisted installation token.
const cache = new Map<UpdateSource, { info: SystemUpdateInfo; checkedAt: number }>()
const sequences: Record<UpdateSource, number> = { patched: 0, official: 0 }
const CACHE_TTL = 60_000
const POLL_INTERVAL = 30 * 60_000

export function useVersionCheck(triggerKey?: string, source: UpdateSource = 'patched', enabled = true) {
  const [state, setState] = useState<{ source: UpdateSource; info: SystemUpdateInfo | null }>({ source, info: null })
  const [backendVersion, setBackendVersion] = useState<string | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState(false)
  const activeRef = useRef(false)
  const requestRef = useRef(0)
  const healthRequestRef = useRef(0)
  const lastTriggerRef = useRef(triggerKey)
  const versions = resolveBuildVersions(__APP_VERSION__, backendVersion)

  const check = useCallback(async (forceNetwork = false) => {
    if (!enabled || !activeRef.current) return
    const request = ++requestRef.current
    const healthRequest = ++healthRequestRef.current
    setLoading(true)
    setError(false)

    // Runtime identity is independent of remote discovery and source-specific plans.
    const healthPending = (async () => {
      let runtimeVersion: string | null = null
      try {
        const health = await api.getHealth({ timeoutMs: 5_000 })
        runtimeVersion = typeof health.build_version === 'string' ? health.build_version : null
      } catch {
        // Legacy servers and failed probes keep the frontend build label.
      }
      if (activeRef.current && healthRequest === healthRequestRef.current) {
        setBackendVersion(runtimeVersion)
      }
    })()

    const updatePending = (async () => {
      let sequence: number | undefined
      try {
        const cached = cache.get(source)
        let info: SystemUpdateInfo
        if (!forceNetwork && cached && Date.now() - cached.checkedAt < CACHE_TTL) {
          info = cached.info
        } else {
          sequence = ++sequences[source]
          info = await api.getSystemUpdate(source)
          if (info.source !== source) throw new Error('Unexpected update source')
          if (activeRef.current && request === requestRef.current && sequence === sequences[source]) {
            cache.set(source, { info, checkedAt: Date.now() })
          }
        }
        if (activeRef.current && request === requestRef.current) setState({ source, info })
      } catch {
        if (activeRef.current && request === requestRef.current) {
          if (sequence === sequences[source]) cache.delete(source)
          setState({ source, info: null })
          setError(true)
        }
      } finally {
        if (activeRef.current && request === requestRef.current) setLoading(false)
      }
    })()

    await Promise.all([healthPending, updatePending])
  }, [source, enabled])

  useEffect(() => {
    activeRef.current = enabled
    if (enabled) void check()
    const timer = enabled ? setInterval(() => void check(), POLL_INTERVAL) : undefined
    return () => {
      activeRef.current = false
      requestRef.current += 1
      healthRequestRef.current += 1
      clearInterval(timer)
    }
  }, [check, enabled])
  useEffect(() => {
    if (lastTriggerRef.current === triggerKey) return
    lastTriggerRef.current = triggerKey
    void check(true)
  }, [check, triggerKey])

  const updateInfo = state.source === source ? state.info : null
  return {
    ...versions,
    // Only the source-bound backend plan may advertise an update; health is diagnostic.
    hasUpdate: updateInfo?.status === 'available' && updateInfo.has_update,
    latestVersion: versionLabel(updateInfo?.latest_version),
    updateInfo, refreshVersion: check, loading, error,
  }
}
