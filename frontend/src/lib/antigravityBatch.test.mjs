import assert from "node:assert/strict";
import test from "node:test";
import { readFileSync } from "node:fs";
import {
  freezeAntigravityTargets, antigravityRefreshTargets, chunkAntigravityIDs,
  normalizeAntigravityRefresh, normalizeAntigravityMutation, exportSelectedAntigravity,
  emptyAntigravityCounts, addAntigravityCounts, antigravityEventCounts, isCompleteAntigravityStream,
} from "./antigravityBatch.ts";

const account = (id, kind = "oauth") => ({ id, name: `Account ${id}`, email: "", antigravity_api: true, antigravity_auth_kind: kind });

test("target snapshots are deduplicated, channel scoped, and never retain credentials", () => {
  const original = { ...account(1), access_token: "secret", credentials: { token: "secret" } };
  const targets = freezeAntigravityTargets([original, original, account(0), account(-2), { ...account(3), antigravity_api: false, channel: "antigravity" }, { id: 4 }, { ...account(5), channel: "grok" }]);
  original.name = "changed";
  assert.equal(targets.length, 1);
  assert.equal(targets[0].name, "Account 1");
  assert.equal(Object.isFrozen(targets[0]), true);
  assert.equal("credentials" in targets[0], false);
  assert.equal("access_token" in targets[0], false);
});

test("refresh only admits explicit OAuth and counts API keys and unknown kinds separately", () => {
  const result = antigravityRefreshTargets([account(1), account(2, "api_key"), account(3, "unknown")]);
  assert.deepEqual(result.accounts.map((row) => row.id), [1]);
  assert.equal(result.apiKeyCount, 1);
  assert.equal(result.unknownCount, 1);
});

test("chunk limits cover large selections without truncation", () => {
  const ids = Array.from({ length: 205 }, (_, index) => index + 1);
  assert.deepEqual(chunkAntigravityIDs(ids).map((chunk) => chunk.length), [100, 100, 5]);
  assert.deepEqual(chunkAntigravityIDs(ids, 10).flat(), ids);
  assert.throws(() => chunkAntigravityIDs(ids, 101));
});

test("refresh truth comes from real items, not inconsistent aggregate fields", () => {
  const result = normalizeAntigravityRefresh([1, 2], { success: 99, failed: 0, items: [{ id: 1, ok: true, warning: "warning" }, { id: 2, ok: false, error: "denied" }] });
  assert.equal(result.success, 1);
  assert.equal(result.failed, 1);
  assert.equal(result.complete, true);
  assert.equal(result.items[0].warning, "warning");
  const missing = normalizeAntigravityRefresh([1, 2], { items: [{ id: 1, ok: true }] });
  assert.equal(missing.complete, false);
  assert.equal(missing.failed, 0);
  assert.equal(missing.items.length, 1);
});

test("unexpected, duplicate and malformed refresh rows cannot invent account outcomes", () => {
  const result = normalizeAntigravityRefresh([1, 2], { items: [{ id: 1, ok: true }, { id: 1, ok: false }, { id: 2, ok: "yes" }, { id: 3, ok: false }] });
  assert.equal(result.complete, false);
  assert.equal(result.success, 1);
  assert.equal(result.failed, 0);
});

test("counts-only metadata partial failures are preserved without guessing IDs", () => {
  assert.deepEqual(normalizeAntigravityMutation(3, { success: 2, failed: 1 }), { success: 2, failed: 1, complete: true });
  assert.equal(normalizeAntigravityMutation(3, { success: 2, failed: 0 }).complete, false);
});

test("empty or oversized exports never reach the ALL fallback", async () => {
  let called = 0;
  const download = async (ids) => { called++; return ids; };
  await assert.rejects(exportSelectedAntigravity([], download));
  await assert.rejects(exportSelectedAntigravity(Array.from({ length: 101 }, (_, i) => i + 1), download));
  await assert.rejects(exportSelectedAntigravity([1, 1], download));
  assert.equal(called, 0);
  assert.deepEqual(await exportSelectedAntigravity([2, 5], download), [2, 5]);
});

test("missing SSE completion is indeterminate and chunk counts accumulate", () => {
  assert.equal(isCompleteAntigravityStream(null, 2), false);
  assert.equal(isCompleteAntigravityStream({ type: "complete", total: 2, current: 2, success: 1, failed: 0 }, 2), false);
  const complete = { type: "complete", total: 2, current: 2, success: 1, failed: 1 };
  assert.equal(isCompleteAntigravityStream(complete, 2), true);
  const first = antigravityEventCounts(complete);
  const combined = addAntigravityCounts(first, first);
  assert.equal(combined.current, 4);
  assert.equal(combined.failed, 2);
  assert.deepEqual(antigravityEventCounts({ current: -1, failed: NaN }), emptyAntigravityCounts());
});

test("component guards lifecycle, locks synchronously, and only emits outer completion", () => {
  const source = readFileSync(new URL("../components/AntigravityBatchActions.tsx", import.meta.url), "utf8");
  assert.match(source, /if \(lock\.current !== "confirm"\) return;\s*lock\.current = "running"/);
  assert.match(source, /acquire\("running"\)/);
  assert.match(source, /acquire\("confirm"\)/);
  assert.match(source, /return \(\) => \{ mounted\.current = false; \}/);
  assert.match(source, /if \(!mounted\.current\) return;\s*changed = true/);
  assert.match(source, /if \(mounted\.current && deleted\.size\) onDeleted/);
  assert.match(source, /if \(mounted\.current && changed\)/);
  assert.match(source, /if \(event\.type === "start"\) return/);
  assert.match(source, /type: "progress", total: ids\.length, \.\.\.counts/);
  assert.match(source, /if \(invalid \|\| !isCompleteAntigravityStream/);
  assert.match(source, /onClose=\{\(\) => setProgressHidden\(true\)\}/);
  assert.doesNotMatch(source, /AbortController|localStorage|sessionStorage|console\./);
});
