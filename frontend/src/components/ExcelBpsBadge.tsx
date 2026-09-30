import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { Layers, PauseCircle, Timer } from "lucide-react";
import type { AccountRow } from "../types";
import { formatBeijingTime } from "../utils/time";
import {
  excelBpsBadgeState,
  excelBpsStatusView,
  formatExcelBpsCountdown,
  summarizeExcelBpsModels,
} from "../lib/excelBpsStatus";
import { cn } from "../lib/utils";

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

const STATUS_TONES = {
  active:
    "bg-emerald-50 text-emerald-700 ring-emerald-600/20 dark:bg-emerald-950/60 dark:text-emerald-400 dark:ring-emerald-400/20",
  paused:
    "bg-amber-50 text-amber-800 ring-amber-500/25 dark:bg-amber-950/40 dark:text-amber-300 dark:ring-amber-400/20",
  cooling:
    "bg-sky-50 text-sky-700 ring-sky-500/25 dark:bg-sky-950/40 dark:text-sky-300 dark:ring-sky-400/20",
} as const;

// Re-render once a second while a probe or cooldown countdown is shown.
function useSecondTick(enabled: boolean): void {
  const [, setTick] = useState(0);
  useEffect(() => {
    if (!enabled) return undefined;
    const id = window.setInterval(() => setTick((tick) => tick + 1), 1000);
    return () => window.clearInterval(id);
  }, [enabled]);
}

interface ExcelBpsStatusProps {
  account: ExcelBpsBadgeAccount;
  /** "row" renders in the table status cell, "card" among the card flags. */
  variant?: "row" | "card";
}

/**
 * Basispoints route state for the status column. A healthy route is a small
 * inline "BPS" pill; a 403 pause or rate-limit cooldown takes its own line
 * (full width in the status cell) with a live countdown to the next probe or
 * to the end of the cooldown.
 */
export function ExcelBpsStatus({ account, variant = "row" }: ExcelBpsStatusProps) {
  const { t } = useTranslation();
  const view = excelBpsStatusView(account);
  useSecondTick(view.state !== "none" && view.state !== "active" && view.remainingMs !== null);
  if (view.state === "none") return null;
  const pause = account.bps_pause;

  if (view.state === "active") {
    const title = account.openai_excel_bps
      ? t("accounts.excelBpsBadgeForcedTitle")
      : t("accounts.excelBpsBadgeGlobalTitle");
    if (variant === "card") {
      return (
        <span className="codex-account-card__flag" title={title}>
          <Layers className="size-3" aria-hidden />
          BPS
        </span>
      );
    }
    return (
      <span
        title={title}
        className={cn(
          "inline-flex items-center gap-1 rounded-md px-1.5 py-0.5 text-[11px] font-medium ring-1 ring-inset",
          STATUS_TONES.active,
        )}
      >
        <Layers className="size-3" aria-hidden />
        BPS
      </span>
    );
  }

  const time = view.remainingMs === null ? "" : formatExcelBpsCountdown(view.remainingMs);
  let tone: keyof typeof STATUS_TONES = "paused";
  let Icon = PauseCircle;
  let label: string;
  let detail: string;
  let title: string;
  if (view.state === "rate_limited") {
    tone = "cooling";
    Icon = Timer;
    label = t("accounts.excelBpsBadgeRateLimited");
    detail = view.remainingMs ? t("accounts.excelBpsResumeIn", { time }) : "";
    title = t("accounts.excelBpsRateLimitedTitle", { until: formatBeijingTime(pause?.rate_limited_until) });
  } else {
    const probe =
      view.remainingMs === null
        ? ""
        : view.remainingMs > 0
          ? t("accounts.excelBpsProbeIn", { time })
          : t("accounts.excelBpsProbing");
    if (view.state === "paused") {
      label = t("accounts.excelBpsBadgePaused");
      detail = probe;
      title = t("accounts.excelBpsPausedTitle", {
        next: formatBeijingTime(pause?.next_probe_at),
        failures: pause?.failures ?? 0,
      });
    } else {
      label = t("accounts.excelBpsBadgeModelsPaused");
      detail = summarizeExcelBpsModels(view.models);
      title = t("accounts.excelBpsModelsPausedTitle", {
        models: view.models.join(", "),
        next: formatBeijingTime(pause?.next_probe_at),
      });
      if (probe) title = `${title}\n${probe}`;
    }
  }

  if (variant === "card") {
    // Same geometry as .codex-account-card__flag, which would otherwise
    // override the tone colors (unlayered CSS beats utility classes).
    return (
      <span
        className={cn(
          "inline-flex max-w-full items-center gap-1 rounded-md px-[0.4375rem] py-1 text-[11px] font-medium leading-[1.2] ring-1 ring-inset",
          STATUS_TONES[tone],
        )}
        title={title}
      >
        <Icon className="size-3 shrink-0" aria-hidden />
        <span className="shrink-0 font-semibold">{label}</span>
        {detail ? <span className="min-w-0 truncate tabular-nums">· {detail}</span> : null}
      </span>
    );
  }
  return (
    <div
      title={title}
      className={cn(
        "flex h-6 min-w-0 basis-full items-center gap-1.5 rounded-md px-2 text-[11px] leading-none ring-1 ring-inset",
        STATUS_TONES[tone],
      )}
    >
      <Icon className="size-3 shrink-0" aria-hidden />
      <span className="shrink-0 font-semibold">{label}</span>
      {detail ? (
        <>
          <span className="h-3 w-px shrink-0 bg-current/20" aria-hidden />
          <span className="min-w-0 truncate tabular-nums">{detail}</span>
        </>
      ) : null}
    </div>
  );
}
