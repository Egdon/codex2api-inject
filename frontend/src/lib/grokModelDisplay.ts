import type { AccountRow } from "../types";

type ModelAccount = Pick<AccountRow, "models" | "grok_models">;

// Reading this must never mutate or populate the operator's fixed whitelist.
export function grokDisplayModels(account: ModelAccount): string[] {
  const configured = account.models ?? [];
  const catalog = account.grok_models;
  if (!catalog || catalog.status === "unknown") return [...configured];
  if (!configured.length) return [...catalog.models];
  const allowed = new Set(configured.map((m) => m.toLowerCase()));
  return catalog.models.filter((m) => allowed.has(m.toLowerCase()));
}

export function grokModelSummaryTitle(account: ModelAccount): string {
  const source = account.models?.length ? "Whitelist" : "Automatic";
  const summary = account.grok_models;
  return [source, summary?.status ?? "unknown", summary?.updated_at ?? ""].filter(Boolean).join(" · ");
}
