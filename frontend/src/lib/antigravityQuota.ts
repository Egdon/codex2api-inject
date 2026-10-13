import type {
  AccountRow,
  AntigravityImportResponse,
  AntigravityModelQuota,
  AntigravityPermissionsSnapshot,
  AntigravityQuotaGroup,
  AntigravityQuotaSnapshot,
} from "../types";

// Antigravity 账号页的纯数据工具:额度快照解析、配额组摘要、身份字段兜底。
// 上游快照历经多版字段名(snake/camel、groups/quota_groups、models 数组/对象),
// 这里统一做容错读取,组件只消费归一化后的结构。

/** 同步时间超过该阈值的额度快照在界面上标注"可能已过期"。 */
export const ANTIGRAVITY_QUOTA_STALE_MS = 6 * 60 * 60 * 1000;

export function toRecord(value: unknown): Record<string, unknown> {
  return value && typeof value === "object" ? (value as Record<string, unknown>) : {};
}

export function firstString(record: Record<string, unknown>, keys: string[]): string {
  for (const key of keys) {
    const value = record[key];
    if (typeof value === "string" && value.trim()) return value.trim();
  }
  return "";
}

export function normalizeFraction(value: unknown): number | null {
  if (typeof value !== "number" || !Number.isFinite(value)) return null;
  const fraction = value > 1 && value <= 100 ? value / 100 : value;
  return Math.min(1, Math.max(0, fraction));
}

export function quotaRemaining(info: AntigravityModelQuota): number | null {
  const record = toRecord(info);
  return normalizeFraction(
    record.remaining_fraction ?? record.remainingFraction ?? record.percentage,
  );
}

export function quotaResetTime(info: AntigravityModelQuota): string {
  return firstString(toRecord(info), ["reset_time", "resetTime"]);
}

export function quotaDisplayName(model: string, info: AntigravityModelQuota): string {
  return firstString(toRecord(info), ["display_name", "displayName"]) || model;
}

export function modelQuotaEntries(
  quota?: AntigravityQuotaSnapshot | null,
): Array<[string, AntigravityModelQuota]> {
  if (!quota?.models) return [];
  if (Array.isArray(quota.models)) {
    return quota.models.map((info, index) => {
      const record = toRecord(info);
      const model =
        firstString(record, ["model", "model_id", "modelId", "name", "id"]) ||
        `model-${index + 1}`;
      return [model, info];
    });
  }
  return Object.entries(quota.models);
}

export function sortedModelQuotaEntries(
  quota?: AntigravityQuotaSnapshot | null,
): Array<[string, AntigravityModelQuota]> {
  return modelQuotaEntries(quota).sort(([leftName, left], [rightName, right]) => {
    if (Boolean(left.recommended) !== Boolean(right.recommended)) {
      return left.recommended ? -1 : 1;
    }
    return quotaDisplayName(leftName, left).localeCompare(
      quotaDisplayName(rightName, right),
    );
  });
}

/** 各模型剩余比例里最低的一个;没有任何可解析比例时为 null。 */
export function lowestModelRemaining(quota?: AntigravityQuotaSnapshot | null): number | null {
  const values = modelQuotaEntries(quota)
    .map(([, info]) => quotaRemaining(info))
    .filter((value): value is number => value != null);
  return values.length > 0 ? Math.min(...values) : null;
}

export function quotaUpdatedAt(quota?: AntigravityQuotaSnapshot | null): string {
  return firstString(toRecord(quota), ["updated_at", "updatedAt"]);
}

export function isQuotaStale(updatedAt: string, now = Date.now()): boolean {
  if (!updatedAt) return false;
  const parsed = Date.parse(updatedAt);
  if (!Number.isFinite(parsed)) return false;
  return now - parsed > ANTIGRAVITY_QUOTA_STALE_MS;
}

export function quotaGroups(quota?: AntigravityQuotaSnapshot | null): AntigravityQuotaGroup[] {
  const record = toRecord(quota);
  const groups = record.quota_groups ?? record.quotaGroups ?? record.groups;
  return Array.isArray(groups) ? (groups as AntigravityQuotaGroup[]) : [];
}

export type QuotaWindowKey = "5h" | "weekly" | "other";

export interface QuotaWindowSummary {
  key: string;
  window: QuotaWindowKey;
  /** 窗口原文(未识别的窗口直接展示上游给的名字)。 */
  rawWindow: string;
  fraction: number | null;
  resetAt: string;
}

export interface QuotaGroupSummary {
  key: string;
  label: string;
  description: string;
  windows: QuotaWindowSummary[];
  lowest: number | null;
}

export function quotaWindowKey(window: string): QuotaWindowKey {
  const value = window.trim().toLowerCase();
  if (value === "5h" || value === "five_hour" || value === "5_hour" || value === "fivehour") {
    return "5h";
  }
  if (value === "weekly" || value === "week" || value === "7d" || value === "seven_day") {
    return "weekly";
  }
  return "other";
}

const WINDOW_ORDER: Record<QuotaWindowKey, number> = { "5h": 0, weekly: 1, other: 2 };

/**
 * 配额组短名:上游给的是 "Gemini Models"、"Claude and GPT models" 这类长名,
 * 列表里只需要 "Gemini"、"Claude / GPT"。
 */
export function shortQuotaGroupLabel(displayName: string, index: number): string {
  const trimmed = displayName
    .trim()
    .replace(/\s+models?$/i, "")
    .replace(/\s+and\s+/gi, " / ")
    .trim();
  return trimmed || `#${index + 1}`;
}

export function summarizeQuotaGroups(quota?: AntigravityQuotaSnapshot | null): QuotaGroupSummary[] {
  return quotaGroups(quota).map((group, index) => {
    const record = toRecord(group);
    const displayName = firstString(record, ["display_name", "displayName"]);
    const buckets = Array.isArray(record.buckets) ? (record.buckets as unknown[]) : [];
    const windows = buckets
      .map((bucket, bucketIndex): QuotaWindowSummary => {
        const item = toRecord(bucket);
        const rawWindow = firstString(item, ["window"]);
        return {
          key: firstString(item, ["bucket_id", "bucketId"]) || `${index}-${bucketIndex}`,
          window: quotaWindowKey(rawWindow),
          rawWindow,
          fraction: normalizeFraction(item.remaining_fraction ?? item.remainingFraction),
          resetAt: firstString(item, ["reset_time", "resetTime"]),
        };
      })
      .sort((left, right) => WINDOW_ORDER[left.window] - WINDOW_ORDER[right.window]);
    const fractions = windows
      .map((window) => window.fraction)
      .filter((value): value is number => value != null);
    return {
      key: `${displayName || "group"}-${index}`,
      label: shortQuotaGroupLabel(displayName, index),
      description: firstString(record, ["description"]),
      windows,
      lowest: fractions.length > 0 ? Math.min(...fractions) : null,
    };
  });
}

export interface AntigravityAICredits {
  credits: number;
  expiryDate: string;
}

export function aiCredits(quota?: AntigravityQuotaSnapshot | null): AntigravityAICredits | null {
  const record = toRecord(toRecord(quota).ai_credits ?? toRecord(quota).aiCredits);
  const credits = record.credits;
  if (typeof credits !== "number" || !Number.isFinite(credits)) return null;
  return { credits, expiryDate: firstString(record, ["expiry_date", "expiryDate"]) };
}

export function subscriptionTier(account: AccountRow): string {
  const quota = toRecord(account.antigravity_quota);
  return (
    firstString(quota, ["subscription_tier", "subscriptionTier"]) ||
    account.plan_type ||
    ""
  );
}

export function permissionAllowed(
  permissions?: AntigravityPermissionsSnapshot | null,
): boolean | null {
  if (!permissions) return null;
  const record = toRecord(permissions);
  const value = record.allowed ?? record.allow_access ?? record.allowAccess;
  return typeof value === "boolean" ? value : null;
}

export function tierLabel(value: unknown): string {
  if (typeof value === "string") return value;
  if (typeof value === "number" || typeof value === "boolean") return String(value);
  const record = toRecord(value);
  const label = firstString(record, [
    "display_name",
    "displayName",
    "name",
    "label",
    "tier",
    "id",
    "key",
    "reason_code",
  ]);
  if (label) return label;
  if (!value) return "";
  try {
    return JSON.stringify(value);
  } catch {
    return String(value);
  }
}

export interface AntigravityTierEntry {
  key: string;
  name: string;
  /** 上游 tier id(free-tier / standard-tier …);与 name 不同时用来区分同名套餐。 */
  id: string;
  isDefault: boolean;
}

/**
 * 套餐列表:上游常给出同名不同 id 的套餐(两个都叫 "Antigravity"),只显示名字会像重复项。
 * 这里保留 id 与默认标记,并去掉 id+名字完全相同的重复条目。
 */
export function tierEntries(value: unknown): AntigravityTierEntry[] {
  if (!Array.isArray(value)) return [];
  const seen = new Set<string>();
  const entries: AntigravityTierEntry[] = [];
  value.forEach((item, index) => {
    const record = toRecord(item);
    const name = tierLabel(item);
    if (!name) return;
    const id = firstString(record, ["id", "tier", "key"]);
    const dedupeKey = `${id}\u0000${name}`;
    if (seen.has(dedupeKey)) return;
    seen.add(dedupeKey);
    entries.push({
      key: `${dedupeKey}-${index}`,
      name,
      id: id && id !== name ? id : "",
      isDefault: record.is_default === true || record.isDefault === true,
    });
  });
  return entries;
}

export function quotaTone(fraction: number | null): string {
  if (fraction == null) return "bg-muted-foreground/40";
  if (fraction <= 0.1) return "bg-red-500";
  if (fraction <= 0.35) return "bg-amber-500";
  return "bg-emerald-500";
}

export function quotaTextTone(fraction: number | null): string {
  if (fraction == null) return "text-muted-foreground";
  if (fraction <= 0.1) return "text-red-600 dark:text-red-400";
  if (fraction <= 0.35) return "text-amber-600 dark:text-amber-400";
  return "text-foreground";
}

export function formatPercent(fraction: number | null): string {
  if (fraction == null) return "-";
  return `${Math.round(fraction * 100)}%`;
}

export function accountLabel(account: AccountRow): string {
  return account.name || account.email || `#${account.id}`;
}

export function identityProjectID(account: AccountRow): string {
  return account.project_id || account.antigravity_project_id || "";
}

export function identityAvatarURL(account: AccountRow): string {
  return account.avatar_url || account.antigravity_avatar_url || "";
}

export function identityVerified(account: AccountRow): boolean {
  return account.verified_email ?? account.antigravity_verified_email ?? false;
}

export function isAntigravityAPIKeyAccount(account: AccountRow): boolean {
  return account.antigravity_auth_kind === "api_key";
}

/** 模型列表输入:按换行/逗号拆分、去空、去重,保持首次出现顺序。 */
export function parseModelList(value: string): string[] {
  return Array.from(
    new Set(
      value
        .split(/[\n,]/)
        .map((model) => model.trim())
        .filter(Boolean),
    ),
  );
}

/**
 * 粘贴的 refresh token 列表:每行一个,忽略空行与 # 注释行,去重。
 * 不在这里校验格式——后端解析器认得多种形态,前端只负责切分。
 */
export function parseRefreshTokenLines(value: string): string[] {
  return Array.from(
    new Set(
      value
        .split(/\r?\n/)
        .map((line) => line.trim())
        .filter((line) => line && !line.startsWith("#")),
    ),
  );
}

export type CredentialInputKind = "empty" | "json" | "json_invalid" | "refresh_token" | "unknown";

/**
 * 单个凭据输入框的实时识别:JSON(含格式错误)、Google refresh token(1// 开头)或其他。
 * 只做提示,不拦截提交——后端解析器认得的形态比这里多。
 */
export function detectCredentialInput(value: string): CredentialInputKind {
  const text = value.trim();
  if (!text) return "empty";
  if (text.startsWith("{") || text.startsWith("[")) {
    try {
      JSON.parse(text);
      return "json";
    } catch {
      return "json_invalid";
    }
  }
  if (/^1\/\/[\w.-]+$/.test(text)) return "refresh_token";
  return "unknown";
}

/** 后端单次导入上限(admin antigravityImportMaxAccounts)。 */
export const ANTIGRAVITY_IMPORT_BATCH_MAX = 500;
/** 后端单份内容上限是 4 MiB;超过这个体积的文件拆成逐账号条目再发。 */
export const ANTIGRAVITY_IMPORT_CONTENT_SPLIT_BYTES = 3 * 1024 * 1024;

function expandImportContent(content: string): string[] {
  const text = content.trim();
  if (!text.startsWith("[") && !text.startsWith("{")) return [content];
  try {
    const parsed: unknown = JSON.parse(text);
    const list = Array.isArray(parsed)
      ? parsed
      : Array.isArray(toRecord(parsed).accounts)
        ? (toRecord(parsed).accounts as unknown[])
        : null;
    if (!list) return [content];
    return list.map((item) => JSON.stringify(item));
  } catch {
    // 解析失败的内容原样交给后端,由后端给出逐文件的错误。
    return [content];
  }
}

/**
 * 把待导入内容切成若干批:总条目不超过单次上限且没有超大文件时原样一批发送(保留文件序号);
 * 否则把 JSON 数组 / {"accounts": [...]} 展开成逐账号条目,按上限分批。
 * 导出的多账号 JSON 数组超过 500 个账号时仍能一次选文件导入。
 */
export function planAntigravityImportBatches(
  contents: string[],
  maxPerBatch = ANTIGRAVITY_IMPORT_BATCH_MAX,
): string[][] {
  const units = contents.flatMap(expandImportContent);
  const oversized = contents.some((content) => content.length > ANTIGRAVITY_IMPORT_CONTENT_SPLIT_BYTES);
  if (units.length <= maxPerBatch && !oversized) return contents.length > 0 ? [contents] : [];
  const batches: string[][] = [];
  for (let start = 0; start < units.length; start += maxPerBatch) {
    batches.push(units.slice(start, start + maxPerBatch));
  }
  return batches;
}

/** 合并分批导入的结果;条目序号按批次偏移成全局位置,便于定位失败项。 */
export function mergeAntigravityImportResponses(
  responses: Array<{ response: AntigravityImportResponse; offset: number }>,
): AntigravityImportResponse {
  const merged: AntigravityImportResponse = { total: 0, imported: 0, synced: 0, degraded: 0, failed: 0, items: [] };
  const warnings = new Set<string>();
  const proxyWarnings = new Set<string>();
  for (const { response, offset } of responses) {
    merged.total += response.total ?? 0;
    merged.imported += response.imported ?? 0;
    merged.synced = (merged.synced ?? 0) + (response.synced ?? 0);
    merged.degraded = (merged.degraded ?? 0) + (response.degraded ?? 0);
    merged.failed += response.failed ?? 0;
    merged.items.push(...(response.items ?? []).map((item) => ({ ...item, index: item.index + offset })));
    if (response.group_ids) merged.group_ids = response.group_ids;
    if (response.warning?.trim()) warnings.add(response.warning.trim());
    if (response.proxies_imported !== undefined) {
      merged.proxies_imported = (merged.proxies_imported ?? 0) + response.proxies_imported;
      merged.proxies_skipped = (merged.proxies_skipped ?? 0) + (response.proxies_skipped ?? 0);
    }
    if (response.proxy_warning?.trim()) proxyWarnings.add(response.proxy_warning.trim());
  }
  if (warnings.size > 0) merged.warning = Array.from(warnings).join("；");
  if (proxyWarnings.size > 0) merged.proxy_warning = Array.from(proxyWarnings).join("；");
  return merged;
}

const warningURLPattern = /https?:\/\/[^\s;]+/;

export function firstWarningURL(text?: string): string | null {
  if (!text) return null;
  const match = text.match(warningURLPattern);
  return match ? match[0] : null;
}
