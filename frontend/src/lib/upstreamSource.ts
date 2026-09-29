export type UpstreamSource = 'bps' | 'codex' | 'other' | '' | (string & {})
export type UpstreamSourceFilter = '' | 'bps' | 'codex' | 'other' | 'unknown'

// Only recorded backend evidence is authoritative; never derive this from account preferences.
export function normalizeUpstreamSource(source?: UpstreamSource | null): Exclude<UpstreamSourceFilter, ''> {
  switch (source) {
    case 'bps': return 'bps'
    case 'codex': return 'codex'
    case 'other': return 'other'
    default: return 'unknown'
  }
}
