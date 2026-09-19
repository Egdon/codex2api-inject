import type { AccountRow, AntigravityBatchRefreshResponse } from "../types";
import type { BatchOperationEvent } from "../hooks/useOperationProgress";

export const ANTIGRAVITY_BATCH_SIZE = 100;
export type AntigravityBatchAccount = Readonly<Pick<AccountRow,
  "id" | "name" | "email" | "account_type" | "antigravity_auth_kind" | "antigravity_api"
> & { channel?: "antigravity" }>;

/** Copy only selection metadata, never credentials or full account details. */
export function freezeAntigravityTargets(accounts: readonly AntigravityBatchAccount[]): AntigravityBatchAccount[] {
  const seen = new Set<number>();
  return accounts.filter((account) => {
    if (!Number.isSafeInteger(account.id) || account.id <= 0 || seen.has(account.id) ||
      account.antigravity_api === false || (account.channel !== undefined && account.channel !== "antigravity") ||
      !(account.antigravity_api || account.channel === "antigravity")) return false;
    seen.add(account.id);
    return true;
  }).map(({ id, name, email, account_type, antigravity_auth_kind }) => Object.freeze({
    id, name, email, account_type, antigravity_auth_kind, antigravity_api: true, channel: "antigravity" as const,
  }));
}

export function antigravityRefreshTargets(accounts: readonly AntigravityBatchAccount[]) {
  const targets = freezeAntigravityTargets(accounts);
  return {
    accounts: targets.filter((account) => account.antigravity_auth_kind === "oauth"),
    apiKeyCount: targets.filter((account) => account.antigravity_auth_kind === "api_key").length,
    unknownCount: targets.filter((account) => !["oauth", "api_key"].includes(account.antigravity_auth_kind ?? "")).length,
  };
}

export function chunkAntigravityIDs(ids: readonly number[], size = ANTIGRAVITY_BATCH_SIZE): number[][] {
  if (!Number.isInteger(size) || size < 1 || size > ANTIGRAVITY_BATCH_SIZE) throw new Error("Invalid batch size");
  const unique = [...new Set(ids.filter((id) => Number.isSafeInteger(id) && id > 0))];
  const chunks: number[][] = [];
  for (let start = 0; start < unique.length; start += size) {
    chunks.push(unique.slice(start, start + size));
  }
  return chunks;
}

export interface AntigravityBatchCounts {
  current: number;
  success: number;
  failed: number;
  banned: number;
  rate_limited: number;
  deleted: number;
}
export function emptyAntigravityCounts(): AntigravityBatchCounts {
  return { current: 0, success: 0, failed: 0, banned: 0, rate_limited: 0, deleted: 0 };
}

const countKeys = ["current", "success", "failed", "banned", "rate_limited", "deleted"] as const;
export function addAntigravityCounts(a: AntigravityBatchCounts, b: AntigravityBatchCounts): AntigravityBatchCounts {
  return Object.fromEntries(countKeys.map((key) => [key, a[key] + b[key]])) as unknown as AntigravityBatchCounts;
}

export function antigravityEventCounts(event: BatchOperationEvent, previous = emptyAntigravityCounts()): AntigravityBatchCounts {
  const next = { ...previous };
  for (const key of countKeys) {
    const value = event[key];
    if (Number.isSafeInteger(value) && value! >= 0) next[key] = Math.max(previous[key], value!);
  }
  return next;
}

/** A complete frame is necessary, but contradictory counts must not become success. */
export function isCompleteAntigravityStream(event: BatchOperationEvent | null, total: number): boolean {
  if (!event || event.type !== "complete" || event.total !== total || event.current !== total) return false;
  const counts = antigravityEventCounts(event);
  return counts.success + counts.failed + counts.banned + counts.rate_limited === total;
}

/** Count only observed, valid items; never invent an account failure for missing data. */
export function normalizeAntigravityRefresh(ids: readonly number[], response: AntigravityBatchRefreshResponse) {
  const allowed = new Set(ids);
  const seen = new Set<number>();
  const items: AntigravityBatchRefreshResponse["items"] = [];
  let malformed = !Array.isArray(response?.items);
  for (const item of Array.isArray(response?.items) ? response.items : []) {
    if (!item || !allowed.has(item.id) || seen.has(item.id) || typeof item.ok !== "boolean") {
      malformed = true;
      continue;
    }
    seen.add(item.id);
    items.push({
      id: item.id, ok: item.ok,
      email: typeof item.email === "string" ? item.email : undefined,
      message: typeof item.message === "string" ? item.message : undefined,
      warning: typeof item.warning === "string" ? item.warning : undefined,
      error: typeof item.error === "string" ? item.error : undefined,
    });
  }
  const success = items.filter((item) => item.ok).length;
  const failed = items.length - success;
  return { items, success, failed, complete: !malformed && items.length === allowed.size };
}

/** Counts-only metadata responses cannot identify which individual accounts failed. */
export function normalizeAntigravityMutation(total: number, response: { success: number; failed: number }) {
  const success = Number.isSafeInteger(response?.success) && response.success >= 0 ? response.success : 0;
  const failed = Number.isSafeInteger(response?.failed) && response.failed >= 0 ? response.failed : 0;
  const valid = success + failed === total && success <= total && failed <= total;
  return { success: success <= total ? success : 0, failed: failed <= total ? failed : 0, complete: valid };
}

/** The shared export API treats empty IDs as ALL: reject empty/oversized input first. */
export async function exportSelectedAntigravity<T>(
  ids: readonly number[],
  download: (ids: number[]) => Promise<T>,
): Promise<T> {
  const chunks = chunkAntigravityIDs(ids);
  if (chunks.length !== 1 || chunks[0].length !== ids.length) {
    throw new Error("Select 1–100 distinct Antigravity account IDs for export");
  }
  return download([...chunks[0]]);
}
