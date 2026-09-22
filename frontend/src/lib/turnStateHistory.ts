export interface TurnStateRenewalRecord {
  id: number
  account_id: number
  account_name: string
  plan_type: string
  model: string
  attempt: number
  max_attempts: number
  proxy_id: number
  proxy_name: string
  proxy_url: string
  proxy_ip: string
  route: string
  started_at: number
  finished_at: number
  duration_ms: number
  status: 'running' | 'success' | 'failed' | 'interrupted'
  reason: string
  expires_before: number
  expires_after: number
}

export interface TurnStateHistoryFilter {
  plan?: string
  account_id?: number
  model?: string
  status?: string
  proxy_url?: string
}

export interface TurnStateHistoryPage {
  records: TurnStateRenewalRecord[]
  total: number
  facets: {
    plans: string[]
    models: string[]
    accounts: { id: number; name: string }[]
    proxies: { url: string; name: string }[]
  }
}

export function turnStateHistoryQuery(page: number, filter: TurnStateHistoryFilter): string {
  const query = new URLSearchParams({ page: String(page), page_size: '20' })
  for (const [key, value] of Object.entries(filter)) {
    if (value !== undefined && value !== '') query.set(key, String(value))
  }
  return query.toString()
}
