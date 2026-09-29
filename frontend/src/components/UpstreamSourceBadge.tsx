import { useTranslation } from 'react-i18next'
import { Badge } from './ui/badge'
import { normalizeUpstreamSource, type UpstreamSource } from '../lib/upstreamSource'

export default function UpstreamSourceBadge({ source }: { source?: UpstreamSource | null }) {
  const { t } = useTranslation()
  const value = normalizeUpstreamSource(source)
  return (
    <Badge variant="outline" title={t('upstreamSource.hint')}
      className={`text-[11px] ${value === 'bps' ? 'border-primary/30 bg-primary/10 text-primary' : 'border-border bg-muted/40 text-muted-foreground'}`}>
      {t(`upstreamSource.${value}`)}
    </Badge>
  )
}
