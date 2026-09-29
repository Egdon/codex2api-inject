import assert from "node:assert/strict";
import test from "node:test";

import { buildUsageLogSearchParams } from "../api.ts";

test("usage log source filters serialize actual source categories without changing channel", () => {
  for (const upstream_source of ['', 'bps', 'codex', 'other', 'unknown']) {
    const params = buildUsageLogSearchParams({ start: 'start', end: 'end', channel: 'codex', upstream_source });
    assert.equal(params.get('upstream_source'), upstream_source || null);
    assert.equal(params.get('channel'), 'codex');
  }
});

test("usage log search params include diagnostic and traffic filters", () => {
  const params = buildUsageLogSearchParams({
    start: "2026-08-05T00:00:00Z",
    end: "2026-08-05T01:00:00Z",
    q: "rate limit",
    model: "gpt-5.6-sol",
    endpoint: "/v1/responses",
    apiKeyId: "12",
    accountId: "34",
    fast: "true",
    stream: "true",
    compact: "true",
    hasCompactionHistory: "true",
    channel: "codex",
    status: "5xx",
    errorOnly: "true",
    errorKind: "server",
    retry: "true",
    viaWebsocket: "false",
    includeCanceled: "true",
  });

  assert.deepEqual(Object.fromEntries(params), {
    start: "2026-08-05T00:00:00Z",
    end: "2026-08-05T01:00:00Z",
    q: "rate limit",
    model: "gpt-5.6-sol",
    endpoint: "/v1/responses",
    api_key_id: "12",
    account_id: "34",
    fast: "true",
    stream: "true",
    compact: "true",
    has_compaction_history: "true",
    channel: "codex",
    status: "5xx",
    error_only: "true",
    error_kind: "server",
    retry: "true",
    via_websocket: "false",
    include_canceled: "true",
  });
});

test("usage log search params omit empty optional filters", () => {
  const params = buildUsageLogSearchParams({
    start: "2026-08-05T00:00:00Z",
    end: "2026-08-05T01:00:00Z",
    q: "",
    status: "",
    retry: "",
  });

  assert.deepEqual(Object.fromEntries(params), {
    start: "2026-08-05T00:00:00Z",
    end: "2026-08-05T01:00:00Z",
  });
});
