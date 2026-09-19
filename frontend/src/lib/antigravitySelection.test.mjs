import assert from "node:assert/strict";
import test from "node:test";
import {
  antigravitySelectionSummary,
  isAntigravitySelectionAccount,
  reconcileAntigravitySelection,
  setAntigravitySelection,
} from "./antigravitySelection.ts";

const account = (id, overrides = {}) => ({
  id,
  name: `Account ${id}`,
  email: `account${id}@example.test`,
  account_type: "oauth",
  antigravity_api: true,
  antigravity_auth_kind: "oauth",
  ...overrides,
});

test("selection retains only minimal identity and eligibility snapshots", () => {
  const row = account(1, {
    access_token: "secret",
    refresh_token: "secret",
    custom_headers: { Authorization: "secret" },
    antigravity_quota: { models: {} },
    detail_loaded: true,
  });
  const selected = setAntigravitySelection(new Map(), [row], true);
  assert.deepEqual(selected.get(1), {
    id: 1,
    name: row.name,
    email: row.email,
    account_type: "oauth",
    antigravity_api: true,
    antigravity_auth_kind: "oauth",
    channel: "antigravity",
  });
  row.name = "changed after selection";
  assert.equal(selected.get(1).name, "Account 1");
});

test("select-all and deselect-all only affect the visible page", () => {
  const first = setAntigravitySelection(new Map(), [account(1), account(2)], true);
  const second = setAntigravitySelection(first, [account(3), account(4)], true);
  const deselected = setAntigravitySelection(second, [account(3), account(4)], false);
  assert.deepEqual([...deselected.keys()], [1, 2]);
  assert.deepEqual([...first.keys()], [1, 2]);
  assert.deepEqual(antigravitySelectionSummary(second, [account(3), account(4)]), {
    visibleSelectedCount: 2,
    hiddenSelectedCount: 2,
    allVisibleSelected: true,
    someVisibleSelected: false,
  });
});

test("partial and empty visible pages have correct indeterminate and hidden counts", () => {
  const selected = setAntigravitySelection(new Map(), [account(1), account(9)], true);
  assert.deepEqual(antigravitySelectionSummary(selected, [account(1), account(2), account(2)]), {
    visibleSelectedCount: 1,
    hiddenSelectedCount: 1,
    allVisibleSelected: false,
    someVisibleSelected: true,
  });
  assert.deepEqual(antigravitySelectionSummary(selected, []), {
    visibleSelectedCount: 0,
    hiddenSelectedCount: 2,
    allVisibleSelected: false,
    someVisibleSelected: false,
  });
});

test("reconciliation refreshes selected snapshots without collecting unselected or dropping hidden IDs", () => {
  const selected = setAntigravitySelection(new Map(), [account(1), account(99)], true);
  const refreshed = reconcileAntigravitySelection(selected, [account(1, { name: "Renamed", antigravity_auth_kind: "api_key" }), account(2)]);
  assert.equal(refreshed.get(1).name, "Renamed");
  assert.equal(refreshed.get(1).antigravity_auth_kind, "api_key");
  assert.equal(refreshed.size, 2);
  assert.equal(refreshed.get(99), selected.get(99));
  assert.equal(reconcileAntigravitySelection(refreshed, []), refreshed);
  assert.equal(reconcileAntigravitySelection(selected, [account(1), account(2)]), selected);
  let browsed = refreshed;
  for (let page = 0; page < 30; page += 1) {
    browsed = reconcileAntigravitySelection(browsed, [account(1000 + page)]);
  }
  assert.equal(browsed, refreshed);
});

test("batch busy freezes both toggles and snapshot reconciliation", () => {
  const selected = setAntigravitySelection(new Map(), [account(1)], true);
  assert.equal(setAntigravitySelection(selected, [account(2)], true, true), selected);
  assert.equal(setAntigravitySelection(selected, [account(1)], false, true), selected);
  assert.equal(reconcileAntigravitySelection(selected, [account(1, { name: "Renamed" })], true), selected);
});

test("selection rejects foreign, ambiguous and invalid identities", () => {
  const rejected = [
    account(2, { antigravity_api: undefined }),
    account(3, { antigravity_api: false }),
    account(4, { channel: "codex" }),
    account(5, { channel: "antigravity", antigravity_api: false }),
    account(0), account(-1), account(1.5), account(Number.NaN),
  ];
  for (const row of rejected) assert.equal(isAntigravitySelectionAccount(row), false);
  const selected = setAntigravitySelection(new Map(), [account(1), ...rejected], true);
  assert.deepEqual([...selected.keys()], [1]);
  assert.equal(isAntigravitySelectionAccount(account(6, { channel: "antigravity", antigravity_api: undefined })), true);
  const invalidated = reconcileAntigravitySelection(selected, [account(1, { channel: "claude" })]);
  assert.equal(invalidated.size, 0);
});
