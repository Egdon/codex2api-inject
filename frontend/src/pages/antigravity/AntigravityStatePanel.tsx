import { useState } from "react";
import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";
import {
  CheckCircle2,
  ChevronRight,
  CircleGauge,
  ExternalLink,
  FileJson,
  FlaskConical,
  KeyRound,
  Loader2,
  Expand,
  RotateCw,
  ShieldCheck,
  ShieldQuestion,
  ShieldX,
  TriangleAlert,
} from "lucide-react";
import type {
  AccountRow,
  AntigravityAccountState,
  AntigravityPermissionsSnapshot,
} from "../../types";
import ChannelLogo from "../../components/ChannelLogo";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import {
  firstString,
  firstWarningURL,
  identityAvatarURL,
  identityProjectID,
  permissionAllowed,
  subscriptionTier,
  tierEntries,
  tierLabel,
  toRecord,
  type AntigravityTierEntry,
} from "../../lib/antigravityQuota";
import { formatRelativeTime } from "../../utils/time";
import { AntigravityModelQuotaList } from "./AntigravityQuotaCells";

export function AccountAvatar({ account, size = 36 }: { account: AccountRow; size?: number }) {
  const [failed, setFailed] = useState(false);
  const avatar = identityAvatarURL(account);
  if (avatar && !failed) {
    return (
      <img
        src={avatar}
        alt=""
        width={size}
        height={size}
        referrerPolicy="no-referrer"
        className="shrink-0 rounded-full border border-border bg-background object-cover"
        style={{ width: size, height: size }}
        onError={() => setFailed(true)}
      />
    );
  }
  return (
    <span
      className="inline-flex shrink-0 items-center justify-center rounded-full border border-border bg-background"
      style={{ width: size, height: size }}
    >
      <ChannelLogo channel="antigravity" size={Math.round(size * 0.58)} />
    </span>
  );
}

// 权限列里的操作入口:同步 warning 会带出 Google 的一次性操作链接
// (TOS 申诉表单 / 账号验证),直接渲染成可点击入口,免得去详情页复制长 URL。
export function SyncWarningAction({ warning }: { warning?: string }) {
  const { t } = useTranslation();
  const url = firstWarningURL(warning);
  if (!url) return null;
  const label = /appeal/i.test(warning ?? "")
    ? t("antigravity.submitAppeal")
    : t("antigravity.verifyAccount");
  return (
    <a
      href={url}
      target="_blank"
      rel="noopener noreferrer"
      onClick={(event) => event.stopPropagation()}
      className="mt-1 inline-flex max-w-[180px] items-center gap-1 text-[11px] font-medium text-primary hover:underline"
    >
      <ExternalLink className="size-3 shrink-0" aria-hidden />
      <span className="truncate">{label}</span>
    </a>
  );
}

// 详情 warning 横幅:URL 转成可点击链接,展示文本截短避免长链接刷屏。
export function LinkifiedWarning({ text }: { text: string }) {
  const parts = text.split(/(https?:\/\/[^\s;]+)/g);
  return (
    <span className="break-words">
      {parts.map((part, index) => {
        if (!/^https?:\/\//.test(part)) {
          return part;
        }
        const display = part.length > 64 ? part.slice(0, 61) + "…" : part;
        return (
          <a
            key={index}
            href={part}
            target="_blank"
            rel="noopener noreferrer"
            className="font-medium underline underline-offset-2"
          >
            {display}
          </a>
        );
      })}
    </span>
  );
}

export function PermissionBadge({ permissions }: { permissions?: AntigravityPermissionsSnapshot }) {
  const { t } = useTranslation();
  const allowed = permissionAllowed(permissions);
  if (allowed == null) {
    return (
      <Badge variant="outline" className="gap-1 text-muted-foreground">
        <ShieldQuestion className="size-3" />
        {t("antigravity.permissionUnknown")}
      </Badge>
    );
  }
  if (allowed) {
    return (
      <Badge className="gap-1 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300">
        <ShieldCheck className="size-3" />
        {t("antigravity.permissionAllowed")}
      </Badge>
    );
  }
  return (
    <Badge variant="destructive" className="gap-1">
      <ShieldX className="size-3" />
      {t("antigravity.permissionBlocked")}
    </Badge>
  );
}

// AntigravityAuthKindChip 凭据形态小徽章(与 Grok 页同款配色:OAuth 紫 / API Key 天蓝)。
export function AntigravityAuthKindChip({ account }: { account: AccountRow }) {
  const { t } = useTranslation();
  const isAPIKey = account.antigravity_auth_kind === "api_key";
  return (
    <span
      className={cn(
        "inline-flex shrink-0 items-center gap-0.5 whitespace-nowrap rounded-md px-1.5 py-0.5 text-[10px] font-medium ring-1 ring-inset",
        isAPIKey
          ? "bg-sky-500/10 text-sky-700 ring-sky-600/20 dark:bg-sky-500/20 dark:text-sky-300 dark:ring-sky-400/20"
          : "bg-violet-500/10 text-violet-700 ring-violet-600/20 dark:bg-violet-500/20 dark:text-violet-300 dark:ring-violet-400/20",
      )}
    >
      {isAPIKey ? <KeyRound className="size-2.5" /> : <FileJson className="size-2.5" />}
      {isAPIKey ? t("antigravity.authKindApiKey") : t("antigravity.authKindOAuth")}
    </span>
  );
}

export type PanelLayout = "compact" | "wide";

export function PanelSection({
  title,
  description,
  action,
  layout = "compact",
  children,
}: {
  title: string;
  description?: ReactNode;
  action?: ReactNode;
  layout?: PanelLayout;
  children: ReactNode;
}) {
  return (
    <section className={layout === "wide" ? "space-y-3" : "space-y-2.5"}>
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <h3
            className={
              layout === "wide"
                ? "text-sm font-semibold text-foreground"
                : "text-[11px] font-semibold uppercase tracking-wide text-muted-foreground"
            }
          >
            {title}
          </h3>
          {description ? (
            <div className={cn("mt-0.5 text-muted-foreground", layout === "wide" ? "text-xs" : "text-[11px] text-muted-foreground/80")}>
              {description}
            </div>
          ) : null}
        </div>
        {action ? <div className="flex shrink-0 items-center gap-1.5">{action}</div> : null}
      </div>
      {children}
    </section>
  );
}

type FactTone = "neutral" | "success" | "warning" | "muted";

const FACT_DOT: Record<FactTone, string> = {
  neutral: "",
  success: "bg-emerald-500",
  warning: "bg-amber-500",
  muted: "bg-muted-foreground/40",
};

/** 统一的事实磁贴:小标签 + 值(可带状态点、等宽字体)。宽版窗口里两个区块共用同一种磁贴。 */
function FactTile({
  label,
  value,
  title,
  tone = "neutral",
  mono = false,
}: {
  label: string;
  value: ReactNode;
  title?: string;
  tone?: FactTone;
  mono?: boolean;
}) {
  return (
    <div className="min-w-0 rounded-lg bg-muted/40 px-3 py-2">
      <div className="text-[11px] text-muted-foreground">{label}</div>
      <div
        className={cn(
          "mt-0.5 flex min-w-0 items-center gap-1.5 text-[13px] font-semibold text-foreground",
          mono && "font-mono text-[12px] font-medium",
        )}
        title={title}
      >
        {FACT_DOT[tone] ? <span className={cn("size-1.5 shrink-0 rounded-full", FACT_DOT[tone])} aria-hidden /> : null}
        <span className="truncate">{value}</span>
      </div>
    </div>
  );
}

export interface AntigravityStateProps {
  state: AntigravityAccountState | null;
  loading: boolean;
  error: string | null;
  action: "sync" | "probe" | null;
  onSync: () => void;
  onProbe: () => void;
}

export function AntigravitySyncWarning({ account }: { account: AccountRow }) {
  if (!account.antigravity_sync_warning) return null;
  return (
    <div className="flex items-start gap-2 rounded-xl border border-amber-300/70 bg-amber-50 px-3 py-2 text-xs text-amber-900 dark:border-amber-800 dark:bg-amber-950/40 dark:text-amber-200">
      <TriangleAlert className="mt-0.5 size-3.5 shrink-0" aria-hidden />
      <LinkifiedWarning text={account.antigravity_sync_warning} />
    </div>
  );
}

function useStateLabels() {
  const { t } = useTranslation();
  return {
    identity: (status: string): { label: string; tone: FactTone } => {
      switch (status) {
        case "verified":
          return { label: t("antigravity.identityVerified"), tone: "success" };
        case "pending":
          return { label: t("antigravity.identityPending"), tone: "warning" };
        case "not_applicable":
          return { label: t("antigravity.identityNotApplicable"), tone: "muted" };
        default:
          return { label: status || "-", tone: "neutral" };
      }
    },
    catalogSource: (source: string): string => {
      switch (source) {
        case "google_control_plane":
          return t("antigravity.catalogSourceGoogle");
        case "declared":
          return t("antigravity.catalogSourceDeclared");
        case "default":
          return t("antigravity.catalogSourceDefault");
        default:
          return source || "-";
      }
    },
  };
}

/** 状态与能力:持久化的身份 / 目录 / 能力观测,可手动同步或做一次能力探测。 */
export function AntigravityStateSection({
  state,
  loading,
  error,
  action,
  onSync,
  onProbe,
  layout = "compact",
}: AntigravityStateProps & { layout?: PanelLayout }) {
  const { t } = useTranslation();
  const labels = useStateLabels();
  const wide = layout === "wide";
  const latest = state?.capabilities?.[0];
  const lastSynced = state?.last_synced_at || state?.last_sync_attempt_at;
  const identity = state ? labels.identity(state.identity.status) : null;
  const catalogSource = state ? labels.catalogSource(state.catalog.source) : "";
  const lastSyncedText = lastSynced
    ? t("antigravity.lastSyncedAt", { time: formatRelativeTime(lastSynced, { variant: "compact" }) })
    : "";
  const buttons = (
    <>
      <Button
        variant={wide ? "outline" : "ghost"}
        size="xs"
        className={wide ? "h-7 px-2 text-[11px]" : "h-7 text-[11px]"}
        disabled={loading || action !== null}
        onClick={onSync}
      >
        <RotateCw className={cn("size-3", action === "sync" && "animate-spin")} />
        {action === "sync" ? t("antigravity.stateSyncing") : t("antigravity.stateSync")}
      </Button>
      <Button
        variant={wide ? "outline" : "ghost"}
        size="xs"
        className={wide ? "h-7 px-2 text-[11px]" : "h-7 text-[11px]"}
        disabled={loading || action !== null}
        onClick={onProbe}
      >
        <FlaskConical className={cn("size-3", action === "probe" && "animate-pulse")} />
        {action === "probe" ? t("antigravity.capabilityProbing") : t("antigravity.capabilityProbe")}
      </Button>
    </>
  );
  return (
    <PanelSection
      layout={layout}
      title={t("antigravity.stateTitle")}
      description={wide ? t("antigravity.stateDescription") : undefined}
      action={buttons}
    >
      <div className="space-y-3 rounded-xl border border-border bg-card p-3">
        {!wide ? (
          <p className="text-[11px] leading-relaxed text-muted-foreground">{t("antigravity.stateDescription")}</p>
        ) : null}
        {loading ? (
          <div className="flex items-center gap-2 text-xs text-muted-foreground">
            <Loader2 className="size-3.5 animate-spin" />
            {t("antigravity.stateLoading")}
          </div>
        ) : null}
        {error ? <div className="rounded-lg bg-destructive/10 px-3 py-2 text-xs text-destructive">{error}</div> : null}
        {state && identity ? (
          <>
            <div className={cn("grid gap-2", wide ? "grid-cols-2 sm:grid-cols-4" : "grid-cols-2")}>
              <FactTile
                label={t("antigravity.stateCredential")}
                value={state.credential_kind === "api_key" ? t("antigravity.authKindApiKey") : t("antigravity.authKindOAuth")}
              />
              <FactTile label={t("antigravity.stateIdentity")} value={identity.label} tone={identity.tone} title={state.identity.status} />
              <FactTile
                label={t("antigravity.stateCatalog")}
                value={t("antigravity.catalogSummary", { count: state.catalog.models.length, source: catalogSource })}
                title={`${state.catalog.models.length} · ${state.catalog.source}`}
              />
              <FactTile
                label={t("antigravity.stateVerification")}
                value={state.catalog.verified ? t("antigravity.verified") : t("antigravity.unverified")}
                tone={state.catalog.verified ? "success" : "warning"}
              />
            </div>
            <div className="flex items-center gap-3 rounded-lg border border-border/70 px-3 py-2">
              <FlaskConical className="size-4 shrink-0 text-muted-foreground" aria-hidden />
              <div className="min-w-0 flex-1">
                <div className="text-xs font-semibold text-foreground">{t("antigravity.capabilityTitle")}</div>
                <div className="truncate text-[11px] text-muted-foreground" title={latest ? `${latest.protocol} · ${latest.model_id} · ${latest.status}` : undefined}>
                  {latest
                    ? `${latest.protocol} · ${latest.model_id} · ${latest.status}${latest.http_status ? ` · HTTP ${latest.http_status}` : ""}`
                    : t("antigravity.capabilityEmpty")}
                </div>
              </div>
              <Badge variant={latest?.verified ? "default" : "outline"} className="shrink-0">
                {latest?.verified ? t("antigravity.verified") : t("antigravity.notProbed")}
              </Badge>
            </div>
            {lastSyncedText ? <div className="text-[10px] text-muted-foreground">{lastSyncedText}</div> : null}
            {state.warnings.length > 0 ? (
              <div className="space-y-1 rounded-lg border border-amber-500/30 bg-amber-500/5 px-3 py-2 text-[11px] text-amber-700 dark:text-amber-300">
                {state.warnings.map((warning) => (
                  <div key={warning}>{warning}</div>
                ))}
              </div>
            ) : null}
          </>
        ) : null}
      </div>
    </PanelSection>
  );
}

function TierChips({ entries, muted = false }: { entries: AntigravityTierEntry[]; muted?: boolean }) {
  const { t } = useTranslation();
  return (
    <div className="flex flex-wrap gap-1.5">
      {entries.map((entry) => (
        <span
          key={entry.key}
          className={cn(
            "inline-flex items-center gap-1 rounded-md border px-2 py-0.5 text-[11px] font-medium",
            muted
              ? "border-border bg-muted/30 text-muted-foreground"
              : "border-emerald-500/25 bg-emerald-500/5 text-foreground",
          )}
          title={entry.id || entry.name}
        >
          {!muted ? <CheckCircle2 className="size-3 shrink-0 text-emerald-500" aria-hidden /> : null}
          {entry.name}
          {entry.id ? <span className="font-mono text-[10px] font-normal text-muted-foreground">{entry.id}</span> : null}
          {entry.isDefault ? (
            <span className="rounded bg-primary/10 px-1 text-[10px] font-medium text-primary">{t("antigravity.tierDefault")}</span>
          ) : null}
        </span>
      ))}
    </div>
  );
}

/** Code Assist 权限:是否允许访问、订阅套餐、项目 ID、可用/不可用套餐。 */
export function AntigravityPermissionSection({
  account,
  layout = "compact",
}: {
  account: AccountRow;
  layout?: PanelLayout;
}) {
  const { t } = useTranslation();
  const wide = layout === "wide";
  const permissions = account.antigravity_permissions;
  const allowed = permissionAllowed(permissions);
  const permissionRecord = toRecord(permissions);
  const currentTier = tierLabel(permissionRecord.current_tier ?? permissionRecord.currentTier);
  const paidTier = tierLabel(permissionRecord.paid_tier ?? permissionRecord.paidTier);
  const allowedTiers = tierEntries(permissionRecord.allowed_tiers ?? permissionRecord.allowedTiers);
  const ineligibleTiers = tierEntries(permissionRecord.ineligible_tiers ?? permissionRecord.ineligibleTiers);
  const reason = firstString(permissionRecord, ["reason"]);
  const projectID = identityProjectID(account);
  return (
    <PanelSection
      layout={layout}
      title={t("antigravity.permissionsTitle")}
      description={wide ? t("antigravity.permissionsDescription") : undefined}
      action={<PermissionBadge permissions={permissions} />}
    >
      <div className="space-y-3 rounded-xl border border-border bg-card p-3">
        {allowed === false && reason ? (
          <div className="flex items-start gap-1.5 rounded-lg bg-red-500/10 px-2.5 py-2 text-[12px] text-red-700 dark:text-red-300">
            <ShieldX className="mt-0.5 size-3.5 shrink-0" aria-hidden />
            <span className="break-words">{reason}</span>
          </div>
        ) : null}
        <div className={cn("grid gap-2", wide ? "grid-cols-2 sm:grid-cols-4" : "grid-cols-2")}>
          <FactTile label={t("antigravity.subscriptionTier")} value={subscriptionTier(account) || currentTier || "-"} />
          <FactTile label={t("antigravity.currentTier")} value={currentTier || "-"} />
          <FactTile label={t("antigravity.paidTier")} value={paidTier || "-"} />
          <FactTile
            label={t("antigravity.projectId")}
            value={projectID || t("antigravity.projectPending")}
            title={projectID || undefined}
            tone={projectID ? "neutral" : "warning"}
            mono={Boolean(projectID)}
          />
        </div>
        {allowedTiers.length > 0 || ineligibleTiers.length > 0 ? (
          <div className="space-y-2 border-t border-border/60 pt-2.5">
            {allowedTiers.length > 0 ? (
              <div className="flex flex-col gap-1.5 sm:flex-row sm:items-start sm:gap-3">
                <div className="shrink-0 pt-0.5 text-[11px] text-muted-foreground sm:w-16">{t("antigravity.allowedTiers")}</div>
                <TierChips entries={allowedTiers} />
              </div>
            ) : null}
            {ineligibleTiers.length > 0 ? (
              <div className="flex flex-col gap-1.5 sm:flex-row sm:items-start sm:gap-3">
                <div className="shrink-0 pt-0.5 text-[11px] text-muted-foreground sm:w-16">{t("antigravity.ineligibleTiers")}</div>
                <TierChips entries={ineligibleTiers} muted />
              </div>
            ) : null}
          </div>
        ) : null}
      </div>
    </PanelSection>
  );
}

/**
 * 详情抽屉 providerSlot:同步告警、状态与能力、Code Assist 权限、逐模型额度。
 * 只读展示持久化观测,不主动打上游;完整的宽版视图走"额度与权限"窗口。
 */
export function AntigravityProviderPanel({
  account,
  onOpenFullDetail,
  ...stateProps
}: AntigravityStateProps & {
  account: AccountRow;
  onOpenFullDetail: () => void;
}) {
  const { t } = useTranslation();
  const isAPIKey = account.antigravity_auth_kind === "api_key";
  return (
    <div className="space-y-5">
      <AntigravitySyncWarning account={account} />
      {!isAPIKey ? (
        <button
          type="button"
          onClick={onOpenFullDetail}
          className="flex w-full items-center gap-3 rounded-xl border border-primary/25 bg-primary/5 px-3 py-2.5 text-left transition-colors hover:border-primary/40 hover:bg-primary/10"
        >
          <span className="flex size-8 shrink-0 items-center justify-center rounded-lg bg-primary/10 text-primary">
            <CircleGauge className="size-4" aria-hidden />
          </span>
          <span className="min-w-0 flex-1">
            <span className="block text-[13px] font-semibold text-foreground">{t("antigravity.quotaDetailTitle")}</span>
            <span className="block text-[11px] text-muted-foreground">{t("antigravity.quotaDetailEntryHint")}</span>
          </span>
          <ChevronRight className="size-4 shrink-0 text-muted-foreground" aria-hidden />
        </button>
      ) : null}
      <AntigravityStateSection {...stateProps} />
      {!isAPIKey ? <AntigravityPermissionSection account={account} /> : null}
      {!isAPIKey ? (
        <PanelSection
          title={t("antigravity.modelQuotaTitle")}
          action={
            <Button variant="ghost" size="xs" className="h-7 text-[11px]" onClick={onOpenFullDetail}>
              <Expand className="size-3" />
              {t("antigravity.quotaDetailOpen")}
            </Button>
          }
        >
          <AntigravityModelQuotaList account={account} />
        </PanelSection>
      ) : null}
    </div>
  );
}
