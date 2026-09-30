import type { BatchUpdateAccountsRequest, CodexFingerprintMode } from "../types";
import { excelBpsFlagsForMode } from './accountQuickConfig.ts';

export interface BuildBatchMetadataUpdateOptions {
  ids: number[];
  updateTags: boolean;
  tags: string[];
  updateGroups: boolean;
  groupIds: number[];
  updateScoreBias: boolean;
  scoreBias: number | null;
  updateBaseConcurrency: boolean;
  baseConcurrency: number | null;
  updateSchedulerPriority: boolean;
  schedulerPriority: number | null;
  updateCodexFingerprintMode?: boolean;
  codexFingerprintMode?: CodexFingerprintMode;
  bpsMode?: import('./bps').BPSBatchMode;
  updateTimezone?: boolean;
  timezone?: string;
}

export function buildBatchMetadataUpdate({
  ids,
  updateTags,
  tags,
  updateGroups,
  groupIds,
  updateScoreBias,
  scoreBias,
  updateBaseConcurrency,
  baseConcurrency,
  updateSchedulerPriority,
  schedulerPriority,
  updateCodexFingerprintMode,
  codexFingerprintMode,
  bpsMode,
  updateTimezone,
  timezone,
}: BuildBatchMetadataUpdateOptions): BatchUpdateAccountsRequest {
  const payload: BatchUpdateAccountsRequest = { ids: [...ids] };
  if (updateTags) payload.tags = [...tags];
  if (updateGroups) payload.group_ids = [...groupIds];
  if (updateScoreBias) payload.score_bias_override = scoreBias;
  if (updateBaseConcurrency)
    payload.base_concurrency_override = baseConcurrency;
  if (updateSchedulerPriority) payload.scheduler_priority = schedulerPriority;
  if (updateCodexFingerprintMode)
    payload.codex_fingerprint_mode = codexFingerprintMode ?? "off";
  if (bpsMode && bpsMode !== 'unchanged') Object.assign(payload, excelBpsFlagsForMode(bpsMode));
  if (updateTimezone) payload.timezone = (timezone ?? "").trim();
  return payload;
}
