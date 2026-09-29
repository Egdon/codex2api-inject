import assert from "node:assert/strict";
import test from "node:test";
import {
  EXCEL_BPS_KNOWN_MODELS,
  excelBpsModelOptions,
  formatExcelBpsModels,
  parseExcelBpsModels,
} from "./excelBpsModels.ts";

test("excel bps model list parses like the backend", () => {
  assert.deepEqual(parseExcelBpsModels(""), []);
  assert.deepEqual(parseExcelBpsModels(undefined), []);
  assert.deepEqual(parseExcelBpsModels(" , ;"), []);
  assert.deepEqual(parseExcelBpsModels("GPT-6-Astra\ngpt-5.6-sol; gpt-5.6-sol"), ["gpt-6-astra", "gpt-5.6-sol"]);
});

test("excel bps picker chips serialize to the stored form", () => {
  assert.equal(formatExcelBpsModels([]), "");
  assert.equal(formatExcelBpsModels([" GPT-6-Astra ", "gpt-5.6-sol", "gpt-6-astra"]), "gpt-6-astra,gpt-5.6-sol");
});

test("excel bps picker lists known models first, then gateway models once", () => {
  const options = excelBpsModelOptions(["gpt-5.5", "GPT-6-ASTRA", "gpt-5.4"]);
  assert.deepEqual(options.slice(0, EXCEL_BPS_KNOWN_MODELS.length), [...EXCEL_BPS_KNOWN_MODELS]);
  assert.deepEqual(options.slice(EXCEL_BPS_KNOWN_MODELS.length), ["gpt-5.5", "gpt-5.4"]);
  assert.deepEqual(excelBpsModelOptions(), [...EXCEL_BPS_KNOWN_MODELS]);
});
