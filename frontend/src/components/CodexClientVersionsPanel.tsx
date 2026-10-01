import { useTranslation } from 'react-i18next'
import type { CodexClientVersionTarget } from '../types'

function CodexVersionRow({ target }: { target: CodexClientVersionTarget }) {
  const { t } = useTranslation()
  const pair = target.pairs[0]
  const source = pair?.source ?? ''
  const label = target.client_kind === 'codex-desktop' ? 'Desktop' : 'VSCode'
  return (
    <tr className="border-t border-border/50 align-top">
      <td className="py-2 pr-3">{label}<br /><span className="font-mono">{target.target_platform}</span></td>
      <td className="py-2 pr-3 font-mono">
        {pair?.app_version ?? '—'}<br />CLI {pair?.cli_version ?? '—'}
        {target.pairs.length > 1 && (
          <details className="mt-1 font-sans">
            <summary className="cursor-pointer text-muted-foreground">{t('settings.codexClientVersions.history', { count: target.pairs.length - 1 })}</summary>
            {target.pairs.slice(1).map((old) => <div key={old.app_version} className="mt-1 font-mono">{old.app_version} / {old.cli_version}</div>)}
          </details>
        )}
      </td>
      <td className="py-2">{source ? t(`settings.codexClientVersions.sources.${source}`, { defaultValue: source }) : '—'}</td>
    </tr>
  )
}

export default function CodexClientVersionsPanel({ targets }: { targets: CodexClientVersionTarget[] }) {
  const { t } = useTranslation()
  if (!targets.length) return <p className="text-xs text-muted-foreground">{t('settings.codexClientVersions.empty')}</p>
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-left text-xs text-muted-foreground">
        <thead className="text-foreground/80">
          <tr>
            <th className="pb-2 pr-3 font-medium">{t('settings.codexClientVersions.platform')}</th>
            <th className="pb-2 pr-3 font-medium">{t('settings.codexClientVersions.pair')}</th>
            <th className="pb-2 font-medium">{t('settings.codexClientVersions.source')}</th>
          </tr>
        </thead>
        <tbody>{targets.map((target) => <CodexVersionRow key={`${target.client_kind}/${target.target_platform}`} target={target} />)}</tbody>
      </table>
    </div>
  )
}
