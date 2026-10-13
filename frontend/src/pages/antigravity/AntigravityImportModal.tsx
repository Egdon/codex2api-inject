import { useCallback, useRef, useState } from "react";
import type { ChangeEvent, DragEvent } from "react";
import { useTranslation } from "react-i18next";
import {
  CheckCircle2,
  ClipboardPaste,
  FileJson,
  FolderOpen,
  Loader2,
  TriangleAlert,
  Upload,
  X,
  XCircle,
} from "lucide-react";
import { api } from "../../api";
import type { ProxyRow } from "../../api";
import type {
  AccountGroup,
  AntigravityImportItem,
  AntigravityImportResponse,
} from "../../types";
import Modal from "../../components/Modal";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { useToast } from "../../hooks/useToast";
import {
  ANTIGRAVITY_IMPORT_BATCH_MAX,
  mergeAntigravityImportResponses,
  parseRefreshTokenLines,
  planAntigravityImportBatches,
} from "../../lib/antigravityQuota";
import { getErrorMessage } from "../../utils/error";
import {
  AccountMetadataFields,
  FieldHint,
  FieldLabel,
  OptionalSettingsSection,
  SegmentedChoice,
  TEXTAREA_CLASS,
} from "./AntigravityFormFields";

type ImportMode = "files" | "tokens";

interface CredentialFile {
  name: string;
  size: number;
  content: string;
}

interface ImportDraft {
  refreshTokens: string;
  proxyUrl: string;
  groupIds: number[];
  // importFileProxies 只控制"文件内代理是否注册进代理表"。该渠道的导入一直会采用
  // 文件里的 proxy_url，关掉它只是让那些代理停在账号绑定上、不进代理池。
  importFileProxies: boolean;
}

const EMPTY_IMPORT_DRAFT: ImportDraft = {
  refreshTokens: "",
  proxyUrl: "",
  groupIds: [],
  importFileProxies: false,
};

function importItemSourceLabel(item: AntigravityImportItem): string {
  const suffix = item.sub_index && item.sub_index > 0 ? `.${item.sub_index}` : "";
  return `#${item.index}${suffix}`;
}

function importItemDisplayLabel(item: AntigravityImportItem): string {
  const source = importItemSourceLabel(item);
  return item.email ? `${item.email} (${source})` : source;
}

function formatFileSize(bytes: number): string {
  if (bytes >= 1024 * 1024) return `${(bytes / 1024 / 1024).toFixed(1)} MB`;
  if (bytes >= 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${bytes} B`;
}

function ImportResultSummary({ result }: { result: AntigravityImportResponse }) {
  const { t } = useTranslation();
  const problems = result.items.filter((item) => !item.ok || !item.synced || item.warning || item.error);
  const chips: Array<{ key: string; label: string; value: number; tone: string }> = [
    { key: "imported", label: t("antigravity.importResultImported"), value: result.imported, tone: "bg-emerald-500/10 text-emerald-700 dark:text-emerald-300" },
    { key: "synced", label: t("antigravity.importResultSynced"), value: result.synced ?? 0, tone: "bg-sky-500/10 text-sky-700 dark:text-sky-300" },
    { key: "degraded", label: t("antigravity.importResultDegraded"), value: result.degraded ?? 0, tone: "bg-amber-500/10 text-amber-700 dark:text-amber-300" },
    { key: "failed", label: t("antigravity.importResultFailed"), value: result.failed, tone: "bg-red-500/10 text-red-700 dark:text-red-300" },
  ];
  return (
    <div className="space-y-2 rounded-xl border border-amber-500/30 bg-amber-500/5 p-3">
      <div className="flex flex-wrap items-center gap-1.5">
        {chips.map((chip) => (
          <span key={chip.key} className={cn("inline-flex items-center gap-1 rounded-md px-2 py-0.5 text-[11px] font-medium tabular-nums", chip.tone)}>
            {chip.label}
            <span className="font-semibold">{chip.value}</span>
          </span>
        ))}
      </div>
      {problems.length > 0 ? (
        <div className="max-h-36 space-y-1 overflow-y-auto pr-1 text-xs">
          {problems.slice(0, 50).map((item) => (
            <div
              key={`${item.index}-${item.sub_index ?? 0}-${item.email ?? "item"}`}
              className="flex items-start gap-1.5"
            >
              {item.ok ? (
                <TriangleAlert className="mt-0.5 size-3 shrink-0 text-amber-500" aria-hidden />
              ) : (
                <XCircle className="mt-0.5 size-3 shrink-0 text-red-500" aria-hidden />
              )}
              <span className="min-w-0 break-words text-muted-foreground">
                <span className="font-medium text-foreground">{importItemDisplayLabel(item)}</span>
                {": "}
                {item.error || item.warning || t("antigravity.syncPending")}
              </span>
            </div>
          ))}
        </div>
      ) : null}
    </div>
  );
}

/** 批量导入:多个 JSON 凭据文件(可拖拽)或多行 refresh token。单个账号走"添加账号"。 */
export default function AntigravityImportModal({
  show,
  proxies,
  groups,
  onCreateGroup,
  onClose,
  onImported,
}: {
  show: boolean;
  proxies: ProxyRow[];
  groups: AccountGroup[];
  onCreateGroup: (name: string) => Promise<number | null>;
  onClose: () => void;
  onImported: () => Promise<void> | void;
}) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const fileInputRef = useRef<HTMLInputElement | null>(null);
  const [mode, setMode] = useState<ImportMode>("files");
  const [draft, setDraft] = useState<ImportDraft>(EMPTY_IMPORT_DRAFT);
  const [files, setFiles] = useState<CredentialFile[]>([]);
  const [dragging, setDragging] = useState(false);
  const [importing, setImporting] = useState(false);
  const [batchProgress, setBatchProgress] = useState<{ current: number; total: number } | null>(null);
  const [result, setResult] = useState<AntigravityImportResponse | null>(null);

  const tokens = parseRefreshTokenLines(draft.refreshTokens);

  const reset = useCallback(() => {
    setDraft(EMPTY_IMPORT_DRAFT);
    setFiles([]);
    setResult(null);
    setMode("files");
    setDragging(false);
    if (fileInputRef.current) fileInputRef.current.value = "";
  }, []);

  const close = useCallback(() => {
    if (importing) return;
    onClose();
    reset();
  }, [importing, onClose, reset]);

  const addFiles = async (list: FileList | File[]) => {
    const picked = Array.from(list).filter(
      (file) => file.name.toLowerCase().endsWith(".json") || file.type === "application/json",
    );
    if (picked.length === 0) {
      showToast(t("antigravity.filesJsonOnly"), "error");
      return;
    }
    try {
      const contents = await Promise.all(
        picked.map(async (file) => ({ name: file.name, size: file.size, content: await file.text() })),
      );
      setFiles((current) => {
        const byName = new Map(current.map((file) => [file.name, file]));
        for (const file of contents) byName.set(file.name, file);
        return Array.from(byName.values());
      });
      setResult(null);
    } catch (fileError) {
      showToast(getErrorMessage(fileError), "error");
    }
  };

  const handleFileInput = async (event: ChangeEvent<HTMLInputElement>) => {
    if (event.target.files?.length) await addFiles(event.target.files);
    event.target.value = "";
  };

  const handleDrop = async (event: DragEvent<HTMLElement>) => {
    event.preventDefault();
    setDragging(false);
    if (event.dataTransfer.files?.length) await addFiles(event.dataTransfer.files);
  };

  const summarizeBatchResult = (response: AntigravityImportResponse) => {
    const needsReview = response.failed > 0 || (response.degraded ?? 0) > 0;
    const proxyWarning = response.proxy_warning?.trim();
    // Toast 只有一个槽位,连续调用会互相顶掉——代理结果和告警必须拼进同一条。
    const summary = needsReview
      ? t("antigravity.importReview", {
          imported: response.imported,
          synced: response.synced ?? 0,
          degraded: response.degraded ?? 0,
          failed: response.failed,
        })
      : t("antigravity.importDone", { imported: response.imported, total: response.total });
    const parts = [summary];
    if (response.proxies_imported !== undefined) {
      parts.push(
        t("accounts.importProxySummary", {
          imported: response.proxies_imported,
          skipped: response.proxies_skipped ?? 0,
        }),
      );
      if (proxyWarning) parts.push(proxyWarning);
    }
    showToast(parts.join(" "), needsReview || proxyWarning ? "warning" : "success");
    return needsReview;
  };

  const handleImport = async () => {
    if (importing) return;
    if (mode === "files" && files.length === 0) {
      showToast(t("antigravity.filesRequired"), "error");
      return;
    }
    if (mode === "tokens" && tokens.length === 0) {
      showToast(t("antigravity.refreshTokensRequired"), "error");
      return;
    }
    setImporting(true);
    setResult(null);
    // 超过单次上限(或单个文件过大)时自动分批:导出的多账号 JSON 数组可以整份导入。
    const batches =
      mode === "files"
        ? planAntigravityImportBatches(files.map((file) => file.content))
        : Array.from({ length: Math.ceil(tokens.length / ANTIGRAVITY_IMPORT_BATCH_MAX) }, (_, index) =>
            tokens.slice(index * ANTIGRAVITY_IMPORT_BATCH_MAX, (index + 1) * ANTIGRAVITY_IMPORT_BATCH_MAX),
          );
    const collected: Array<{ response: AntigravityImportResponse; offset: number }> = [];
    let offset = 0;
    try {
      for (const [index, batch] of batches.entries()) {
        if (batches.length > 1) setBatchProgress({ current: index + 1, total: batches.length });
        const response = await api.importAntigravityAccounts({
          files: mode === "files" ? batch : [],
          refresh_tokens: mode === "tokens" ? batch : undefined,
          proxy_url: draft.proxyUrl.trim() || undefined,
          group_ids: draft.groupIds,
          import_proxy: mode === "files" ? draft.importFileProxies || undefined : undefined,
        });
        collected.push({ response, offset });
        offset += batch.length;
      }
      const merged = collected.length === 1 ? collected[0].response : mergeAntigravityImportResponses(collected);
      setResult(merged);
      if (!summarizeBatchResult(merged)) {
        onClose();
        reset();
      }
      await onImported();
    } catch (importError) {
      const message = getErrorMessage(importError);
      if (collected.length > 0) {
        // 已完成的批次照常展示并刷新列表,失败批次的错误单独提示。
        setResult(mergeAntigravityImportResponses(collected));
        showToast(
          t("antigravity.importBatchStopped", { done: collected.length, total: batches.length, error: message }),
          "error",
        );
        await onImported();
      } else {
        showToast(message, "error");
      }
    } finally {
      setImporting(false);
      setBatchProgress(null);
    }
  };

  const totalSize = files.reduce((sum, file) => sum + file.size, 0);
  const pending = mode === "files" ? files.length : tokens.length;
  const submitLabel = importing
    ? batchProgress
      ? t("antigravity.importingBatch", batchProgress)
      : t("antigravity.importing")
    : mode === "files"
      ? t("antigravity.importFilesCount", { count: files.length })
      : t("antigravity.importTokensCount", { count: tokens.length });

  return (
    <Modal
      show={show}
      title={t("antigravity.importTitle")}
      onClose={close}
      contentClassName="sm:max-w-[640px]"
      footer={
        <>
          <Button variant="outline" onClick={close} disabled={importing}>
            {t("common.cancel")}
          </Button>
          <Button onClick={() => void handleImport()} disabled={importing || pending === 0}>
            {importing ? <Loader2 className="animate-spin" /> : <Upload />}
            {submitLabel}
          </Button>
        </>
      }
    >
      <div className="space-y-4">
        <SegmentedChoice<ImportMode>
          value={mode}
          onChange={(next) => {
            setMode(next);
            setResult(null);
          }}
          options={[
            { value: "files", label: t("antigravity.batchFiles"), icon: <FolderOpen className="size-4" /> },
            { value: "tokens", label: t("antigravity.refreshTokensMode"), icon: <ClipboardPaste className="size-4" /> },
          ]}
        />

        {mode === "files" ? (
          <div className="space-y-2">
            <input
              ref={fileInputRef}
              type="file"
              accept=".json,application/json"
              multiple
              className="hidden"
              onChange={(event) => void handleFileInput(event)}
            />
            <button
              type="button"
              onClick={() => fileInputRef.current?.click()}
              onDragOver={(event) => {
                event.preventDefault();
                setDragging(true);
              }}
              onDragLeave={() => setDragging(false)}
              onDrop={(event) => void handleDrop(event)}
              className={cn(
                "flex w-full flex-col items-center justify-center gap-2 rounded-xl border border-dashed px-5 text-center transition-colors",
                files.length > 0 ? "min-h-[96px] py-4" : "min-h-[150px] py-6",
                dragging
                  ? "border-primary bg-primary/10"
                  : "border-border bg-muted/20 hover:border-primary/40 hover:bg-primary/5",
              )}
            >
              <span className="flex size-10 items-center justify-center rounded-xl bg-primary/10 text-primary">
                <FolderOpen className="size-5" />
              </span>
              <span className="text-sm font-semibold text-foreground">
                {dragging
                  ? t("antigravity.dropFilesHere")
                  : files.length > 0
                    ? t("antigravity.addMoreFiles")
                    : t("antigravity.chooseFiles")}
              </span>
              <span className="text-xs text-muted-foreground">{t("antigravity.chooseFilesHint")}</span>
            </button>
            {files.length > 0 ? (
              <div className="rounded-xl border border-border">
                <div className="flex items-center justify-between border-b border-border/70 px-3 py-1.5 text-[11px] text-muted-foreground">
                  <span>
                    {t("antigravity.filesSelected", { count: files.length })} · {formatFileSize(totalSize)}
                  </span>
                  <button
                    type="button"
                    className="font-medium transition-colors hover:text-foreground"
                    onClick={() => setFiles([])}
                  >
                    {t("antigravity.clearFiles")}
                  </button>
                </div>
                <div className="max-h-36 overflow-y-auto px-1.5 py-1">
                  {files.map((file) => (
                    <div key={file.name} className="flex items-center gap-2 rounded-md px-1.5 py-1 text-xs hover:bg-muted/40">
                      <FileJson className="size-3.5 shrink-0 text-muted-foreground" />
                      <span className="min-w-0 flex-1 truncate">{file.name}</span>
                      <span className="shrink-0 tabular-nums text-[10px] text-muted-foreground">{formatFileSize(file.size)}</span>
                      <button
                        type="button"
                        className="flex size-5 shrink-0 items-center justify-center rounded text-muted-foreground hover:bg-muted hover:text-foreground"
                        aria-label={t("antigravity.removeFile", { name: file.name })}
                        onClick={() => setFiles((current) => current.filter((item) => item.name !== file.name))}
                      >
                        <X className="size-3" />
                      </button>
                    </div>
                  ))}
                </div>
              </div>
            ) : null}
            <label className="flex cursor-pointer items-start gap-2 rounded-lg px-1 py-1 text-xs text-muted-foreground">
              <input
                type="checkbox"
                className="mt-0.5 size-3.5 accent-primary"
                checked={draft.importFileProxies}
                onChange={(event) =>
                  setDraft((current) => ({ ...current, importFileProxies: event.target.checked }))
                }
              />
              <span>
                <span className="font-medium text-foreground">{t("accounts.importFileProxies")}</span>
                <span className="mt-0.5 block text-[11px]">{t("antigravity.importFileProxiesHint")}</span>
              </span>
            </label>
          </div>
        ) : (
          <label className="block space-y-1.5">
            <span className="flex items-center justify-between gap-2">
              <FieldLabel>{t("antigravity.refreshTokensLabel")}</FieldLabel>
              {tokens.length > 0 ? (
                <span className="inline-flex items-center gap-1 text-[11px] font-medium text-emerald-700 dark:text-emerald-300">
                  <CheckCircle2 className="size-3" aria-hidden />
                  {t("antigravity.refreshTokensDetected", { count: tokens.length })}
                </span>
              ) : null}
            </span>
            <textarea
              value={draft.refreshTokens}
              onChange={(event) => {
                setDraft((current) => ({ ...current, refreshTokens: event.target.value }));
                setResult(null);
              }}
              rows={9}
              spellCheck={false}
              placeholder={t("antigravity.refreshTokensPlaceholder")}
              className={TEXTAREA_CLASS}
            />
            <FieldHint>{t("antigravity.refreshTokensHint")}</FieldHint>
          </label>
        )}

        <OptionalSettingsSection proxyUrl={draft.proxyUrl} groupIds={draft.groupIds} groups={groups}>
          <AccountMetadataFields
            stacked
            proxyUrl={draft.proxyUrl}
            proxies={proxies}
            onProxyUrlChange={(proxyUrl) => setDraft((current) => ({ ...current, proxyUrl }))}
            groupIds={draft.groupIds}
            onGroupIdsChange={(groupIds) => setDraft((current) => ({ ...current, groupIds }))}
            groups={groups}
            onCreateGroup={onCreateGroup}
          />
        </OptionalSettingsSection>

        {result && (result.failed > 0 || (result.degraded ?? 0) > 0) ? <ImportResultSummary result={result} /> : null}
      </div>
    </Modal>
  );
}
