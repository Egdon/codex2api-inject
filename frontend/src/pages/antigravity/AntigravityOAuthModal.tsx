import { useCallback, useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import {
  Check,
  CheckCircle2,
  Copy,
  ExternalLink,
  Link2,
  Loader2,
  RotateCw,
  TriangleAlert,
} from "lucide-react";
import { api } from "../../api";
import type { ProxyRow } from "../../api";
import type {
  AccountGroup,
  AntigravityOAuthStartResponse,
  AntigravityOAuthStatusResponse,
} from "../../types";
import Modal from "../../components/Modal";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";
import { useToast } from "../../hooks/useToast";
import { getErrorMessage } from "../../utils/error";
import {
  AccountMetadataFields,
  FieldHint,
  FieldLabel,
  OptionalSettingsSection,
  TEXTAREA_CLASS,
} from "./AntigravityFormFields";

type OAuthModalStatus = "idle" | "starting" | "waiting" | "processing" | "completed" | "failed" | "cancelled";

interface OAuthDraft {
  name: string;
  proxyUrl: string;
  groupIds: number[];
}

const EMPTY_OAUTH_DRAFT: OAuthDraft = { name: "", proxyUrl: "", groupIds: [] };

function StepIndicator({ step, failed }: { step: 1 | 2 | 3; failed: boolean }) {
  const { t } = useTranslation();
  const steps = [
    t("antigravity.oauthStepAccount"),
    t("antigravity.oauthStepAuthorize"),
    t("antigravity.oauthStepDone"),
  ];
  return (
    <ol className="flex items-center gap-2">
      {steps.map((label, index) => {
        const number = index + 1;
        const done = number < step || (number === 3 && step === 3);
        const current = number === step && !done;
        return (
          <li key={label} className="flex min-w-0 flex-1 items-center gap-2">
            <span
              className={cn(
                "flex size-6 shrink-0 items-center justify-center rounded-full text-[11px] font-semibold ring-1 ring-inset",
                done
                  ? "bg-emerald-500 text-white ring-emerald-500"
                  : current && failed
                    ? "bg-red-500/10 text-red-600 ring-red-500/40 dark:text-red-300"
                    : current
                      ? "bg-primary text-primary-foreground ring-primary"
                      : "bg-muted text-muted-foreground ring-border",
              )}
            >
              {done ? <Check className="size-3.5" /> : number}
            </span>
            <span className={cn("truncate text-xs font-medium", current || done ? "text-foreground" : "text-muted-foreground")}>
              {label}
            </span>
            {number < steps.length ? <span className="h-px min-w-3 flex-1 bg-border" aria-hidden /> : null}
          </li>
        );
      })}
    </ol>
  );
}

/** Google OAuth 添加账号:启动授权会话 → 轮询状态 → 必要时手动粘贴回调地址。 */
export default function AntigravityOAuthModal({
  proxies,
  groups,
  onCreateGroup,
  onClose,
  onCompleted,
}: {
  proxies: ProxyRow[];
  groups: AccountGroup[];
  onCreateGroup: (name: string) => Promise<number | null>;
  onClose: () => void;
  onCompleted: () => Promise<void> | void;
}) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const [draft, setDraft] = useState<OAuthDraft>(EMPTY_OAUTH_DRAFT);
  const [session, setSession] = useState<AntigravityOAuthStartResponse | null>(null);
  const [status, setStatus] = useState<AntigravityOAuthStatusResponse | null>(null);
  const [callbackURL, setCallbackURL] = useState("");
  const [modalStatus, setModalStatus] = useState<OAuthModalStatus>("idle");
  const [starting, setStarting] = useState(false);
  const [completing, setCompleting] = useState(false);
  const [cancelling, setCancelling] = useState(false);
  const [copied, setCopied] = useState(false);
  const pollTimerRef = useRef<number | null>(null);
  const pollInFlightRef = useRef(false);
  const pollGenerationRef = useRef(0);

  const stopPolling = useCallback(() => {
    pollGenerationRef.current += 1;
    pollInFlightRef.current = false;
    if (pollTimerRef.current !== null) {
      window.clearTimeout(pollTimerRef.current);
      pollTimerRef.current = null;
    }
  }, []);

  const resetSession = useCallback(() => {
    stopPolling();
    setSession(null);
    setStatus(null);
    setCallbackURL("");
    setModalStatus("idle");
  }, [stopPolling]);

  useEffect(() => () => stopPolling(), [stopPolling]);

  const pollStatus = useCallback(
    async (sessionId: string) => {
      if (pollInFlightRef.current) return;
      const generation = pollGenerationRef.current;
      pollInFlightRef.current = true;
      try {
        const result = await api.getAntigravityOAuthStatus(sessionId);
        if (generation !== pollGenerationRef.current) return;
        setStatus(result);
        if (result.status === "completed") {
          stopPolling();
          setModalStatus("completed");
          showToast(
            result.email
              ? t("antigravity.oauthSuccessWithEmail", { email: result.email })
              : t("antigravity.oauthSuccess"),
            result.warning ? "warning" : "success",
          );
          await onCompleted();
          return;
        }
        if (result.status === "failed" || result.status === "cancelled") {
          stopPolling();
          setModalStatus(result.status);
          if (result.error) showToast(result.error, "error");
          return;
        }
        setModalStatus(result.status === "processing" ? "processing" : "waiting");
        pollTimerRef.current = window.setTimeout(() => {
          if (generation !== pollGenerationRef.current) return;
          pollTimerRef.current = null;
          void pollStatus(sessionId);
        }, 1200);
      } catch (pollError) {
        if (generation !== pollGenerationRef.current) return;
        stopPolling();
        const message = getErrorMessage(pollError);
        setModalStatus("failed");
        setStatus((current) =>
          current
            ? { ...current, status: "failed", error: message }
            : { session_id: sessionId, status: "failed", error: message, expires_at: new Date().toISOString() },
        );
        showToast(message, "error");
      } finally {
        pollInFlightRef.current = false;
      }
    },
    [onCompleted, showToast, stopPolling, t],
  );

  const handleStart = useCallback(async () => {
    // Open synchronously inside the click handler so browsers do not classify
    // the later OAuth navigation (after the API round trip) as a blocked
    // popup. The window is only navigated after the server returns a
    // state-bound authorization URL.
    const popup = window.open("about:blank", "_blank");
    setStarting(true);
    stopPolling();
    setStatus(null);
    setCallbackURL("");
    setModalStatus("starting");
    try {
      const result = await api.startAntigravityOAuth({
        name: draft.name.trim() || undefined,
        proxy_url: draft.proxyUrl.trim() || undefined,
        group_ids: draft.groupIds,
      });
      setSession(result);
      setStatus({ session_id: result.session_id, status: "waiting", expires_at: result.expires_at });
      setModalStatus("waiting");
      if (popup) {
        try {
          popup.opener = null;
          popup.location.href = result.auth_url;
        } catch {
          showToast(t("antigravity.oauthPopupBlocked"), "warning");
        }
      } else {
        showToast(t("antigravity.oauthPopupBlocked"), "warning");
      }
      pollTimerRef.current = window.setTimeout(() => {
        pollTimerRef.current = null;
        void pollStatus(result.session_id);
      }, 700);
    } catch (startError) {
      try {
        popup?.close();
      } catch {
        // Ignore a popup that the user already closed.
      }
      setModalStatus("failed");
      showToast(getErrorMessage(startError), "error");
    } finally {
      setStarting(false);
    }
  }, [draft.groupIds, draft.name, draft.proxyUrl, pollStatus, showToast, stopPolling, t]);

  const handleComplete = useCallback(async () => {
    if (!session) return;
    const url = callbackURL.trim();
    if (!url) {
      showToast(t("antigravity.oauthCallbackRequired"), "error");
      return;
    }
    setCompleting(true);
    setModalStatus("processing");
    setStatus((current) => (current ? { ...current, status: "processing", error: "" } : current));
    try {
      await api.completeAntigravityOAuth({ session_id: session.session_id, callback_url: url });
      setCallbackURL("");
      stopPolling();
      pollTimerRef.current = window.setTimeout(() => {
        pollTimerRef.current = null;
        void pollStatus(session.session_id);
      }, 250);
    } catch (completeError) {
      const message = getErrorMessage(completeError);
      // Validation failures (for example a pasted URL with the wrong state)
      // leave the server session in `waiting`. Keep that state visible so the
      // user can correct the URL or cancel it cleanly instead of orphaning a
      // loopback listener by treating every 4xx as terminal.
      setModalStatus("waiting");
      setStatus((current) => (current ? { ...current, status: "failed", error: message } : current));
      showToast(message, "error");
      pollTimerRef.current = window.setTimeout(() => {
        pollTimerRef.current = null;
        void pollStatus(session.session_id);
      }, 500);
    } finally {
      setCompleting(false);
    }
  }, [callbackURL, pollStatus, session, showToast, stopPolling, t]);

  const handleCancel = useCallback(async () => {
    const sessionId = session?.session_id;
    stopPolling();
    if (!sessionId) {
      resetSession();
      onClose();
      return;
    }
    setCancelling(true);
    try {
      await api.cancelAntigravityOAuth(sessionId);
      if (status?.status !== "completed") {
        showToast(t("antigravity.oauthCancelled"), "success");
      }
    } catch (cancelError) {
      showToast(getErrorMessage(cancelError), "error");
    } finally {
      setCancelling(false);
      resetSession();
      onClose();
    }
  }, [onClose, resetSession, session, showToast, status?.status, stopPolling, t]);

  const copyAuthURL = async () => {
    if (!session?.auth_url) return;
    try {
      await navigator.clipboard.writeText(session.auth_url);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    } catch {
      showToast(t("common.copyFailed"), "error");
    }
  };

  const terminal = modalStatus === "completed" || modalStatus === "failed" || modalStatus === "cancelled";
  const failed = modalStatus === "failed" || modalStatus === "cancelled";
  const step: 1 | 2 | 3 = !session ? 1 : modalStatus === "completed" ? 3 : 2;

  return (
    <Modal
      show
      title={t("antigravity.oauthTitle")}
      onClose={() => {
        if (!starting && !completing && !cancelling) void handleCancel();
      }}
      contentClassName="sm:max-w-[640px]"
      footer={
        <>
          <Button variant="outline" onClick={() => void handleCancel()} disabled={starting || completing || cancelling}>
            {terminal ? t("common.close") : t("common.cancel")}
          </Button>
          {!session ? (
            <Button onClick={() => void handleStart()} disabled={starting}>
              {starting ? <Loader2 className="animate-spin" /> : <Link2 />}
              {starting ? t("antigravity.oauthStarting") : t("antigravity.oauthStart")}
            </Button>
          ) : failed ? (
            <Button onClick={resetSession} disabled={cancelling}>
              <RotateCw />
              {t("antigravity.oauthRetry")}
            </Button>
          ) : modalStatus === "completed" ? null : (
            <Button onClick={() => void handleComplete()} disabled={completing || !callbackURL.trim()}>
              {completing ? <Loader2 className="animate-spin" /> : <Link2 />}
              {completing ? t("antigravity.oauthSubmitting") : t("antigravity.oauthComplete")}
            </Button>
          )}
        </>
      }
    >
      <div className="space-y-4">
        <StepIndicator step={step} failed={failed} />
        {!session ? (
          <>
            <div className="rounded-xl border border-border bg-muted/30 px-4 py-3 text-sm text-muted-foreground">
              <p className="mb-1 font-semibold text-foreground">{t("antigravity.oauthStep1Title")}</p>
              <p>{t("antigravity.oauthStep1Description")}</p>
            </div>
            <label className="block space-y-1.5">
              <FieldLabel>{t("antigravity.accountName")}</FieldLabel>
              <Input
                value={draft.name}
                onChange={(event) => setDraft((current) => ({ ...current, name: event.target.value }))}
                placeholder={t("antigravity.accountNamePlaceholder")}
              />
            </label>
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
          </>
        ) : (
          <>
            <div
              className={cn(
                "rounded-xl border px-4 py-3 text-sm",
                failed
                  ? "border-red-500/30 bg-red-500/5 text-red-700 dark:text-red-300"
                  : modalStatus === "completed"
                    ? "border-emerald-500/30 bg-emerald-500/5 text-emerald-700 dark:text-emerald-300"
                    : "border-primary/30 bg-primary/5 text-muted-foreground",
              )}
            >
              <div className="flex items-center gap-2 font-semibold text-foreground">
                {modalStatus === "completed" ? (
                  <CheckCircle2 className="size-4 text-emerald-500" />
                ) : failed ? (
                  <TriangleAlert className="size-4 text-red-500" />
                ) : (
                  <Loader2 className="size-4 animate-spin text-primary" />
                )}
                {modalStatus === "waiting"
                  ? t("antigravity.oauthWaiting")
                  : modalStatus === "processing"
                    ? t("antigravity.oauthProcessing")
                    : modalStatus === "completed"
                      ? t("antigravity.oauthCompleted")
                      : modalStatus === "cancelled"
                        ? t("antigravity.oauthCancelled")
                        : t("antigravity.oauthFailed")}
              </div>
              <p className="mt-1 text-xs leading-relaxed">
                {modalStatus === "completed"
                  ? status?.email
                    ? t("antigravity.oauthSuccessWithEmail", { email: status.email })
                    : t("antigravity.oauthSuccess")
                  : t("antigravity.oauthStep2Description")}
              </p>
            </div>

            {session.auth_url && modalStatus !== "completed" ? (
              <div className="rounded-xl border border-border px-4 py-3">
                <div className="mb-2 flex items-center justify-between gap-2">
                  <p className="text-xs font-semibold text-muted-foreground">{t("antigravity.oauthOpenLink")}</p>
                  <Button variant="ghost" size="xs" onClick={() => void copyAuthURL()}>
                    {copied ? <Check className="size-3 text-emerald-500" /> : <Copy className="size-3" />}
                    {t("common.copy")}
                  </Button>
                </div>
                <a
                  href={session.auth_url}
                  target="_blank"
                  rel="noopener noreferrer"
                  className="inline-flex items-start gap-1.5 text-sm font-semibold text-primary hover:underline"
                >
                  <ExternalLink className="mt-0.5 size-3.5 shrink-0" />
                  <span className="line-clamp-2 break-all">{session.auth_url}</span>
                </a>
              </div>
            ) : null}

            {modalStatus !== "completed" ? (
              <label className="block space-y-1.5">
                <FieldLabel>{t("antigravity.oauthCallbackLabel")}</FieldLabel>
                <textarea
                  value={callbackURL}
                  onChange={(event) => setCallbackURL(event.target.value)}
                  rows={3}
                  spellCheck={false}
                  placeholder={t("antigravity.oauthCallbackPlaceholder")}
                  className={TEXTAREA_CLASS}
                />
                <FieldHint>{t("antigravity.oauthCallbackHint")}</FieldHint>
              </label>
            ) : null}

            {status?.warning ? (
              <div className="rounded-lg border border-amber-500/30 bg-amber-500/5 px-3 py-2 text-xs text-amber-700 dark:text-amber-300">
                {status.warning}
              </div>
            ) : null}
            {status?.error ? (
              <div className="rounded-lg border border-red-500/30 bg-red-500/5 px-3 py-2 text-xs text-red-700 dark:text-red-300">
                {status.error}
              </div>
            ) : null}
          </>
        )}
      </div>
    </Modal>
  );
}
