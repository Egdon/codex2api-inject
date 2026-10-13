import { useState } from "react";
import { useTranslation } from "react-i18next";
import { ChevronDown, Clock, Coins, TriangleAlert } from "lucide-react";
import type { AccountRow, AntigravityQuotaSnapshot } from "../../types";
import ModelLogo from "../../components/ModelLogo";
import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";
import {
  aiCredits,
  formatPercent,
  isQuotaStale,
  lowestModelRemaining,
  modelQuotaEntries,
  quotaDisplayName,
  quotaRemaining,
  quotaResetTime,
  quotaTextTone,
  quotaTone,
  quotaUpdatedAt,
  sortedModelQuotaEntries,
  summarizeQuotaGroups,
  type QuotaWindowSummary,
} from "../../lib/antigravityQuota";
import { formatBeijingTime, formatRelativeTime } from "../../utils/time";

// formatUntil 把未来时间压成 "2d3h" / "4h12m" / "8m" 的倒计时;已过去或无法解析时为空。
function formatUntil(iso: string, now = Date.now()): string {
  const target = Date.parse(iso);
  if (!Number.isFinite(target)) return "";
  const remain = Math.floor((target - now) / 60000);
  if (remain <= 0) return "";
  const days = Math.floor(remain / 1440);
  const hours = Math.floor((remain % 1440) / 60);
  const minutes = remain % 60;
  if (days > 0) return `${days}d${hours}h`;
  if (hours > 0) return `${hours}h${minutes}m`;
  return `${minutes}m`;
}

function useWindowLabels() {
  const { t } = useTranslation();
  return {
    short: (window: QuotaWindowSummary) =>
      window.window === "5h"
        ? t("antigravity.windowShort5h")
        : window.window === "weekly"
          ? t("antigravity.windowShortWeekly")
          : window.rawWindow || "-",
    long: (window: QuotaWindowSummary) =>
      window.window === "5h"
        ? t("antigravity.window5h")
        : window.window === "weekly"
          ? t("antigravity.windowWeekly")
          : window.rawWindow || "-",
  };
}

function QuotaBar({ fraction, className }: { fraction: number | null; className?: string }) {
  return (
    <div className={cn("h-1.5 overflow-hidden rounded-full bg-muted", className)}>
      <div
        className={cn("h-full rounded-full transition-[width] duration-500", quotaTone(fraction))}
        style={{ width: `${fraction == null ? 0 : Math.max(2, fraction * 100)}%` }}
      />
    </div>
  );
}

function QuotaFreshness({ quota, className }: { quota?: AntigravityQuotaSnapshot; className?: string }) {
  const { t } = useTranslation();
  const updatedAt = quotaUpdatedAt(quota);
  if (!updatedAt) return null;
  const stale = isQuotaStale(updatedAt);
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1 text-[10px] tabular-nums",
        stale ? "text-amber-600 dark:text-amber-400" : "text-muted-foreground/80",
        className,
      )}
      title={`${t("antigravity.quotaSyncedAt")} ${formatBeijingTime(updatedAt)}`}
    >
      {stale ? <TriangleAlert className="size-2.5" aria-hidden /> : <Clock className="size-2.5" aria-hidden />}
      {t("antigravity.updatedAt", { time: formatRelativeTime(updatedAt, { variant: "compact" }) })}
      {stale ? ` · ${t("antigravity.quotaStale")}` : null}
    </span>
  );
}

/**
 * 列表额度单元:每个配额组一行,5h / 每周两条迷你进度条;没有配额组时回退成
 * "N 个模型 · 最低剩余"。整格可点开详情。
 */
export function AntigravityQuotaCell({
  account,
  onOpen,
}: {
  account: AccountRow;
  onOpen: () => void;
}) {
  const { t } = useTranslation();
  const labels = useWindowLabels();
  const quota = account.antigravity_quota;
  const groups = summarizeQuotaGroups(quota);
  const models = modelQuotaEntries(quota);

  if (groups.length === 0 && models.length === 0) {
    return (
      <button
        type="button"
        onClick={onOpen}
        className="text-left text-xs text-muted-foreground transition-colors hover:text-foreground"
      >
        {account.antigravity_auth_kind === "api_key"
          ? t("antigravity.quotaNotApplicable")
          : t("antigravity.quotaNotSynced")}
      </button>
    );
  }

  if (groups.length === 0) {
    const lowest = lowestModelRemaining(quota);
    return (
      <button
        type="button"
        onClick={onOpen}
        className="group block min-w-[180px] text-left"
        title={t("antigravity.viewDetails")}
      >
        <div className="flex items-center justify-between gap-2 text-xs">
          <span className="font-medium text-foreground">
            {t("antigravity.modelsCount", { count: models.length })}
          </span>
          <span className={cn("font-mono font-semibold tabular-nums", quotaTextTone(lowest))}>
            {formatPercent(lowest)}
          </span>
        </div>
        <QuotaBar fraction={lowest} className="mt-1.5" />
        <div className="mt-1 flex items-center justify-between gap-2">
          <span className="text-[10px] text-muted-foreground">{t("antigravity.lowestRemaining")}</span>
          <QuotaFreshness quota={quota} />
        </div>
      </button>
    );
  }

  return (
    <button
      type="button"
      onClick={onOpen}
      className="group block min-w-[248px] space-y-1 rounded-md text-left outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
      title={t("antigravity.viewDetails")}
    >
      {groups.map((group) => (
        <div key={group.key} className="flex items-center gap-2" title={group.description || group.label}>
          <span className="w-[68px] shrink-0 truncate text-[11px] font-medium text-foreground/80">
            {group.label}
          </span>
          {group.windows.slice(0, 2).map((window) => {
            const until = window.resetAt ? formatUntil(window.resetAt) : "";
            return (
              <span
                key={window.key}
                className="flex min-w-0 items-center gap-1"
                title={[
                  `${group.label} · ${labels.long(window)} ${formatPercent(window.fraction)}`,
                  window.resetAt ? `${t("antigravity.resetAt", { time: formatBeijingTime(window.resetAt) })}` : "",
                ]
                  .filter(Boolean)
                  .join("\n")}
              >
                <span className="w-5 shrink-0 text-[10px] text-muted-foreground">{labels.short(window)}</span>
                <QuotaBar fraction={window.fraction} className="w-11 shrink-0" />
                <span
                  className={cn(
                    "w-8 shrink-0 text-right text-[11px] font-semibold tabular-nums",
                    quotaTextTone(window.fraction),
                  )}
                >
                  {formatPercent(window.fraction)}
                </span>
                {until && window.fraction != null && window.fraction < 1 ? (
                  <span className="hidden shrink-0 text-[10px] tabular-nums text-muted-foreground/70 @6xl/antigravity-accounts:inline">
                    {until}
                  </span>
                ) : null}
              </span>
            );
          })}
        </div>
      ))}
      <QuotaFreshness quota={quota} />
    </button>
  );
}

/** 详情抽屉"用量"区:配额组大进度条 + 重置时间 + AI 积分 + 同步时间。 */
export function AntigravityQuotaOverview({ account }: { account: AccountRow }) {
  const { t } = useTranslation();
  const labels = useWindowLabels();
  const quota = account.antigravity_quota;
  const groups = summarizeQuotaGroups(quota);
  const credits = aiCredits(quota);
  const lowest = lowestModelRemaining(quota);

  if (account.antigravity_auth_kind === "api_key") {
    return <p className="text-xs text-muted-foreground">{t("antigravity.quotaNotApplicable")}</p>;
  }
  if (groups.length === 0 && lowest == null) {
    return <p className="text-xs text-muted-foreground">{t("antigravity.quotaNotSynced")}</p>;
  }

  return (
    <div className="space-y-3">
      {groups.length > 0 ? (
        groups.map((group) => (
          <div key={group.key} className="space-y-1.5">
            <div className="flex items-baseline justify-between gap-2">
              <span className="text-[12px] font-semibold text-foreground">{group.label}</span>
              {group.description ? (
                <span className="truncate text-[10px] text-muted-foreground" title={group.description}>
                  {group.description}
                </span>
              ) : null}
            </div>
            {group.windows.map((window) => {
              const until = window.resetAt ? formatUntil(window.resetAt) : "";
              return (
                <div key={window.key} className="flex items-center gap-2">
                  <span className="w-12 shrink-0 text-[11px] text-muted-foreground">{labels.long(window)}</span>
                  <QuotaBar fraction={window.fraction} className="min-w-0 flex-1" />
                  <span
                    className={cn(
                      "w-10 shrink-0 text-right text-[12px] font-semibold tabular-nums",
                      quotaTextTone(window.fraction),
                    )}
                  >
                    {formatPercent(window.fraction)}
                  </span>
                  <span
                    className="w-14 shrink-0 text-right text-[10px] tabular-nums text-muted-foreground"
                    title={window.resetAt ? t("antigravity.resetAt", { time: formatBeijingTime(window.resetAt) }) : undefined}
                  >
                    {until || "-"}
                  </span>
                </div>
              );
            })}
          </div>
        ))
      ) : (
        <div className="space-y-1.5">
          <div className="flex items-center justify-between text-[12px]">
            <span className="text-muted-foreground">{t("antigravity.lowestRemaining")}</span>
            <span className={cn("font-semibold tabular-nums", quotaTextTone(lowest))}>{formatPercent(lowest)}</span>
          </div>
          <QuotaBar fraction={lowest} />
        </div>
      )}
      <div className="flex flex-wrap items-center justify-between gap-2 border-t border-border/60 pt-2">
        {credits ? (
          <span className="inline-flex items-center gap-1 text-[11px] text-muted-foreground">
            <Coins className="size-3 text-amber-500" aria-hidden />
            {t("antigravity.aiCredits")}:
            <span className="font-semibold tabular-nums text-foreground">{credits.credits.toLocaleString()}</span>
            {credits.expiryDate ? (
              <span className="text-[10px]">· {t("antigravity.aiCreditsExpiry", { time: formatBeijingTime(credits.expiryDate) })}</span>
            ) : null}
          </span>
        ) : (
          <span />
        )}
        <QuotaFreshness quota={quota} />
      </div>
    </div>
  );
}

const MODEL_PREVIEW_LIMIT = 8;

/**
 * 逐模型额度明细。compact 用于详情抽屉(模型多时默认只展开前几项);
 * wide 用于"额度与权限"窗口:每行右侧是重置时间 + 百分比 + 进度条,全部展开。
 */
export function AntigravityModelQuotaList({
  account,
  variant = "compact",
}: {
  account: AccountRow;
  variant?: "compact" | "wide";
}) {
  const { t } = useTranslation();
  const [expanded, setExpanded] = useState(false);
  const models = sortedModelQuotaEntries(account.antigravity_quota);
  if (models.length === 0) {
    return (
      <div className="rounded-xl border border-dashed border-border p-4 text-center text-xs text-muted-foreground">
        {t("antigravity.quotaNotSynced")}
      </div>
    );
  }
  const wide = variant === "wide";
  const visible = wide || expanded ? models : models.slice(0, MODEL_PREVIEW_LIMIT);
  return (
    <div className="overflow-hidden rounded-xl border border-border bg-card">
      <div className="divide-y divide-border/70">
        {visible.map(([model, info]) => {
          const fraction = quotaRemaining(info);
          const resetAt = quotaResetTime(info);
          const capabilities = (
            <div className="mt-0.5 flex flex-wrap gap-x-2 text-[10px] text-muted-foreground">
              {info.supports_images ? <span>{t("antigravity.images")}</span> : null}
              {info.supports_thinking ? <span>{t("antigravity.thinking")}</span> : null}
              {info.max_output_tokens ? (
                <span>{t("antigravity.maxOutput", { count: info.max_output_tokens })}</span>
              ) : null}
              {!wide && resetAt ? (
                <span title={formatBeijingTime(resetAt)}>
                  {t("antigravity.resetAt", { time: formatBeijingTime(resetAt).slice(5, 16) })}
                </span>
              ) : null}
            </div>
          );
          const title = (
            <div className="flex min-w-0 items-center gap-1.5">
              <span
                className={cn("truncate font-medium text-foreground", wide ? "text-[13px]" : "text-[12px]")}
                title={model}
              >
                {quotaDisplayName(model, info)}
              </span>
              {wide && quotaDisplayName(model, info) !== model ? (
                <span className="hidden truncate font-mono text-[10px] text-muted-foreground sm:inline">{model}</span>
              ) : null}
              {info.recommended ? (
                <Badge variant="secondary" className="h-4 px-1 text-[9px]">
                  {t("antigravity.recommended")}
                </Badge>
              ) : null}
            </div>
          );
          if (wide) {
            return (
              <div
                key={model}
                className="grid gap-2 px-3.5 py-2.5 sm:grid-cols-[minmax(0,1fr)_220px] sm:items-center"
              >
                <div className="flex min-w-0 items-center gap-2.5">
                  <ModelLogo model={model} size={18} variant="plain" />
                  <div className="min-w-0 flex-1">
                    {title}
                    {capabilities}
                  </div>
                </div>
                <div>
                  <div className="flex items-center justify-between gap-2 text-[11px]">
                    <span className="truncate text-muted-foreground" title={resetAt ? formatBeijingTime(resetAt) : undefined}>
                      {resetAt ? t("antigravity.resetAt", { time: formatBeijingTime(resetAt) }) : "-"}
                    </span>
                    <span className={cn("shrink-0 font-mono font-semibold tabular-nums", quotaTextTone(fraction))}>
                      {formatPercent(fraction)}
                    </span>
                  </div>
                  <QuotaBar fraction={fraction} className="mt-1.5" />
                </div>
              </div>
            );
          }
          return (
            <div key={model} className="flex items-center gap-2.5 px-3 py-2">
              <ModelLogo model={model} size={16} variant="plain" />
              <div className="min-w-0 flex-1">
                {title}
                {capabilities}
              </div>
              <QuotaBar fraction={fraction} className="w-16 shrink-0" />
              <span className={cn("w-9 shrink-0 text-right text-[11px] font-semibold tabular-nums", quotaTextTone(fraction))}>
                {formatPercent(fraction)}
              </span>
            </div>
          );
        })}
      </div>
      {!wide && models.length > MODEL_PREVIEW_LIMIT ? (
        <button
          type="button"
          onClick={() => setExpanded((current) => !current)}
          className="flex w-full items-center justify-center gap-1 border-t border-border/70 bg-muted/20 py-1.5 text-[11px] font-medium text-muted-foreground transition-colors hover:bg-muted/40 hover:text-foreground"
        >
          <ChevronDown className={cn("size-3 transition-transform", expanded && "rotate-180")} aria-hidden />
          {expanded
            ? t("antigravity.collapseModels")
            : t("antigravity.showAllModels", { count: models.length })}
        </button>
      ) : null}
    </div>
  );
}
