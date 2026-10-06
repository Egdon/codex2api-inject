import assert from "node:assert/strict";
import test from "node:test";

import { buildWritableSettingsPayload } from "./settingsPayload.ts";

test("writable settings payload omits response cache generation regardless of value", () => {
  for (const generation of [7, 0, null, undefined]) {
    const settings = {
      site_name: "CodexProxy",
      response_cache_local_max_bytes: 64 * 1024 * 1024,
      response_cache_local_max_entry_bytes: 8 * 1024 * 1024,
      response_cache_reconstruct_max_bytes: 64 * 1024 * 1024,
      response_cache_config_generation: generation,
      future_setting: "preserved",
      codex_images_main_model: "gpt-5.6-sol",
      codex_images_default_main_model: "gpt-5.6-luna",
      codex_egress: { mode: "resin", resin_enabled: true },
      codex_client_versions: [{ cli_version: "0.158.0-alpha.2.1" }],
    };

    const payload = buildWritableSettingsPayload(settings);

    assert.equal(
      Object.hasOwn(payload, "response_cache_config_generation"),
      false,
    );
    assert.deepEqual(payload, {
      site_name: "CodexProxy",
      response_cache_local_max_bytes: 64 * 1024 * 1024,
      response_cache_local_max_entry_bytes: 8 * 1024 * 1024,
      response_cache_reconstruct_max_bytes: 64 * 1024 * 1024,
      future_setting: "preserved",
      codex_images_main_model: "gpt-5.6-sol",
    });
    assert.equal(
      Object.hasOwn(settings, "response_cache_config_generation"),
      true,
      "the source settings object must not be mutated",
    );
    assert.equal(settings.response_cache_config_generation, generation);
  }
});


test("merged settings hydrate mismatch on and maintenance identity off without overriding explicit values", async () => {
  const { readFileSync } = await import("node:fs");
  const source = readFileSync(new URL("../pages/Settings.tsx", import.meta.url), "utf8");
  const body = source.match(/const normalizeLazySettingsForm = useCallback\(\(settings: SystemSettings\): SystemSettings => \{([\s\S]*?)\n  \}, \[\]\)/);
  assert.ok(body, "exercise the actual settings hydration callback");
  const normalize = Function("settings", "normalizeResponseCacheSettings", "normalizeBillingTierPolicyValue", "MIB", "DEFAULT_MODELS_LIST_READ_MAX_BYTES", body[1]);
  for (const value of [undefined, null, false, true]) {
    const input = {
      show_upstream_model_mismatch: value,
      codex_unified_client_identity_enabled: value,
    };
    const result = normalize(input, (settings) => ({ ...settings }), (policy) => policy, 1048576, 4194304);
    assert.equal(result.show_upstream_model_mismatch, value ?? true);
    assert.equal(result.codex_unified_client_identity_enabled, value ?? false);
    const payload = buildWritableSettingsPayload(result);
    assert.equal(payload.show_upstream_model_mismatch, value ?? true);
    assert.equal(payload.codex_unified_client_identity_enabled, value ?? false);
    assert.equal(input.show_upstream_model_mismatch, value);
  }
});

test("v3.0.7 settings, concurrency and model source translations exist in all three locales", async () => {
  const { readFileSync } = await import("node:fs");
  const keys = [
    "grok.modelSourceList", "grok.modelSourceAuto",
    "accounts.schedulerKeepConcurrencyLabel", "accounts.schedulerKeepConcurrencyHint",
    "settings.showUpstreamModelMismatch", "settings.showUpstreamModelMismatchDesc",
    "settings.codexUnifiedClientIdentity", "settings.codexUnifiedClientIdentityDesc",
    "settings.codexUnifiedClientIdentityCompatHint",
  ];
  for (const language of ["zh", "en", "zh-TW"]) {
    const locale = JSON.parse(readFileSync(new URL(`../locales/${language}.json`, import.meta.url), "utf8"));
    for (const key of keys) {
      const value = key.split(".").reduce((entry, part) => entry?.[part], locale);
      assert.equal(typeof value, "string", `${language}: ${key}`);
      assert.ok(value.trim().length > 0, `${language}: ${key}`);
    }
  }
});
