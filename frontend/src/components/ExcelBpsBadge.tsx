import { useTranslation } from "react-i18next";
import { Layers, PauseCircle, Timer } from "lucide-react";
import type { AccountRow } from "../types";
import { formatBeijingTime } from "../utils/time";
import { excelBpsBadgeState } from "../lib/excelBpsStatus";

type ExcelBpsBadgeAccount = Pick<AccountRow, "openai_excel_bps" | "openai_excel_bps_effective" | "bps_pause">;

/** Whether the account list should render an Excel Basispoints badge. */
export function accountShowsExcelBpsBadge(account: ExcelBpsBadgeAccount): boolean {
  return excelBpsBadgeState(account) !== "none";
}

interface ExcelBpsBadgeProps {
  account: ExcelBpsBadgeAccount;
  /** Card view uses the compact flag style shared by the other account flags. */
  variant?: "row" | "card";
}

const ROW_STYLES = {
  active:
    "bg-emerald-50 text-emerald-700 ring-emerald-600/20 dark:bg-emerald-950 dark:text-emerald-400 dark:ring-emerald-400/20",
  paused:
    "bg-amber-50 text-amber-700 ring-amber-600/20 dark:bg-amber-950 dark:text-amber-400 dark:ring-amber-400/20",
  cooling:
    "bg-sky-50 text-sky-700 ring-sky-600/20 dark:bg-sky-950 dark:text-sky-400 dark:ring-sky-400/20",
} as const;

export default function ExcelBpsBadge({ account, variant = "row" }: ExcelBpsBadgeProps) {
  const { t } = useTranslation();
  const state = excelBpsBadgeState(account);
  if (state === "none") return null;
  const pause = account.bps_pause;
  let label = "BPS";
  let title = account.openai_excel_bps
    ? t("accounts.excelBpsBadgeForcedTitle")
    : t("accounts.excelBpsBadgeGlobalTitle");
  let tone: keyof typeof ROW_STYLES = "active";
  let Icon = Layers;
  if (state === "paused" || state === "models_paused") {
    tone = "paused";
    Icon = PauseCircle;
    label = state === "paused" ? t("accounts.excelBpsBadgePaused") : t("accounts.excelBpsBadgeModelsPaused");
    title = t(state === "paused" ? "accounts.excelBpsPausedTitle" : "accounts.excelBpsModelsPausedTitle", {
      models: (pause?.models ?? []).join(", "),
      next: formatBeijingTime(pause?.next_probe_at),
      failures: pause?.failures ?? 0,
    });
  } else if (state === "rate_limited") {
    tone = "cooling";
    Icon = Timer;
    label = t("accounts.excelBpsBadgeRateLimited");
    title = t("accounts.excelBpsRateLimitedTitle", { until: formatBeijingTime(pause?.rate_limited_until) });
  }
  if (variant === "card") {
    return (
      <span className="codex-account-card__flag" title={title}>
        <Icon className="size-3" />
        {label}
      </span>
    );
  }
  return (
    <span
      title={title}
      className={`inline-flex items-center gap-0.5 rounded-md px-1.5 py-0.5 text-[10px] font-medium ring-1 ring-inset ${ROW_STYLES[tone]}`}
    >
      <Icon className="size-2.5" />
      {label}
    </span>
  );
}
