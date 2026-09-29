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

/** Whether the account can be resumed manually from the UI. */
export function excelBpsCanResume(account: ExcelBpsStatusSource, now = Date.now()): boolean {
  const state = excelBpsBadgeState(account, now);
  return state === "paused" || state === "models_paused" || state === "rate_limited";
}
