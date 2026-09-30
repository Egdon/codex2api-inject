/**
 * Models the Basispoints access catalog offered as of 2026-09-29. Only a
 * suggestion list for the settings picker: the backend accepts any model
 * name, and a model Basispoints refuses pauses itself and falls back to
 * native Codex.
 */
export const EXCEL_BPS_KNOWN_MODELS = [
  "gpt-6-astra",
  "gpt-5.6-sol",
  "gpt-5.6-terra",
  "gpt-5.6-luna",
  "gpt-6-sol",
  "gpt-6-luna",
] as const;

/**
 * Parses the stored global model list the same way the backend does: commas,
 * semicolons and whitespace separate names; names are trimmed, lowercased and
 * de-duplicated in their first order.
 */
export function parseExcelBpsModels(raw: string | null | undefined): string[] {
  const seen = new Set<string>();
  const models: string[] = [];
  for (const part of (raw ?? "").split(/[,;\s]+/)) {
    const model = part.trim().toLowerCase();
    if (!model || seen.has(model)) continue;
    seen.add(model);
    models.push(model);
  }
  return models;
}

/** Serializes picker chips to the stored comma-separated form. */
export function formatExcelBpsModels(models: readonly string[]): string {
  return parseExcelBpsModels(models.join(",")).join(",");
}

/**
 * Picker options: the known Basispoints models first, then every other model
 * the gateway lists, each once.
 */
export function excelBpsModelOptions(gatewayModels: readonly string[] = []): string[] {
  return parseExcelBpsModels([...EXCEL_BPS_KNOWN_MODELS, ...gatewayModels].join(","));
}
