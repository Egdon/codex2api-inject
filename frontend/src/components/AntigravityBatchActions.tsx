import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { api, type ProxyRow } from "../api";
import type { AccountGroup, BatchUpdateAccountsRequest } from "../types";
import { postAdminSSE, readOperationSSE, useOperationProgress, type BatchOperationEvent } from "../hooks/useOperationProgress";
import { collectAccountOperationResult, snapshotAccountOperationResults, type AccountOperationResult, type AccountOperationResultsState } from "../lib/accountOperationResults";
import {
  ANTIGRAVITY_BATCH_SIZE, addAntigravityCounts, antigravityEventCounts,
  antigravityRefreshTargets, chunkAntigravityIDs, emptyAntigravityCounts,
  exportSelectedAntigravity, freezeAntigravityTargets, isCompleteAntigravityStream,
  normalizeAntigravityMutation, normalizeAntigravityRefresh,
  type AntigravityBatchAccount,
} from "../lib/antigravityBatch";
import AccountGroupMultiSelect from "./AccountGroupMultiSelect";
import Modal from "./Modal";
import OperationProgressToast from "./OperationProgressToast";
import OperationResultsModal from "./OperationResultsModal";
import { ProxyField } from "./ProxyField";
import { Button } from "./ui/button";

type Action = "test" | "refresh" | "enable" | "disable" | "groups" | "proxy" | "delete" | "export";
type Confirmation = { action: "groups" | "proxy" | "delete" | "export"; accounts: AntigravityBatchAccount[] };
export interface AntigravityBatchActionsProps {
  selectedAccounts: readonly AntigravityBatchAccount[];
  hiddenSelectedCount: number;
  proxies: ProxyRow[];
  groups: AccountGroup[];
  onClearSelection: () => void;
  onChanged: () => Promise<void>;
  onBusyChange?: (busy: boolean) => void;
  onDeleted?: (ids: number[]) => void;
}

export function AntigravityBatchActions({ selectedAccounts, hiddenSelectedCount, proxies, groups, onClearSelection, onChanged, onBusyChange, onDeleted }: AntigravityBatchActionsProps) {
  const { t } = useTranslation();
  const mounted = useRef(true);
  useEffect(() => {
    mounted.current = true;
    return () => { mounted.current = false; };
  }, []);
  // One lock covers both confirmation ownership and the request lifecycle.
  const lock = useRef<"idle" | "confirm" | "running">("idle");
  const [busy, setBusy] = useState(false);
  const [confirmation, setConfirmation] = useState<Confirmation | null>(null);
  const [groupIDs, setGroupIDs] = useState<number[]>([]);
  const [proxy, setProxy] = useState("");
  const [notice, setNotice] = useState<{ text: string; error: boolean } | null>(null);
  const [progressHidden, setProgressHidden] = useState(false);
  const [partialResults, setPartialResults] = useState<AccountOperationResultsState | null>(null);
  const [resultAccounts, setResultAccounts] = useState<AntigravityBatchAccount[]>([]);
  const { operationProgress, operationResults, reportOperationEvent, closeOperationResults } = useOperationProgress(true);
  const targets = freezeAntigravityTargets(selectedAccounts);
  const refresh = antigravityRefreshTargets(targets);
  const blocked = busy || confirmation !== null;
  const title = (action: Action) => t(`antigravity.batch.${action}`);
  const acquire = (phase: "confirm" | "running") => {
    if (lock.current !== "idle") return false;
    lock.current = phase;
    onBusyChange?.(true);
    return true;
  };
  const release = () => {
    lock.current = "idle";
    if (!mounted.current) return;
    setBusy(false);
    onBusyChange?.(false);
  };
  const closeConfirmation = () => {
    if (lock.current !== "confirm") return;
    setConfirmation(null);
    release();
  };
  const openConfirmation = (action: Confirmation["action"]) => {
    const accounts = freezeAntigravityTargets(selectedAccounts);
    if (!accounts.length || !acquire("confirm")) return;
    setGroupIDs([]);
    setProxy("");
    setConfirmation({ action, accounts });
  };

  const execute = async (action: Action, accounts: AntigravityBatchAccount[], confirmed = false) => {
    if (confirmed) {
      if (lock.current !== "confirm") return;
      lock.current = "running";
    } else if (!accounts.length || !acquire("running")) return;
    setBusy(true);
    setConfirmation(null);
    setNotice(null);
    setPartialResults(null);
    closeOperationResults();
    setResultAccounts(accounts);
    setProgressHidden(false);
    const eligible = action === "refresh" ? antigravityRefreshTargets(accounts).accounts : accounts;
    const ids = eligible.map((account) => account.id);
    const operationAction = action === "test" ? "batch_test" : action === "delete" ? "batch_delete" : "batch_refresh";
    const withProgress = ["test", "refresh", "delete"].includes(action);
    const results = new Map<number, AccountOperationResult>();
    const deleted = new Set<number>();
    let counts = emptyAntigravityCounts();
    let completedChunks = emptyAntigravityCounts();
    let interrupted = false;
    let changed = false;
    const emit = (event: BatchOperationEvent) => {
      if (!mounted.current) return;
      collectAccountOperationResult(results, event);
      reportOperationEvent(title(action), event);
    };
    try {
      if (!ids.length) throw new Error("No eligible targets");
      if (withProgress) emit({ type: "start", action: operationAction, total: ids.length, ...counts });
      if (action === "export") {
        const file = await exportSelectedAntigravity(ids, (selected) => api.exportAntigravityAccounts(selected));
        if (!mounted.current) return;
        const url = URL.createObjectURL(file.blob);
        const anchor = document.createElement("a");
        try {
          anchor.href = url;
          anchor.download = file.filename || "antigravity-accounts.zip";
          document.body.appendChild(anchor);
          anchor.click();
        } finally {
          anchor.remove();
          window.setTimeout(() => URL.revokeObjectURL(url), 1000);
        }
        setNotice({ text: t("antigravity.batch.exportDone", { count: file.count ?? ids.length }), error: false });
        return;
      }
      for (const chunk of chunkAntigravityIDs(ids, action === "refresh" ? 10 : ANTIGRAVITY_BATCH_SIZE)) {
        // Navigation fences new requests; the already-posted chunk is not cancelled or retried.
        if (!mounted.current) return;
        changed = true; // Even a lost response can have changed server state.
        if (action === "test" || action === "delete") {
          const allowed = new Set(chunk);
          let chunkCounts = emptyAntigravityCounts();
          let finalEvent: BatchOperationEvent | null = null;
          let invalid = false;
          const response = await postAdminSSE(`/accounts/batch-${action}?stream=true`, { ids: chunk });
          await readOperationSSE(response, (event) => {
            // readOperationSSE catches callback exceptions; validation uses a flag instead.
            if (event.action !== operationAction || !["start", "progress", "complete"].includes(event.type) ||
              (event.account_id !== undefined && !allowed.has(event.account_id))) {
              invalid = true;
              return;
            }
            if (event.type === "start") return; // Never reset results between chunks.
            if (finalEvent) { invalid = true; return; }
            const previousSuccess = chunkCounts.success;
            chunkCounts = antigravityEventCounts(event, chunkCounts);
            if (chunkCounts.current > chunk.length || chunkCounts.success + chunkCounts.failed + chunkCounts.banned + chunkCounts.rate_limited > chunk.length) {
              invalid = true;
              return;
            }
            counts = addAntigravityCounts(completedChunks, chunkCounts);
            if (event.type === "complete") finalEvent = event;
            if (action === "delete" && event.type === "progress" && event.account_id && !event.error && chunkCounts.success === previousSuccess + 1) {
              deleted.add(event.account_id);
            }
            emit({ ...event, type: "progress", total: ids.length, ...counts });
          });
          if (invalid || !isCompleteAntigravityStream(finalEvent, chunk.length)) throw new Error("Incomplete stream");
        } else if (action === "refresh") {
          const response = await api.batchRefreshAntigravityAccounts(chunk);
          const normalized = normalizeAntigravityRefresh(chunk, response);
          counts = addAntigravityCounts(completedChunks, {
            ...emptyAntigravityCounts(), current: normalized.items.length, success: normalized.success, failed: normalized.failed,
          });
          // Every item is already confirmed by the received chunk, not simulated in-flight progress.
          for (const item of normalized.items) {
            const message = [item.message, item.warning, item.error].filter(Boolean).join("; ");
            emit({ type: "progress", action: operationAction, total: ids.length, ...counts,
              account_id: item.id, account_email: item.email, status: item.ok ? "success" : "failed", message });
          }
          if (!normalized.complete) throw new Error("Incomplete refresh response");
        } else {
          const patch: BatchUpdateAccountsRequest = action === "enable" || action === "disable"
            ? { enabled: action === "enable" }
            : action === "groups" ? { group_ids: [...groupIDs] } : { proxy_url: proxy.trim() };
          const response = await api.batchUpdateAccounts({ ...patch, ids: chunk });
          const normalized = normalizeAntigravityMutation(chunk.length, response);
          counts = addAntigravityCounts(completedChunks, {
            ...emptyAntigravityCounts(), current: normalized.success + normalized.failed,
            success: normalized.success, failed: normalized.failed,
          });
          if (!normalized.complete) throw new Error("Incomplete metadata response");
          if (!mounted.current) return;
          setNotice({ text: t("antigravity.batch.runningCounts", { ...counts, total: ids.length }), error: false });
        }
        completedChunks = counts;
      }
      if (!mounted.current) return;
      if (withProgress) emit({ type: "complete", action: operationAction, total: ids.length, ...counts, message: "" });
      setNotice({
        text: t(action === "test" ? "antigravity.batch.testDone" : "antigravity.batch.done", { ...counts, total: ids.length }) +
          (action === "refresh" ? ` ${t("antigravity.batch.refreshExcluded", { count: antigravityRefreshTargets(accounts).apiKeyCount })}` : "") +
          (action === "test" && counts.current > results.size ? ` ${t("antigravity.batch.missingResults", { count: counts.current - results.size })}` : "") +
          (!["test", "refresh", "delete"].includes(action) ? ` ${t("antigravity.batch.countsOnly")}` : ""),
        error: counts.failed + counts.banned + counts.rate_limited > 0,
      });
    } catch {
      if (!mounted.current) return;
      interrupted = true;
      setProgressHidden(true); // Do not manufacture a successful complete frame.
      if (action === "test" || action === "refresh") {
        setPartialResults({ action: operationAction === "batch_test" ? "batch_test" : "batch_refresh", results: snapshotAccountOperationResults(results) });
      }
      setNotice({
        text: t(action === "refresh" ? "antigravity.batch.refreshInterrupted" : action === "export" ? "antigravity.batch.exportFailed" : "antigravity.batch.interrupted", {
          ...counts, total: ids.length, unknown: Math.max(0, ids.length - counts.current),
        }), error: true,
      });
    } finally {
      if (mounted.current && deleted.size) onDeleted?.([...deleted]);
      if (mounted.current && changed) {
        try { await onChanged(); }
        catch { if (mounted.current) setNotice((previous) => ({ text: `${previous?.text ?? ""} ${t("antigravity.batch.reloadFailed")}`, error: true })); }
      }
      if (mounted.current && interrupted) setProgressHidden(true);
      release();
    }
  };

  return <div className="space-y-3">
    {targets.length > 0 && <div className="toolbar-surface sticky bottom-4 z-20 flex flex-wrap items-center gap-2 rounded-lg border border-border bg-card p-3 shadow-sm" aria-busy={busy}>
      <div className="mr-auto text-sm">
        <span className="font-medium">{t("antigravity.batch.selected", { count: targets.length })}</span>
        {hiddenSelectedCount > 0 && <span className="ml-2 text-muted-foreground">{t("antigravity.batch.hidden", { count: hiddenSelectedCount })}</span>}
      </div>
      {(["test", "refresh", "enable", "disable"] as const).map((action) => <Button key={action} type="button" size="sm" variant="outline"
        disabled={blocked || !targets.length || (action === "refresh" && !refresh.accounts.length)}
        onClick={() => void execute(action, freezeAntigravityTargets(selectedAccounts))}>{title(action)}</Button>)}
      {(["groups", "proxy", "export", "delete"] as const).map((action) => <Button key={action} type="button" size="sm" variant={action === "delete" ? "destructive" : "outline"}
        disabled={blocked || !targets.length || (action === "export" && targets.length > ANTIGRAVITY_BATCH_SIZE)}
        onClick={() => openConfirmation(action)}>{title(action)}</Button>)}
      <Button type="button" variant="ghost" size="sm" disabled={blocked || !targets.length} onClick={() => { if (lock.current === "idle") onClearSelection(); }}>{t("antigravity.batch.clearSelection")}</Button>
      <p className="w-full text-xs text-muted-foreground">{t("antigravity.batch.refreshHint", { count: refresh.accounts.length, excluded: refresh.apiKeyCount, unknown: refresh.unknownCount })} {t("antigravity.batch.exportLimit", { count: ANTIGRAVITY_BATCH_SIZE })}</p>
      {busy && <p className="w-full text-xs text-muted-foreground">{t("antigravity.batch.closeNotCancel")}</p>}
    </div>}
    {notice && <div role={notice.error ? "alert" : "status"} className={`rounded-lg border p-3 text-sm ${notice.error ? "border-destructive/30 bg-destructive/5 text-destructive" : "border-border bg-muted/30"}`}>{notice.text}</div>}
    <Modal show={!!confirmation} title={confirmation ? title(confirmation.action) : ""} onClose={closeConfirmation} footer={<>
      <Button type="button" variant="outline" onClick={closeConfirmation} disabled={busy}>{t("antigravity.batch.cancel")}</Button>
      <Button type="button" variant={confirmation?.action === "delete" ? "destructive" : "default"} disabled={busy || !confirmation}
        onClick={() => { if (confirmation) void execute(confirmation.action, confirmation.accounts, true); }}>
        {t(confirmation?.action === "groups" && !groupIDs.length ? "antigravity.batch.confirmClearGroups" : confirmation?.action === "proxy" && !proxy.trim() ? "antigravity.batch.confirmClearProxy" : "antigravity.batch.confirm")}
      </Button>
    </>}>
      {confirmation && <div className="space-y-4">
        <p className="text-sm">{t("antigravity.batch.frozenTargets", { count: confirmation.accounts.length })}</p>
        <p className="text-sm text-muted-foreground">{t(`antigravity.batch.${confirmation.action}Confirm`)}</p>
        {confirmation.action === "groups" && <AccountGroupMultiSelect groups={groups.filter((group) => group.channel === "antigravity")} value={groupIDs} onChange={setGroupIDs}
          placeholder={t("antigravity.batch.chooseGroups")} emptyLabel={t("antigravity.batch.noGroups")}
          selectedLabel={t("antigravity.batch.groupCount", { count: groupIDs.length })} disabled={busy} />}
        {confirmation.action === "proxy" && <ProxyField value={proxy} onChange={setProxy} proxies={proxies} disabled={busy} label={t("antigravity.batch.proxy")} />}
        <ul className="max-h-40 overflow-auto rounded-md bg-muted/40 p-3 text-xs">
          {confirmation.accounts.slice(0, 20).map((account) => <li key={account.id} className="break-all">#{account.id} {account.email || account.name}</li>)}
          {confirmation.accounts.length > 20 && <li>{t("antigravity.batch.moreTargets", { count: confirmation.accounts.length - 20 })}</li>}
        </ul>
      </div>}
    </Modal>
    <OperationProgressToast progress={progressHidden ? null : operationProgress} onClose={() => setProgressHidden(true)} />
    <OperationResultsModal state={partialResults ?? operationResults} accounts={resultAccounts} channel="antigravity" onClose={() => { setPartialResults(null); closeOperationResults(); }} />
  </div>;
}

export default AntigravityBatchActions;
