import { useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { api, AdminAPIError } from '../api'
import type { SystemBuildInfo, SystemUpdateInfo, UpdateSource } from '../types'
import { useVersionCheck } from '../hooks/useVersionCheck'
import { useToast } from '../hooks/useToast'
import { getErrorMessage } from '../utils/error'
import Modal from './Modal'

const exactVersion = (version: string) => version.replace(/^v/i, '')
const validBuild = (build: SystemBuildInfo) => Boolean(build && typeof build.version === 'string' && build.version && typeof build.source === 'string' && build.source && typeof build.local === 'boolean')
const eligible = (info: SystemUpdateInfo | null) => Boolean(info && info.supported &&
  (info.status === 'available' || info.status === 'migration') && info.target_tag && info.plan_token &&
  Date.parse(info.plan_expires_at) > Date.now())

export default function SystemUpdateModal({ show, onClose }: { show: boolean; onClose: () => void }) {
  const { t } = useTranslation()
  const { showToast } = useToast()
  const patched = useVersionCheck(undefined, 'patched', show)
  const official = useVersionCheck(undefined, 'official', show)
  const [selected, setSelected] = useState<UpdateSource>('patched')
  const [confirmation, setConfirmation] = useState<SystemUpdateInfo | null>(null)
  const [officialConfirmed, setOfficialConfirmed] = useState(false)
  const [migrationConfirmed, setMigrationConfirmed] = useState(false)
  const [busy, setBusy] = useState(false)
  const [waiting, setWaiting] = useState(false)
  const [build, setBuild] = useState<SystemBuildInfo | null>(null)
  const [now, setNow] = useState(Date.now())
  const generation = useRef(0)
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null)
  const selectedInfo = selected === 'patched' ? patched.updateInfo : official.updateInfo

  useEffect(() => {
    if (!show) return
    setSelected('patched')
    setConfirmation(null)
    setOfficialConfirmed(false)
    setMigrationConfirmed(false)
    void api.getSystemBuild().then(setBuild).catch(() => setBuild(null))
    const clock = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(clock)
  }, [show])
  useEffect(() => () => {
    generation.current += 1
    if (timer.current) clearTimeout(timer.current)
  }, [])

  const poll = (plan: SystemUpdateInfo, before: SystemBuildInfo) => {
    const id = ++generation.current
    const deadline = Date.now() + 120_000
    const target = exactVersion(plan.target_tag)
    const next = async () => {
      if (generation.current !== id) return
      let matched = false
      try {
        const current = await api.getSystemBuild()
        if (!validBuild(current)) throw new Error('Missing build identity')
        if (generation.current !== id) return
        setBuild(current)
        const changed = current.version !== before.version || current.source !== before.source ||
          current.revision !== before.revision || current.local !== before.local
        matched = changed && !current.local && current.source === plan.source && exactVersion(current.version) === target
      } catch (error) {
        // Only official replacement can remove this endpoint. Remote release health is irrelevant.
        const missingBuild = error instanceof AdminAPIError ? error.status === 404 || error.status === 405 :
          error instanceof SyntaxError || (error instanceof Error && error.message === 'Missing build identity')
        if (plan.source === 'official' && missingBuild) {
          try {
            const current = await api.getLegacySystemVersion()
            matched = Boolean(current.current_version) && exactVersion(current.current_version) === target &&
              (before.source !== 'official' || before.local || exactVersion(before.version) !== target)
          } catch { /* Service may still be restarting. */ }
        }
      }
      if (generation.current !== id) return
      if (matched) {
        showToast(t('common.updateApplied'), 'success')
        window.location.reload()
      } else if (Date.now() >= deadline) {
        setWaiting(false)
        showToast(t('common.restartTimeout'), 'error')
        void patched.refreshVersion(true)
        void official.refreshVersion(true)
      } else timer.current = setTimeout(next, 1500)
    }
    timer.current = setTimeout(next, 2500)
  }

  const install = async (plan: SystemUpdateInfo) => {
    if (busy || waiting || !eligible(plan)) return
    if ((plan.source === 'official' || plan.requires_official_confirmation) && !officialConfirmed) return
    if (plan.requires_migration_confirmation && !migrationConfirmed) return
    setBusy(true)
    let before: SystemBuildInfo | null = null
    let submitted = false
    try {
      // Capture actual local identity before mutation; never use the frontend bundle version.
      before = await api.getSystemBuild()
      if (!validBuild(before)) throw new Error(t('updates.identityUnavailable'))
      submitted = true
      await api.performSystemUpdate({ source: plan.source, target_tag: plan.target_tag, plan_token: plan.plan_token,
        confirm_official: officialConfirmed, confirm_migration: migrationConfirmed })
      setConfirmation(null)
      setWaiting(true)
      poll(plan, before)
    } catch (error) {
      setConfirmation(null)
      // A lost POST response is not proof of failure; verify without submitting again.
      if (submitted && before && (!(error instanceof AdminAPIError) || error.status >= 500)) {
        setWaiting(true)
        poll(plan, before)
      } else {
        showToast(getErrorMessage(error, t('common.updateFailed')), 'error')
        void patched.refreshVersion(true)
        void official.refreshVersion(true)
      }
    } finally { setBusy(false) }
  }

  return <>
    <Modal show={show} title={t('updates.title')} onClose={() => { if (!busy && !waiting) onClose() }} contentClassName="sm:max-w-[760px]">
      <p className="mb-3 text-sm text-muted-foreground">{t('updates.localBuild', { version: build?.version || '—', source: build?.source || '—' })}</p>
      <p className="mb-3 break-all text-sm text-muted-foreground">{t('updates.buildMetadata', { base: build?.upstream_base || '—', revision: build?.revision || '—' })}</p>
      <div className="grid gap-3 sm:grid-cols-2">
        {(['patched', 'official'] as const).map(source => {
          const check = source === 'patched' ? patched : official
          const info = check.updateInfo
          return <section key={source} className={`rounded-lg border p-4 ${selected === source ? 'border-primary bg-primary/5' : 'border-border'}`}>
            <label className="flex items-center gap-2 font-semibold">
              <input type="radio" name="update-source" checked={selected === source} disabled={busy || waiting} onChange={() => setSelected(source)} />
              {t(`updates.${source}`)}
            </label>
            <p className="mt-2 text-sm text-muted-foreground">{t(`updates.${source}Description`)}</p>
            <p className="mt-3 text-sm" role="status">{check.loading ? t('common.versionChecking') : t(`updates.status.${check.error ? 'unavailable' : info?.status || 'unavailable'}`)}</p>
            {info?.target_tag && <p className="mt-1 break-all font-mono text-sm">{info.target_tag}</p>}
            {info?.unsupported_reason && <p className="mt-2 text-sm text-amber-600">{info.unsupported_reason}</p>}
            {info?.warning && <p className="mt-2 text-sm text-amber-600">{info.warning}</p>}
            {info?.release_url && <a className="mt-2 block text-sm text-primary underline" href={info.release_url} target="_blank" rel="noopener noreferrer">{t('common.viewReleaseNotes')}</a>}
            <button type="button" className="mt-3 text-sm text-primary underline disabled:opacity-50" disabled={check.loading || busy || waiting} onClick={() => void check.refreshVersion(true)}>{t('updates.refresh')}</button>
          </section>
        })}
      </div>
      <p className="mt-4 text-sm text-muted-foreground">{t('updates.dockerWarning')}</p>
      {selectedInfo?.plan_token && Date.parse(selectedInfo.plan_expires_at) <= now && <p className="mt-2 text-sm text-amber-600">{t('updates.expired')}</p>}
      <button type="button" className="mt-4 rounded-md bg-primary px-4 py-2 text-sm font-semibold text-primary-foreground disabled:opacity-50"
        disabled={busy || waiting || !eligible(selectedInfo) || (selected === 'patched' ? patched.loading : official.loading)}
        onClick={() => {
          if (!selectedInfo) return
          setOfficialConfirmed(false)
          setMigrationConfirmed(false)
          setConfirmation(selectedInfo)
        }}>{waiting ? t('updates.verifying') : busy ? t('common.updating') : t('updates.review')}</button>
    </Modal>
    <Modal show={Boolean(confirmation)} title={confirmation?.source === 'official' ? t('updates.officialConfirmTitle') : t('updates.confirmTitle')} onClose={() => { if (!busy) setConfirmation(null) }}>
      <p className="break-all font-mono text-sm">{confirmation?.source} / {confirmation?.target_tag}</p>
      <p className="mt-2 text-sm text-muted-foreground">{t('updates.lockedPlan')}</p>
      <p className="mt-3 text-sm">{t('updates.backupWarning')}</p>
      {(confirmation?.source === 'official' || confirmation?.requires_official_confirmation) && <label className="mt-4 flex items-start gap-2 text-sm text-amber-700 dark:text-amber-300">
        <input type="checkbox" className="mt-1" checked={officialConfirmed} disabled={busy} onChange={event => setOfficialConfirmed(event.target.checked)} />{t('updates.officialWarning')}
      </label>}
      {confirmation?.requires_migration_confirmation && <label className="mt-4 flex items-start gap-2 text-sm">
        <input type="checkbox" className="mt-1" checked={migrationConfirmed} disabled={busy} onChange={event => setMigrationConfirmed(event.target.checked)} />{t('updates.migrationWarning')}
      </label>}
      <p className="mt-3 text-sm text-muted-foreground">{t('updates.dockerWarning')}</p>
      <button type="button" className="mt-4 rounded-md bg-primary px-4 py-2 text-sm font-semibold text-primary-foreground disabled:opacity-50"
        disabled={busy || !eligible(confirmation) || Boolean((confirmation?.source === 'official' || confirmation?.requires_official_confirmation) && !officialConfirmed) || Boolean(confirmation?.requires_migration_confirmation && !migrationConfirmed)}
        onClick={() => { if (confirmation) void install(confirmation) }}>{busy ? t('common.updating') : t('updates.confirmInstall')}</button>
    </Modal>
  </>
}
