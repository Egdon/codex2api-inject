import { useEffect, useRef, useState } from 'react'
import { RefreshCw } from 'lucide-react'
import { getAdminKey } from '../api'
import { useToast } from '../hooks/useToast'
import { readTurnStateRefresh } from '../lib/turnStateRefresh'
import { useTranslation } from 'react-i18next'
import type { AccountRow, CodexTurnStatePhase, CodexTurnStateStatus } from '../types'

export function isCodexTurnStateAccount(account: AccountRow) {
  return (
    !account.openai_responses_api &&
    !account.grok_api &&
    !account.claude_api &&
    !account.antigravity_api
  )
}

const colors: Record<CodexTurnStatePhase, string> = {
  ready: 'bg-amber-50 text-amber-700 ring-amber-600/20 dark:bg-amber-950 dark:text-amber-400',
  healthy:
    'bg-emerald-50 text-emerald-700 ring-emerald-600/20 dark:bg-emerald-950 dark:text-emerald-400 dark:ring-emerald-400/20',
  recovering:
    'bg-orange-50 text-orange-700 ring-orange-600/20 dark:bg-orange-950 dark:text-orange-400 dark:ring-orange-400/20',
  degraded:
    'bg-red-50 text-red-700 ring-red-600/20 dark:bg-red-950 dark:text-red-400 dark:ring-red-400/20',
  unknown: 'bg-zinc-100 text-zinc-500 ring-zinc-500/20 dark:bg-zinc-900 dark:text-zinc-400',
}

export default function CodexTurnStateBadge({ account }: { account: AccountRow }) {
  const { t } = useTranslation()
  const { showToast } = useToast()
  const [busy, setBusy] = useState(false)
  const [progress, setProgress] = useState('')
  const [override, setOverride] = useState<CodexTurnStateStatus>()
  const request = useRef<AbortController | null>(null)
  useEffect(() => () => request.current?.abort(), [])
  useEffect(() => {
    setOverride(undefined)
  }, [account.codex_turn_state_status])
  const refresh = async () => {
    if (request.current) return
    const controller = new AbortController()
    request.current = controller
    setBusy(true)
    const failures: string[] = []
    try {
      const key = getAdminKey()
      const response = await fetch(`/api/admin/accounts/${account.id}/turn-state/refresh`, {
        method: 'POST',
        headers: key ? { 'X-Admin-Key': key } : {},
        signal: controller.signal,
      })
      if (!response.ok) {
        const error = await response.json().catch(() => null)
        throw new Error(error?.error || `HTTP ${response.status}`)
      }
      if (!response.body) throw new Error(t('accounts.turnStateStatus.refreshFailed'))
      await readTurnStateRefresh(response.body, (event) => {
        if (event.type === 'testing') setProgress(event.model ?? '')
        if (event.type === 'result' && event.result && !event.result.saved) {
          failures.push(`${event.result.model}: ${event.result.error ?? ''}`)
        }
        if (event.type === 'done') {
          setOverride(event.status)
          const summary = t('accounts.turnStateStatus.refreshResult', {
            saved: event.saved ?? 0,
            total: event.total ?? 0,
          })
          showToast(
            [summary, ...failures].join('\n'),
            failures.length || !event.saved ? 'error' : 'success',
          )
        }
      })
    } catch (error) {
      if (!controller.signal.aborted)
        showToast(
          `${t('accounts.turnStateStatus.refreshFailed')}: ${error instanceof Error ? error.message : ''}`,
          'error',
        )
    } finally {
      request.current = null
      setBusy(false)
      setProgress('')
    }
  }
  if (!isCodexTurnStateAccount(account)) return null
  const status = override ?? account.codex_turn_state_status
  const state = status?.state ?? 'unknown'
  const label = (phase: CodexTurnStatePhase) => t(`accounts.turnStateStatus.${phase}`)
  const title = [
    ...(status?.injection_enabled === false
      ? [t('accounts.turnStateStatus.injectionDisabled')]
      : []),
    ...(status?.models.length
      ? status.models.map((model) =>
          [
            t('accounts.turnStateStatus.observation', {
              model: model.model,
              state: label(model.state),
              length: model.length || '—',
            }),
            model.template_expires_at
              ? t('accounts.turnStateStatus.expires', {
                  time: new Date(model.template_expires_at).toLocaleTimeString([], {
                    hour: '2-digit',
                    minute: '2-digit',
                    hourCycle: 'h23',
                  }),
                })
              : t(`accounts.turnStateStatus.${model.template_cached ? 'cached' : 'uncached'}`),
          ].join(' · '),
        )
      : [t('accounts.turnStateStatus.noObservations')]),
  ].join('\n')

  return (
    <span className="inline-flex items-center gap-1">
      <span
        className={`inline-flex shrink-0 items-center gap-1 rounded-md px-1.5 py-0.5 text-[10px] font-medium ring-1 ring-inset ${colors[state]}`}
        title={title}
        data-turn-state={state}
      >
        <span className="size-1.5 rounded-full bg-current" aria-hidden />
        {label(state)}
      </span>
      <button
        type="button"
        disabled={busy || status?.injection_enabled === false}
        aria-label={t('accounts.turnStateStatus.refresh')}
        title={
          status?.injection_enabled === false
            ? t('accounts.turnStateStatus.injectionDisabled')
            : busy
              ? t('accounts.turnStateStatus.refreshing', { model: progress })
              : t('accounts.turnStateStatus.refreshHint')
        }
        className="inline-flex size-5 items-center justify-center rounded text-zinc-500 hover:bg-amber-50 hover:text-amber-700 disabled:opacity-50 disabled:cursor-not-allowed dark:hover:bg-zinc-800"
        onClick={(event) => {
          event.stopPropagation()
          void refresh()
        }}
      >
        <RefreshCw className={`size-3 ${busy ? 'animate-spin' : ''}`} aria-hidden />
      </button>
    </span>
  )
}
