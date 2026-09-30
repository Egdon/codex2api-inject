import type { AccountRow, UpdateAccountSchedulerRequest } from '../types'
import { accountSupportsExcelBps, excelBpsFlagsForMode, excelBpsModeFromAccount, type ExcelBpsMode } from './accountQuickConfig.ts'

export type BPSAccountMetadata = Pick<AccountRow, 'id' | 'account_type' | 'openai_responses_api' | 'grok_api' | 'claude_api' | 'antigravity_api' | 'agent_identity' | 'openai_excel_bps' | 'openai_excel_bps_opt_out'>
export type BPSBatchMode = 'unchanged' | ExcelBpsMode

// Require positive OAuth identity. Missing details must never imply eligibility.
export function isBPSAccountEligible(account: BPSAccountMetadata | null | undefined): boolean {
  return account?.account_type === 'oauth' && accountSupportsExcelBps(account)
}

// Omit unchanged preferences, including legacy invalid flags, when editing other fields.
export function buildBPSAccountPatch(account: BPSAccountMetadata, mode: ExcelBpsMode): Pick<UpdateAccountSchedulerRequest, 'openai_excel_bps' | 'openai_excel_bps_opt_out'> {
  if (mode === excelBpsModeFromAccount(account)) return {}
  if (!isBPSAccountEligible(account) && !(mode === 'off' && (account.openai_excel_bps || account.openai_excel_bps_opt_out))) return {}
  return excelBpsFlagsForMode(mode)
}

export function selectBPSBatchAccounts(ids: number[], accounts: BPSAccountMetadata[], mode: BPSBatchMode) {
  const byID = new Map(accounts.map((account) => [account.id, account]))
  const eligibleIDs: number[] = []
  const unsupportedIDs: number[] = []
  const unknownIDs: number[] = []
  if (mode === 'unchanged') return { eligibleIDs, unsupportedIDs, unknownIDs }
  for (const id of new Set(ids)) {
    const account = byID.get(id)
    if (!account) {
      unknownIDs.push(id)
    } else if (mode === 'off' && (account.openai_excel_bps === true || account.openai_excel_bps_opt_out === true)) {
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
