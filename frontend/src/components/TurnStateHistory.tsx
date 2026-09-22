import { useEffect, useState } from 'react'
import { Filter, FilterX, History, RefreshCw } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { api } from '../api'
import Pagination from './Pagination'
import { Button } from './ui/button'
import { Select } from './ui/select'
import { formatBeijingTime } from '../utils/time'
import { getErrorMessage } from '../utils/error'
import { qualityTestPlanTone } from '../lib/qualityTest'
import type { TurnStateHistoryFilter, TurnStateHistoryPage } from '../lib/turnStateHistory'

const emptyPage: TurnStateHistoryPage = {
  records: [],
  total: 0,
  facets: { plans: [], models: [], accounts: [], proxies: [] },
}
const dateLabel = (value: number) =>
  value ? formatBeijingTime(new Date(value).toISOString()) : '—'
const statuses = ['running', 'success', 'failed', 'interrupted']

export default function TurnStateHistory() {
  const { t } = useTranslation()
  const [page, setPage] = useState(1)
  const [filter, setFilter] = useState<TurnStateHistoryFilter>({})
  const [revision, setRevision] = useState(0)
  const [data, setData] = useState<TurnStateHistoryPage>(emptyPage)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const { plan, account_id, model, status, proxy_url } = filter
  useEffect(() => {
    const controller = new AbortController()
    let timer: ReturnType<typeof setTimeout>
    setLoading(true)
    const poll = async () => {
      let delay = 5000
      try {
        const result = await api.getTurnStateHistory(
          page,
          { plan, account_id, model, status, proxy_url },
          controller.signal,
        )
        if (controller.signal.aborted) return
        setData(result)
        setError('')
        if (result.records.some((record) => record.status === 'running')) delay = 1500
      } catch (err) {
        if (!controller.signal.aborted) setError(getErrorMessage(err))
      } finally {
        if (!controller.signal.aborted) {
          setLoading(false)
          timer = setTimeout(poll, delay)
        }
      }
    }
    void poll()
    return () => {
      controller.abort()
      clearTimeout(timer)
    }
  }, [page, revision, plan, account_id, model, status, proxy_url])
  const update = (patch: TurnStateHistoryFilter) => {
    setFilter((value) => ({ ...value, ...patch }))
    setPage(1)
  }
  const facets = data.facets
  return (
    <section
      className="quality-test-records turn-state-history"
      aria-labelledby="turn-state-history-title"
    >
      <div className="quality-test-records-heading">
        <div>
          <h3 id="turn-state-history-title">{t('turnStateHistory.title')}</h3>
          <p>{t('turnStateHistory.hint')}</p>
        </div>
        <div className="quality-test-records-tools">
          <span className="quality-test-records-count">{data.total}</span>
          <Button variant="outline" size="sm" onClick={() => setRevision((value) => value + 1)}>
            <RefreshCw className={loading ? 'animate-spin' : ''} />
            {t('common.refresh')}
          </Button>
        </div>
      </div>
      {error ? (
        <div role="alert" className="quality-test-error">
          {error}
        </div>
      ) : null}
      <div className="quality-test-filters" role="group" aria-label={t('turnStateHistory.filters')}>
        <span className="quality-test-filters-label">
          <Filter className="size-3.5" />
          {t('qualityTest.filters.title')}
        </span>
        <Select
          aria-label={t('qualityTest.filters.plan')}
          compact
          value={plan ?? ''}
          onValueChange={(value) => update({ plan: value })}
          options={[
            { value: '', label: t('qualityTest.filters.allPlans') },
            ...facets.plans.map((value) => ({ value, label: value })),
          ]}
        />
        <Select
          aria-label={t('qualityTest.filters.account')}
          compact
          value={account_id ? String(account_id) : ''}
          onValueChange={(value) => update({ account_id: Number(value) || undefined })}
          options={[
            { value: '', label: t('qualityTest.filters.allAccounts') },
            ...facets.accounts.map((item) => ({
              value: String(item.id),
              label: `${item.name} · #${item.id}`,
            })),
          ]}
        />
        <Select
          aria-label={t('qualityTest.filters.model')}
          compact
          value={model ?? ''}
          onValueChange={(value) => update({ model: value })}
          options={[
            { value: '', label: t('qualityTest.filters.allModels') },
            ...facets.models.map((value) => ({ value, label: value })),
          ]}
        />
        <Select
          aria-label={t('turnStateHistory.status')}
          compact
          value={status ?? ''}
          onValueChange={(value) => update({ status: value })}
          options={[
            { value: '', label: t('turnStateHistory.allStatuses') },
            ...statuses.map((value) => ({ value, label: t(`turnStateHistory.statuses.${value}`) })),
          ]}
        />
        <Select
          aria-label={t('turnStateHistory.proxy')}
          compact
          value={proxy_url ?? ''}
          onValueChange={(value) => update({ proxy_url: value })}
          options={[
            { value: '', label: t('turnStateHistory.allProxies') },
            ...facets.proxies.map((item) => ({
              value: item.url,
              label: item.name ? `${item.name} · ${item.url}` : item.url,
            })),
          ]}
        />
        {Object.values(filter).some(Boolean) ? (
          <Button
            variant="ghost"
            size="sm"
            onClick={() => {
              setFilter({})
              setPage(1)
            }}
          >
            <FilterX className="size-3.5" />
            {t('turnStateHistory.clear')}
          </Button>
        ) : null}
      </div>
      <div className="quality-test-records-scroll" aria-busy={loading}>
        <table>
          <thead>
            <tr>
              {[
                'record',
                'account',
                'model',
                'attempt',
                'proxy',
                'started',
                'status',
                'duration',
                'expiry',
                'result',
              ].map((key) => (
                <th key={key}>{t(`turnStateHistory.${key}`)}</th>
              ))}
            </tr>
          </thead>
          <tbody>
            {data.records.map((record) => {
              const statusClass =
                record.status === 'success'
                  ? 'completed'
                  : record.status === 'failed'
                    ? 'error'
                    : record.status
              const duration =
                record.status === 'running'
                  ? Math.max(0, Date.now() - record.started_at)
                  : record.duration_ms
              return (
                <tr key={record.id}>
                  <td className="quality-test-record-id">#{record.id}</td>
                  <td>
                    <div className="quality-test-record-account">
                      <span title={record.account_name}>{record.account_name}</span>
                      <small>
                        #{record.account_id}
                        <span
                          className={`quality-test-plan quality-test-plan--${qualityTestPlanTone(record.plan_type)}`}
                        >
                          {record.plan_type || '—'}
                        </span>
                      </small>
                    </div>
                  </td>
                  <td>
                    <span className="quality-test-record-model">{record.model}</span>
                  </td>
                  <td>
                    <span className="quality-test-effort-chip">
                      {record.attempt} / {record.max_attempts}
                    </span>
                  </td>
                  <td>
                    <div className="turn-state-history-proxy">
                      <span>
                        {record.proxy_name ||
                          t(`turnStateHistory.routes.${record.route}`, { defaultValue: '—' })}
                        {record.proxy_id > 0 ? ` · #${record.proxy_id}` : ''}
                      </span>
                      <code>{record.proxy_url || '—'}</code>
                      {record.proxy_ip ? (
                        <small>
                          {t('turnStateHistory.lastIP')}: {record.proxy_ip}
                        </small>
                      ) : null}
                    </div>
                  </td>
                  <td className="quality-test-record-time">{dateLabel(record.started_at)}</td>
                  <td>
                    <span className={`quality-test-status quality-test-status-pill ${statusClass}`}>
                      <span className="quality-test-status-dot" />
                      {t(`turnStateHistory.statuses.${record.status}`)}
                    </span>
                  </td>
                  <td className="is-numeric">
                    {record.status === 'interrupted' && !duration
                      ? '—'
                      : `${(duration / 1000).toFixed(1)} s`}
                  </td>
                  <td className="quality-test-record-time">
                    <div className="turn-state-history-expiry">
                      <span>
                        {t('turnStateHistory.before')}: {dateLabel(record.expires_before)}
                      </span>
                      {record.expires_after > 0 ? (
                        <span>
                          {t('turnStateHistory.after')}: {dateLabel(record.expires_after)}
                        </span>
                      ) : null}
                    </div>
                  </td>
                  <td className="turn-state-history-reason">
                    {record.reason
                      ? t(`turnStateHistory.reasons.${record.reason}`, {
                          defaultValue: record.reason,
                        })
                      : t('turnStateHistory.inProgress')}
                  </td>
                </tr>
              )
            })}
          </tbody>
        </table>
        {data.records.length === 0 ? (
          <div className="quality-test-records-empty">
            <History className="size-8" />
            <h4>{t(loading ? 'turnStateHistory.loading' : 'turnStateHistory.empty')}</h4>
            <p>{t('turnStateHistory.emptyHint')}</p>
          </div>
        ) : null}
      </div>
      <Pagination
        page={page}
        totalPages={Math.ceil(data.total / 20)}
        onPageChange={setPage}
        totalItems={data.total}
        pageSize={20}
      />
    </section>
  )
}
