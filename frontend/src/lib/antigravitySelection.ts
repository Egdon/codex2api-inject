import type { AccountRow } from "../types";

export type AntigravitySelectionAccount = Readonly<
  Pick<AccountRow, "id" | "name" | "email" | "account_type" | "antigravity_auth_kind" | "antigravity_api"> & {
    channel: "antigravity";
  }
>;

type SelectionCandidate = Pick<AccountRow,
  "id" | "name" | "email" | "account_type" | "antigravity_auth_kind" | "antigravity_api"
> & { channel?: string };

export type AntigravitySelection = ReadonlyMap<number, AntigravitySelectionAccount>;

export function isAntigravitySelectionAccount(account: SelectionCandidate): boolean {
  return Number.isSafeInteger(account.id) && account.id > 0 &&
    account.antigravity_api !== false &&
    (account.channel === undefined || account.channel === "antigravity") &&
    (account.antigravity_api === true || account.channel === "antigravity");
}

function snapshot(account: SelectionCandidate): AntigravitySelectionAccount {
  // Never retain credentials, hydrated details, or even quota objects in selection state.
  return {
    id: account.id,
    name: account.name,
    email: account.email,
    account_type: account.account_type,
    antigravity_auth_kind: account.antigravity_auth_kind,
    antigravity_api: account.antigravity_api,
    channel: "antigravity",
  };
}

function sameSnapshot(a: AntigravitySelectionAccount, b: AntigravitySelectionAccount): boolean {
  return a.id === b.id && a.name === b.name && a.email === b.email &&
    a.account_type === b.account_type && a.antigravity_auth_kind === b.antigravity_auth_kind &&
    a.antigravity_api === b.antigravity_api && a.channel === b.channel;
}

export function setAntigravitySelection(
  selection: AntigravitySelection,
  rows: readonly SelectionCandidate[],
  selected: boolean,
  busy = false,
): AntigravitySelection {
  if (busy) return selection;
  const next = new Map(selection);
  for (const row of rows) {
    if (!selected || !isAntigravitySelectionAccount(row)) {
      next.delete(row.id);
    } else {
      next.set(row.id, snapshot(row));
    }
  }
  return next;
}

export function reconcileAntigravitySelection(
  selection: AntigravitySelection,
  visibleRows: readonly SelectionCandidate[],
  busy = false,
): AntigravitySelection {
  if (busy || selection.size === 0) return selection;
  let next: Map<number, AntigravitySelectionAccount> | undefined;
  for (const row of visibleRows) {
    const current = selection.get(row.id);
    if (!current) continue;
    if (!isAntigravitySelectionAccount(row)) {
      next ??= new Map(selection);
      next.delete(row.id);
      continue;
    }
    const updated = snapshot(row);
    if (!sameSnapshot(current, updated)) {
      next ??= new Map(selection);
      next.set(row.id, updated);
    }
  }
  // The cache contains only selected IDs; a new page never appends unselected snapshots
  // or prunes off-page selections based on an incomplete/filtered list response.
  return next ?? selection;
}

export function antigravitySelectionSummary(
  selection: AntigravitySelection,
  visibleRows: readonly SelectionCandidate[],
) {
  const visibleIds = new Set(visibleRows.filter(isAntigravitySelectionAccount).map((row) => row.id));
  let visibleSelectedCount = 0;
  for (const id of visibleIds) {
    if (selection.has(id)) visibleSelectedCount += 1;
  }
  return {
    visibleSelectedCount,
    hiddenSelectedCount: selection.size - visibleSelectedCount,
    allVisibleSelected: visibleIds.size > 0 && visibleSelectedCount === visibleIds.size,
    someVisibleSelected: visibleSelectedCount > 0 && visibleSelectedCount < visibleIds.size,
  };
}
