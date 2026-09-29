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
