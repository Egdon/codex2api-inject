import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { api } from '../api'
import type { AccountRow } from '../types'
import {
  antigravityCooldownReasonKey,
  antigravityCooldownSummary,
  cooldownRemainingSeconds,
  projectAntigravityCooldowns,
  safeCooldownReason,
  type AntigravityModelCooldown,
} from '../lib/antigravityCooldowns'
import { formatBeijingTime } from '../utils/time'
import Modal from './Modal'
import { Button } from './ui/button'

function CooldownReason({ reason }: { reason: string }) {
  const { t } = useTranslation()
  const key = antigravityCooldownReasonKey(reason)
  return <>{key ? t(key) : safeCooldownReason(reason) || t('antigravity.cooldowns.reasonUnknown')}</>
}

/** No requests or timers here: list rows use their existing account summary only. */
export function AntigravityCooldownSummary({ account }: { account: AccountRow }) {
  const { t } = useTranslation()
  const summary = antigravityCooldownSummary(account, Date.now())
  if (!summary) return null
  return (
    <div className="min-w-0 space-y-0.5 text-xs text-muted-foreground">
      <div className="break-words">
        {t('antigravity.cooldowns.summary')}: <CooldownReason reason={summary.reason} />
      </div>
      <div>{t('antigravity.cooldowns.resetAt')}: {formatBeijingTime(summary.resetAt, t('antigravity.cooldowns.resetUnknown'))}</div>
    </div>
  )
}

export interface AntigravityCooldownDetailsProps {
  accountId: number
  onChanged?: () => void
}

/** Mount only inside an open detail view. Keying also isolates pending mutations on account switch. */
export function AntigravityCooldownDetails(props: AntigravityCooldownDetailsProps) {
  return <CooldownDetailsSession key={props.accountId} {...props} />
}

function CooldownDetailsSession({ accountId, onChanged }: AntigravityCooldownDetailsProps) {
  const { t } = useTranslation()
  const [rows, setRows] = useState<AntigravityModelCooldown[]>([])
  const [loadStatus, setLoadStatus] = useState<'loading' | 'ready' | 'error'>('loading')
  const [now, setNow] = useState(Date.now)
  const [confirmation, setConfirmation] = useState<{ model: string } | 'all' | null>(null)
  const [clearing, setClearing] = useState(false)
  const [actionStatus, setActionStatus] = useState<'error' | 'success' | 'refresh-error' | null>(null)
  const mounted = useRef(false)
  const lifetime = useRef(0)
  const requestId = useRef(0)
  const controller = useRef<AbortController | null>(null)
  const mutationInFlight = useRef(false)
  const onChangedRef = useRef(onChanged)

  useLayoutEffect(() => {
    onChangedRef.current = onChanged
  }, [onChanged])

  useLayoutEffect(() => {
    mounted.current = true
    return () => {
      mounted.current = false
      lifetime.current += 1
      requestId.current += 1
      controller.current?.abort()
    }
  }, [])

  const reload = useCallback(async () => {
    if (!mounted.current) return
    controller.current?.abort()
    const requestController = new AbortController()
    controller.current = requestController
    const id = ++requestId.current
    const isCurrent = () => mounted.current && id === requestId.current && !requestController.signal.aborted
    setLoadStatus('loading')
    setRows([])
    try {
      // The common detail endpoint returns AccountRow directly, not an envelope.
      // Project immediately; never retain, log, or persist the credential-bearing response.
      const account = await api.getAccount(accountId, requestController.signal)
      if (!isCurrent()) return
      const receivedAt = Date.now()
      setRows(projectAntigravityCooldowns(account, receivedAt))
      setNow(receivedAt)
      setLoadStatus('ready')
    } catch {
      if (!isCurrent()) return
      setLoadStatus('error')
    }
  }, [accountId])

  useEffect(() => {
    void reload()
    return () => {
      requestId.current += 1
      controller.current?.abort()
    }
  }, [reload])

  // One bounded timer per mounted detail view, shared by every model; no auto-fetch loop.
  useEffect(() => {
    if (!rows.some((row) => row.deadline !== null && row.deadline > Date.now())) return
    const timer = window.setInterval(() => {
      const timestamp = Date.now()
      setNow(timestamp)
      if (!rows.some((row) => row.deadline !== null && row.deadline > timestamp)) {
        window.clearInterval(timer)
      }
    }, 1000)
    return () => window.clearInterval(timer)
  }, [rows])

  const clearConfirmed = async () => {
    if (!confirmation || mutationInFlight.current || loadStatus !== 'ready') return
    const target = confirmation
    const generation = lifetime.current
    const isCurrent = () => mounted.current && generation === lifetime.current
    mutationInFlight.current = true
    setClearing(true)
    setActionStatus(null)
    try {
      if (target === 'all') await api.clearAllAccountModelCooldowns(accountId)
      else await api.clearAccountModelCooldown(accountId, target.model)
    } catch {
      if (isCurrent()) {
        setActionStatus('error')
        setConfirmation(null)
        setClearing(false)
        mutationInFlight.current = false
      }
      return
    }
    if (!isCurrent()) return
    setConfirmation(null)
    setActionStatus('success')
    // Refresh details even if the parent callback changes; never update a newer account view.
    await reload()
    if (!isCurrent()) return
    try {
      // A void callback may still be implemented by an async parent refresh.
      await onChangedRef.current?.()
    } catch {
      if (isCurrent()) setActionStatus('refresh-error')
    } finally {
      if (isCurrent()) {
        setClearing(false)
        mutationInFlight.current = false
      }
    }
  }

  const remainingLabel = (row: AntigravityModelCooldown) => {
    const seconds = cooldownRemainingSeconds(row.deadline, now)
    if (seconds === null) return t('antigravity.cooldowns.resetUnknown')
    if (seconds === 0) return t('antigravity.cooldowns.expired')
    return t('antigravity.cooldowns.countdown', {
      hours: Math.floor(seconds / 3600),
      minutes: Math.floor((seconds % 3600) / 60),
      seconds: seconds % 60,
    })
  }

  return (
    <section className="space-y-3 rounded-xl border border-border bg-card p-4 text-card-foreground" aria-label={t('antigravity.cooldowns.title')}>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h3 className="font-semibold">{t('antigravity.cooldowns.title')}</h3>
        <div className="flex flex-wrap gap-2">
          <Button type="button" variant="outline" size="sm" disabled={clearing || loadStatus === 'loading'} onClick={() => { setActionStatus(null); void reload() }}>
            {t('antigravity.cooldowns.refresh')}
          </Button>
          <Button type="button" variant="outline" size="sm" disabled={clearing || loadStatus !== 'ready' || rows.length === 0} onClick={() => setConfirmation('all')}>
            {t('antigravity.cooldowns.clearAll')}
          </Button>
        </div>
      </div>
      <p className="text-xs text-muted-foreground">{t('antigravity.cooldowns.hint')}</p>
      {actionStatus && (
        <p role={actionStatus === 'success' ? 'status' : 'alert'} className={actionStatus === 'success' ? 'text-sm text-muted-foreground' : 'text-sm text-destructive'}>
          {actionStatus === 'error'
            ? t('antigravity.cooldowns.clearFailed')
            : actionStatus === 'refresh-error'
              ? t('antigravity.cooldowns.parentRefreshFailed')
              : t('antigravity.cooldowns.clearSuccess')}
        </p>
      )}
      {loadStatus === 'loading' ? (
        <p role="status" className="text-sm text-muted-foreground">{t('antigravity.cooldowns.loading')}</p>
      ) : loadStatus === 'error' ? (
        <div role="alert" className="flex flex-wrap items-center gap-2 text-sm text-destructive">
          <span>{t('antigravity.cooldowns.loadFailed')}</span>
          <Button type="button" variant="outline" size="sm" disabled={clearing} onClick={() => void reload()}>{t('antigravity.cooldowns.retry')}</Button>
        </div>
      ) : rows.length === 0 ? (
        <p className="text-sm text-muted-foreground">{t('antigravity.cooldowns.empty')}</p>
      ) : (
        <ul className="space-y-2">
          {rows.map((row) => (
            <li key={row.model} className="flex flex-wrap items-start justify-between gap-3 rounded-lg border border-border bg-muted/30 p-3">
              <dl className="min-w-0 flex-1 space-y-1 text-sm">
                <div><dt className="sr-only">{t('antigravity.cooldowns.model')}</dt><dd className="break-all font-medium">{row.model}</dd></div>
                <div className="break-words text-muted-foreground"><dt className="inline">{t('antigravity.cooldowns.reason')}: </dt><dd className="inline"><CooldownReason reason={row.reason} /></dd></div>
                <div className="text-muted-foreground"><dt className="inline">{t('antigravity.cooldowns.resetAt')}: </dt><dd className="inline">{formatBeijingTime(row.resetAt, t('antigravity.cooldowns.resetUnknown'))}</dd></div>
                <div className="text-muted-foreground"><dt className="inline">{t('antigravity.cooldowns.remaining')}: </dt><dd className="inline tabular-nums">{remainingLabel(row)}</dd></div>
              </dl>
              <Button type="button" variant="outline" size="sm" disabled={clearing} aria-label={t('antigravity.cooldowns.clearModelLabel', { model: row.model })} onClick={() => setConfirmation({ model: row.model })}>
                {t('antigravity.cooldowns.clearModel')}
              </Button>
            </li>
          ))}
        </ul>
      )}
      <Modal
        show={confirmation !== null}
        title={t('antigravity.cooldowns.confirmTitle')}
        onClose={() => { if (!clearing) setConfirmation(null) }}
        showCloseButton={!clearing}
        footer={<>
          <Button type="button" variant="outline" disabled={clearing} onClick={() => setConfirmation(null)}>{t('antigravity.cooldowns.cancel')}</Button>
          <Button type="button" variant="destructive" disabled={clearing || loadStatus !== 'ready'} onClick={() => void clearConfirmed()}>
            {clearing ? t('antigravity.cooldowns.clearing') : t('antigravity.cooldowns.confirmClear')}
          </Button>
        </>}
      >
        <p className="break-words text-sm text-muted-foreground">
          {confirmation === 'all'
            ? t('antigravity.cooldowns.confirmAll')
            : t('antigravity.cooldowns.confirmModel', { model: confirmation?.model ?? '' })}
        </p>
      </Modal>
    </section>
  )
}
