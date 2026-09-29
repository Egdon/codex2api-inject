import { createContext, useContext } from 'react'
import { useTranslation } from 'react-i18next'
import { Badge } from './ui/badge'
import { Switch } from './ui/switch'
import { isBPSAccountEligible, type BPSAccountMetadata } from '../lib/bps'

export const BPSMasterContext = createContext<boolean | null>(null)

export function BPSPreferenceBadge({ account }: { account: BPSAccountMetadata }) {
  const { t } = useTranslation()
  const masterEnabled = useContext(BPSMasterContext)
  if (!account.openai_excel_bps) return null
  return <Badge variant="outline" className="border-border bg-muted/40 text-muted-foreground text-[10px]" title={t('bps.preferenceHint')}>
    {t(masterEnabled === false ? 'bps.masterOffBadge' : 'bps.preferenceBadge')}
  </Badge>
}

export function BPSAccountControl({ account, checked, onCheckedChange, disabled = false }: {
  account: BPSAccountMetadata
  checked: boolean
  onCheckedChange: (checked: boolean) => void
  disabled?: boolean
}) {
  const { t } = useTranslation()
  const eligible = isBPSAccountEligible(account)
  const masterEnabled = useContext(BPSMasterContext)
  return <div className="rounded-xl border border-border/70 bg-card p-4 space-y-2">
    <div className="flex items-center justify-between gap-3">
      <span className="text-sm font-semibold text-foreground">{t('bps.accountTitle')}</span>
      <Switch checked={checked} onCheckedChange={onCheckedChange}
        disabled={disabled || (!eligible && !checked)} aria-label={t('bps.accountTitle')} />
    </div>
    <p className="text-xs text-muted-foreground">{t('bps.accountHint')}</p>
    {!eligible && <p className="text-xs text-muted-foreground">{t('bps.unsupported')}</p>}
    {masterEnabled === false && <p className="text-xs text-muted-foreground">{t('bps.masterOffHint')}</p>}
  </div>
}
