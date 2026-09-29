import type { ExcelBpsPauseView } from "../types";

export type ExcelBpsBadgeState = "none" | "active" | "paused" | "models_paused" | "rate_limited";

interface ExcelBpsStatusSource {
  openai_excel_bps_effective?: boolean;
  bps_pause?: ExcelBpsPauseView | null;
}

function futureTime(value: string | undefined, now: number): number | null {
  if (!value) return null;
  const at = Date.parse(value);
  return Number.isFinite(at) && at > now ? at : null;
}

/**
 * Account-list badge state for Excel Basispoints. A 403 pause outranks a 429
 * cooldown because it lasts until a probe succeeds; an elapsed cooldown shows
 * as the ordinary active badge.
 */
export function excelBpsBadgeState(account: ExcelBpsStatusSource, now = Date.now()): ExcelBpsBadgeState {
  if (!account.openai_excel_bps_effective) return "none";
  const pause = account.bps_pause;
  if (pause?.scope === "account") return "paused";
  if (pause?.scope === "models" && (pause.models?.length ?? 0) > 0) return "models_paused";
  if (futureTime(pause?.rate_limited_until, now) != null) return "rate_limited";
  return "active";
}

export interface ExcelBpsStatusView {
  state: ExcelBpsBadgeState;
  /**
   * Milliseconds until the next recovery probe (403 pauses) or the end of a
   * rate-limit cooldown; 0 when it is due, null when unknown or not applicable.
   */
  remainingMs: number | null;
  /** Paused models for a model-scoped pause. */
  models: string[];
}

/** Status-column view of an account's Basispoints route. */
export function excelBpsStatusView(account: ExcelBpsStatusSource, now = Date.now()): ExcelBpsStatusView {
  const state = excelBpsBadgeState(account, now);
  const pause = account.bps_pause;
  const until =
    state === "paused" || state === "models_paused"
      ? pause?.next_probe_at
      : state === "rate_limited"
        ? pause?.rate_limited_until
        : undefined;
  const at = until ? Date.parse(until) : Number.NaN;
  return {
    state,
    remainingMs: Number.isFinite(at) ? Math.max(0, at - now) : null,
    models: state === "models_paused" ? (pause?.models ?? []) : [],
  };
}

/** Compact countdown: "42s", "3m 05s", "2h 07m". */
export function formatExcelBpsCountdown(ms: number): string {
  const total = Math.max(0, Math.ceil(ms / 1000));
  const hours = Math.floor(total / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  const seconds = total % 60;
  if (hours > 0) return `${hours}h ${String(minutes).padStart(2, "0")}m`;
  if (minutes > 0) return `${minutes}m ${String(seconds).padStart(2, "0")}s`;
  return `${seconds}s`;
}

/** "gpt-6-astra", or "gpt-6-astra +2" when several models are paused. */
export function summarizeExcelBpsModels(models: readonly string[]): string {
  if (models.length === 0) return "";
  return models.length === 1 ? models[0] : `${models[0]} +${models.length - 1}`;
}

/** Whether the account can be resumed manually from the UI. */
export function excelBpsCanResume(account: ExcelBpsStatusSource, now = Date.now()): boolean {
  const state = excelBpsBadgeState(account, now);
  return state === "paused" || state === "models_paused" || state === "rate_limited";
}
