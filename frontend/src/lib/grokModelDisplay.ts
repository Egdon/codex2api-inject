import type { AccountRow } from "../types";

type ModelAccount = Pick<AccountRow, "models" | "grok_models">;

export const DEFAULT_GROK_TEST_MODELS = [
  "grok-4.7",
  "grok-4.6",
  "grok-4.5",
  "grok-4",
  "grok-3-fast",
  "grok-3",
  "grok-2",
];

// Reading this must never mutate or populate the operator's fixed whitelist.
export function grokDisplayModels(account: ModelAccount): string[] {
  const configured = account.models ?? [];
  const catalog = account.grok_models;
  if (!catalog || catalog.status === "unknown") return [...configured];
  if (!configured.length) return [...catalog.models];
  const allowed = new Set(configured.map((m) => m.toLowerCase()));
  return catalog.models.filter((m) => allowed.has(m.toLowerCase()));
}

// A known but empty catalog means the account exposes no text model; only an
// unknown catalog may fall back to the static defaults.
export function grokConnectionTestModels(account: ModelAccount): string[] {
  const models = grokDisplayModels(account).filter(
    (m) => m.trim() !== "" && !m.toLowerCase().includes("image"),
  );
  if (models.length > 0) return models;
  return account.grok_models && account.grok_models.status !== "unknown"
    ? []
    : [...DEFAULT_GROK_TEST_MODELS];
}

export function grokModelSummaryTitle(account: ModelAccount): string {
  const source = account.models?.length ? "Whitelist" : "Automatic";
  const summary = account.grok_models;
  return [source, summary?.status ?? "unknown", summary?.updated_at ?? ""].filter(Boolean).join(" · ");
}
