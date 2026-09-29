import type { AccountRow, UpdateAccountSchedulerRequest } from '../types'

export type BPSAccountMetadata = Pick<AccountRow, 'id' | 'account_type' | 'openai_responses_api' | 'grok_api' | 'claude_api' | 'antigravity_api' | 'agent_identity' | 'openai_excel_bps'>
export type BPSBatchMode = 'unchanged' | 'on' | 'off'

// Require positive OAuth identity. Missing details must never imply eligibility.
export function isBPSAccountEligible(account: BPSAccountMetadata | null | undefined): boolean {
  return account?.account_type === 'oauth'
    && !account.openai_responses_api && !account.grok_api && !account.claude_api
    && !account.antigravity_api && !account.agent_identity
}

export function isBPSMasterEnabled(value: boolean | undefined): boolean {
  return value ?? true
}

// Omit unchanged preferences, including legacy invalid flags, when editing other fields.
export function buildBPSAccountPatch(account: BPSAccountMetadata, enabled: boolean): Pick<UpdateAccountSchedulerRequest, 'openai_excel_bps'> {
  if (enabled === (account.openai_excel_bps ?? false)) return {}
  if (enabled && !isBPSAccountEligible(account)) return {}
  return { openai_excel_bps: enabled }
}

export function selectBPSBatchAccounts(ids: number[], accounts: BPSAccountMetadata[], mode: BPSBatchMode) {
  const byID = new Map(accounts.map((account) => [account.id, account]))
  const eligibleIDs: number[] = []
  const unsupportedIDs: number[] = []
  const unknownIDs: number[] = []
  for (const id of new Set(ids)) {
    const account = byID.get(id)
    if (!account) {
      unknownIDs.push(id)
    } else if (mode === 'off' && account.openai_excel_bps === true) {
      eligibleIDs.push(id)
    } else if (!account.account_type) {
      unknownIDs.push(id)
    } else if (isBPSAccountEligible(account)) {
      eligibleIDs.push(id)
    } else {
      unsupportedIDs.push(id)
    }
  }
  return { eligibleIDs, unsupportedIDs, unknownIDs }
}
