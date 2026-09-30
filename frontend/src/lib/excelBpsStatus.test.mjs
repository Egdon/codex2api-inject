import assert from "node:assert/strict";
import test from "node:test";
import { excelBpsBadgeState, excelBpsCanResume } from "./excelBpsStatus.ts";

const now = Date.parse("2026-09-30T08:00:00Z");

test("excel bps badge distinguishes active, paused and cooling routes", () => {
  assert.equal(excelBpsBadgeState({}, now), "none");
  assert.equal(excelBpsBadgeState({ openai_excel_bps_effective: false, bps_pause: { scope: "account" } }, now), "none");
  assert.equal(excelBpsBadgeState({ openai_excel_bps_effective: true }, now), "active");
  assert.equal(excelBpsBadgeState({ openai_excel_bps_effective: true, bps_pause: { scope: "account", reason: "forbidden" } }, now), "paused");
  assert.equal(excelBpsBadgeState({ openai_excel_bps_effective: true, bps_pause: { scope: "models", models: ["gpt-5.5"] } }, now), "models_paused");
  assert.equal(
    excelBpsBadgeState({ openai_excel_bps_effective: true, bps_pause: { rate_limited_until: "2026-09-30T08:01:00Z" } }, now),
    "rate_limited",
  );
  assert.equal(
    excelBpsBadgeState({ openai_excel_bps_effective: true, bps_pause: { rate_limited_until: "2026-09-30T07:59:00Z" } }, now),
    "active",
  );
  // A 403 pause outranks a concurrent 429 cooldown.
  assert.equal(
    excelBpsBadgeState({ openai_excel_bps_effective: true, bps_pause: { scope: "account", rate_limited_until: "2026-09-30T08:01:00Z" } }, now),
    "paused",
  );
});

test("excel bps resume is offered only for paused or cooling routes", () => {
  assert.equal(excelBpsCanResume({ openai_excel_bps_effective: true }, now), false);
  assert.equal(excelBpsCanResume({ openai_excel_bps_effective: true, bps_pause: { scope: "account" } }, now), true);
  assert.equal(excelBpsCanResume({ openai_excel_bps_effective: true, bps_pause: { rate_limited_until: "2026-09-30T08:05:00Z" } }, now), true);
});

test("excel bps status view counts down to the next probe or the end of a cooldown", async () => {
  const { excelBpsStatusView } = await import("./excelBpsStatus.ts");
  const bps = { openai_excel_bps_effective: true };
  assert.deepEqual(excelBpsStatusView({}, now), { state: "none", remainingMs: null, models: [] });
  assert.deepEqual(excelBpsStatusView(bps, now), { state: "active", remainingMs: null, models: [] });
  assert.deepEqual(
    excelBpsStatusView({ ...bps, bps_pause: { scope: "account", next_probe_at: "2026-09-30T08:00:42Z" } }, now),
    { state: "paused", remainingMs: 42_000, models: [] },
  );
  // A probe that is due (or already running) shows zero, not a negative value.
  assert.equal(
    excelBpsStatusView({ ...bps, bps_pause: { scope: "account", next_probe_at: "2026-09-30T07:59:00Z" } }, now).remainingMs,
    0,
  );
  assert.equal(excelBpsStatusView({ ...bps, bps_pause: { scope: "account" } }, now).remainingMs, null);
  assert.deepEqual(
    excelBpsStatusView({ ...bps, bps_pause: { scope: "models", models: ["gpt-6-astra", "gpt-5.6-sol"], next_probe_at: "2026-09-30T08:01:00Z" } }, now),
    { state: "models_paused", remainingMs: 60_000, models: ["gpt-6-astra", "gpt-5.6-sol"] },
  );
  assert.deepEqual(
    excelBpsStatusView({ ...bps, bps_pause: { rate_limited_until: "2026-09-30T08:00:05Z" } }, now),
    { state: "rate_limited", remainingMs: 5_000, models: [] },
  );
});

test("excel bps countdown and model summaries stay compact", async () => {
  const { formatExcelBpsCountdown, summarizeExcelBpsModels } = await import("./excelBpsStatus.ts");
  assert.equal(formatExcelBpsCountdown(0), "0s");
  assert.equal(formatExcelBpsCountdown(4_200), "5s");
  assert.equal(formatExcelBpsCountdown(185_000), "3m 05s");
  assert.equal(formatExcelBpsCountdown(7_620_000), "2h 07m");
  assert.equal(summarizeExcelBpsModels([]), "");
  assert.equal(summarizeExcelBpsModels(["gpt-6-astra"]), "gpt-6-astra");
  assert.equal(summarizeExcelBpsModels(["gpt-6-astra", "gpt-5.6-sol", "gpt-5.5"]), "gpt-6-astra +2");
});
