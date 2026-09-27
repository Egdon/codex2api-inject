import { useEffect, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import {
  CheckCircle2,
  ExternalLink,
  Fingerprint,
  Loader2,
  XCircle,
} from "lucide-react";
import type { AccountRow } from "../types";
import { getAdminKey } from "../api";
import Modal from "./Modal";
import { Button } from "@/components/ui/button";
import { DraftNumberInput } from "@/components/ui/draft-number-input";
import { Select } from "@/components/ui/select";

interface DetectorResult {
  model: string;
  display_name: string;
  probability: number;
  profile_similarity: number;
  score: number;
  family: string;
  family_name: string;
  conditional_probability: number;
}

interface DetectorDiagnostic {
  index: number;
  parsed_numbers: number;
  minimum_numbers: number;
  accepted: boolean;
}

interface DetectorFamilyProbability {
  family: string;
  display_name: string;
  probability: number;
}

interface DetectorReport {
  prediction: string;
  prediction_name: string;
  probability: number;
  used_outputs: number;
  results: DetectorResult[];
  diagnostics: DetectorDiagnostic[];
  calibration: {
    queries: number;
    beta: number;
    cv_accuracy: number;
  };
  family_prediction: string;
  family_prediction_name: string;
  family_probability: number;
  family_probabilities: DetectorFamilyProbability[];
  method: string;
  candidate_scope: string;
  bank_schema: string;
  source: string;
  source_url: string;
  source_revision: string;
}

interface DetectorEvent {
  type: "start" | "progress" | "complete";
  index?: number;
  total?: number;
  attempt?: number;
  max_attempts?: number;
  concurrency?: number;
  model?: string;
  source?: string;
  source_revision?: string;
  probe_id?: string;
  status?: string;
  parsed_numbers?: number;
  minimum_numbers?: number;
  elapsed_ms?: number;
  error?: string;
  report?: DetectorReport;
}

const TARGET_OUTPUTS = 3;
const MAX_ATTEMPTS = 6;
const MAX_CONCURRENCY = 3;

function percentage(value: number | undefined, digits = 1) {
  if (!Number.isFinite(value)) return "-";
  return `${((value || 0) * 100).toFixed(digits)}%`;
}

export default function ModelDetectorModal({
  account,
  requestModels,
  defaultModel,
  onClose,
}: {
  account: AccountRow;
  requestModels: string[];
  defaultModel: string;
  onClose: () => void;
}) {
  const { t } = useTranslation();
  const [model, setModel] = useState(defaultModel || requestModels[0] || "");
  const [concurrency, setConcurrency] = useState(1);
  const [status, setStatus] = useState<
    "idle" | "running" | "complete" | "error"
  >("idle");
  const [progress, setProgress] = useState(0);
  const [total, setTotal] = useState(TARGET_OUTPUTS);
  const [attempt, setAttempt] = useState(0);
  const [maxAttempts, setMaxAttempts] = useState(MAX_ATTEMPTS);
  const [lastParsed, setLastParsed] = useState<number | null>(null);
  const [lastMinimum, setLastMinimum] = useState<number | null>(null);
  const [lastStatus, setLastStatus] = useState("");
  const [error, setError] = useState("");
  const [report, setReport] = useState<DetectorReport | null>(null);
  const abortRef = useRef<AbortController | null>(null);

  useEffect(() => () => abortRef.current?.abort(), []);

  const modelOptions = useMemo(() => {
    const merged = Array.from(
      new Set([defaultModel, ...requestModels].filter(Boolean)),
    );
    return merged.map((value) => ({ label: value, value }));
  }, [defaultModel, requestModels]);

  const start = async () => {
    if (!model || status === "running") return;
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    setStatus("running");
    setProgress(0);
    setTotal(TARGET_OUTPUTS);
    setAttempt(0);
    setMaxAttempts(MAX_ATTEMPTS);
    setLastParsed(null);
    setLastMinimum(null);
    setLastStatus("");
    setError("");
    setReport(null);

    try {
      const params = new URLSearchParams({
        model,
        concurrency: String(concurrency),
      });
      const response = await fetch(
        `/api/admin/accounts/${account.id}/model-detector?${params.toString()}`,
        {
          signal: controller.signal,
          headers: getAdminKey() ? { "X-Admin-Key": getAdminKey() } : {},
        },
      );
      if (!response.ok) {
        const body = await response.text();
        let message = `HTTP ${response.status}`;
        try {
          const parsed = JSON.parse(body) as { error?: string };
          if (parsed.error) message = parsed.error;
        } catch {
          // Keep the status fallback.
        }
        throw new Error(message);
      }
      const reader = response.body?.getReader();
      if (!reader) throw new Error(t("accounts.detectorStreamUnsupported"));
      const decoder = new TextDecoder();
      let buffer = "";
      let sawComplete = false;
      const consume = (lines: string[]) => {
        for (const line of lines) {
          const trimmed = line.trim();
          if (!trimmed.startsWith("data: ")) continue;
          try {
            const event = JSON.parse(trimmed.slice(6)) as DetectorEvent;
            if (event.type === "start") {
              setTotal(event.total || TARGET_OUTPUTS);
              setMaxAttempts(event.max_attempts || MAX_ATTEMPTS);
            } else if (event.type === "progress") {
              setProgress(event.index || 0);
              setTotal(event.total || TARGET_OUTPUTS);
              setAttempt(event.attempt || 0);
              setMaxAttempts(event.max_attempts || MAX_ATTEMPTS);
              setLastParsed(event.parsed_numbers ?? null);
              setLastMinimum(event.minimum_numbers ?? null);
              setLastStatus(event.status || "");
            } else if (event.type === "complete") {
              sawComplete = true;
              setProgress(event.index || 0);
              setTotal(event.total || TARGET_OUTPUTS);
              setAttempt(event.attempt || 0);
              setMaxAttempts(event.max_attempts || MAX_ATTEMPTS);
              if (event.report) {
                setReport(event.report);
                setStatus("complete");
              } else {
                setError(event.error || t("accounts.detectorFailed"));
                setStatus("error");
              }
            }
          } catch {
            // Ignore incomplete/non-SSE lines.
          }
        }
      };
      while (true) {
        const { done, value } = await reader.read();
        if (done) {
          buffer += decoder.decode();
          break;
        }
        buffer += decoder.decode(value, { stream: true });
        const lines = buffer.split("\n");
        buffer = lines.pop() || "";
        consume(lines);
      }
      if (buffer.trim()) consume([buffer]);
      if (!sawComplete) throw new Error(t("accounts.detectorConnectionEnded"));
    } catch (caught) {
      if (caught instanceof DOMException && caught.name === "AbortError")
        return;
      setStatus("error");
      setError(
        caught instanceof Error ? caught.message : t("accounts.detectorFailed"),
      );
    }
  };

  const running = status === "running";
  const rankedResults = report?.results || [];

  return (
    <Modal
      show={true}
      title={t("accounts.detectorTitle")}
      onClose={() => {
        abortRef.current?.abort();
        onClose();
      }}
      footer={
        <div className="flex w-full items-center justify-end gap-2">
          <Button
            variant="outline"
            onClick={() => {
              abortRef.current?.abort();
              onClose();
            }}
          >
            {t("common.close")}
          </Button>
          <Button disabled={running || !model} onClick={() => void start()}>
            {running ? (
              <Loader2 className="size-3.5 animate-spin" />
            ) : (
              <Fingerprint className="size-3.5" />
            )}
            {running
              ? t("accounts.detectorRunning")
              : t("accounts.detectorStart")}
          </Button>
        </div>
      }
      contentClassName="sm:max-w-[760px]"
    >
      <div className="space-y-4">
        <div className="rounded-lg border border-amber-200 bg-amber-50 px-4 py-3 text-xs leading-relaxed text-amber-800 dark:border-amber-900/50 dark:bg-amber-950/30 dark:text-amber-300">
          {t("accounts.detectorDisclaimer")}
        </div>

        <div className="grid gap-3 sm:grid-cols-[minmax(0,1fr)_8rem] sm:max-w-lg">
          <label className="block min-w-0 space-y-1 text-xs font-medium">
            <span>{t("accounts.detectorRequestModel")}</span>
            <Select
              value={model}
              onValueChange={setModel}
              options={modelOptions}
              disabled={running}
            />
          </label>
          <label className="block space-y-1 text-xs font-medium">
            <span>{t("accounts.detectorConcurrency")}</span>
            <DraftNumberInput
              min={1}
              max={MAX_CONCURRENCY}
              inputMode="numeric"
              value={concurrency}
              onValueChange={setConcurrency}
              disabled={running}
            />
          </label>
        </div>

        {running && (
          <div
            className="space-y-2 rounded-lg border border-border bg-muted/30 px-4 py-3"
            aria-live="polite"
          >
            <div className="flex items-center justify-between gap-3 text-xs">
              <span>{t("accounts.detectorProgress")}</span>
              <span className="tabular-nums">
                {progress} / {total}
              </span>
            </div>
            <div className="h-1.5 overflow-hidden rounded-full bg-muted">
              <div
                className="h-full rounded-full bg-primary transition-[width]"
                style={{
                  width: `${total ? Math.min(100, (progress / total) * 100) : 0}%`,
                }}
              />
            </div>
            <div className="flex flex-wrap items-center justify-between gap-2 text-[11px] text-muted-foreground">
              <span>
                {t("accounts.detectorAttempt", {
                  current: attempt,
                  total: maxAttempts,
                })}
              </span>
              {lastParsed !== null && lastMinimum !== null ? (
                <span
                  className={
                    lastStatus === "ok"
                      ? "text-emerald-600 dark:text-emerald-400"
                      : "text-amber-700 dark:text-amber-300"
                  }
                >
                  {t("accounts.detectorParsedNumbers", {
                    parsed: lastParsed,
                    minimum: lastMinimum,
                  })}
                </span>
              ) : null}
            </div>
          </div>
        )}

        {error && (
          <div
            className="flex items-start gap-2 rounded-lg border border-red-200 bg-red-50 px-4 py-3 text-xs text-red-700 dark:border-red-900/50 dark:bg-red-950/30 dark:text-red-400"
            aria-live="polite"
          >
            <XCircle className="mt-0.5 size-4 shrink-0" />
            <span>{error}</span>
          </div>
        )}

        {report && (
          <div
            className="overflow-hidden rounded-lg border border-border"
            aria-live="polite"
          >
            <div className="grid divide-y divide-border sm:grid-cols-3 sm:divide-x sm:divide-y-0">
              <div className="px-4 py-3">
                <div className="text-[11px] text-muted-foreground">
                  {t("accounts.detectorPrediction")}
                </div>
                <div className="mt-1 break-all font-mono text-sm font-semibold">
                  {report.prediction_name || report.prediction}
                </div>
              </div>
              <div className="px-4 py-3">
                <div className="text-[11px] text-muted-foreground">
                  {t("accounts.detectorProbability")}
                </div>
                <div className="mt-1 text-sm font-semibold tabular-nums">
                  {percentage(report.probability)}
                </div>
              </div>
              <div className="px-4 py-3">
                <div className="text-[11px] text-muted-foreground">
                  {t("accounts.detectorFamily")}
                </div>
                <div className="mt-1 text-sm font-semibold">
                  {report.family_prediction_name}
                </div>
                <div className="text-[11px] tabular-nums text-muted-foreground">
                  {percentage(report.family_probability)}{" "}
                  {t("accounts.detectorFamilyProbability")}
                </div>
              </div>
            </div>

            <div className="border-t border-border px-4 py-3">
              <div className="mb-2 text-xs font-medium">
                {t("accounts.detectorCandidateRanking")}
              </div>
              <div className="space-y-2">
                {rankedResults.map((result) => (
                  <div
                    key={result.model}
                    className="grid grid-cols-[minmax(0,9rem)_minmax(5rem,1fr)_3.5rem] items-center gap-2 text-xs sm:grid-cols-[minmax(0,12rem)_minmax(8rem,1fr)_3.5rem_3.5rem]"
                  >
                    <span className="min-w-0">
                      <span
                        className="block truncate font-mono"
                        title={result.display_name || result.model}
                      >
                        {result.display_name || result.model}
                      </span>
                      <span className="block truncate text-[10px] text-muted-foreground">
                        {result.family_name}
                      </span>
                    </span>
                    <div className="h-1.5 overflow-hidden rounded-full bg-muted">
                      <div
                        className="h-full rounded-full bg-primary"
                        style={{
                          width: `${Math.max(0, Math.min(100, result.probability * 100))}%`,
                        }}
                      />
                    </div>
                    <span className="text-right tabular-nums text-muted-foreground">
                      {percentage(result.probability)}
                    </span>
                    <span
                      className="hidden text-right tabular-nums text-muted-foreground sm:inline"
                      title={t("accounts.detectorProfileSimilarity")}
                    >
                      {percentage(result.profile_similarity)}
                    </span>
                  </div>
                ))}
              </div>
            </div>

            <div className="border-t border-border px-4 py-3">
              <div className="mb-2 text-xs font-medium">
                {t("accounts.detectorDiagnostics")}
              </div>
              <div className="flex flex-wrap gap-2">
                {report.diagnostics.map((diagnostic) => (
                  <span
                    key={diagnostic.index}
                    className={`inline-flex items-center gap-1 rounded-md border px-2 py-1 text-[11px] ${diagnostic.accepted ? "border-emerald-200 text-emerald-700 dark:border-emerald-900 dark:text-emerald-400" : "border-amber-200 text-amber-700 dark:border-amber-900 dark:text-amber-300"}`}
                  >
                    {diagnostic.accepted ? (
                      <CheckCircle2 className="size-3" />
                    ) : (
                      <XCircle className="size-3" />
                    )}
                    {t("accounts.detectorDiagnosticItem", {
                      index: diagnostic.index + 1,
                      parsed: diagnostic.parsed_numbers,
                      minimum: diagnostic.minimum_numbers,
                    })}
                  </span>
                ))}
              </div>
            </div>

            <div className="border-t border-border bg-muted/20 px-4 py-3 text-[11px] leading-relaxed text-muted-foreground">
              <div>{t("accounts.detectorClosedSetWarning")}</div>
              <div className="mt-1 flex flex-wrap items-center gap-x-3 gap-y-1">
                <span>
                  {report.used_outputs} {t("accounts.detectorValidSamples")}
                </span>
                <span>
                  {t("accounts.detectorCalibration", {
                    accuracy: percentage(report.calibration.cv_accuracy),
                  })}
                </span>
                <a
                  className="inline-flex items-center gap-1 text-primary hover:underline"
                  href={report.source_url}
                  target="_blank"
                  rel="noreferrer"
                >
                  {report.source}
                  <ExternalLink className="size-3" />
                </a>
              </div>
            </div>
          </div>
        )}
      </div>
    </Modal>
  );
}
