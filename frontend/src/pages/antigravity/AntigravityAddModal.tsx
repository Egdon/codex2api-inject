import { useCallback, useState } from "react";
import { useTranslation } from "react-i18next";
import { CheckCircle2, CircleAlert, FileJson, Info, KeyRound, Loader2, Plus } from "lucide-react";
import { api } from "../../api";
import type { ProxyRow } from "../../api";
import type { AccountGroup, AntigravityAuthKind } from "../../types";
import Modal from "../../components/Modal";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";
import { useToast } from "../../hooks/useToast";
import { ANTIGRAVITY_DEFAULT_MODELS } from "../../lib/antigravityModels";
import { detectCredentialInput, parseModelList } from "../../lib/antigravityQuota";
import { getErrorMessage } from "../../utils/error";
import {
  AccountMetadataFields,
  APIKeyModelFields,
  ChoiceCards,
  FieldHint,
  FieldLabel,
  OptionalSettingsSection,
  SecretInput,
  TEXTAREA_CLASS,
} from "./AntigravityFormFields";

interface AddDraft {
  name: string;
  authKind: AntigravityAuthKind;
  authJson: string;
  apiKey: string;
  models: string;
  modelMapping: string;
  proxyUrl: string;
  groupIds: number[];
}

const EMPTY_ADD_DRAFT: AddDraft = {
  name: "",
  authKind: "oauth",
  authJson: "",
  apiKey: "",
  models: ANTIGRAVITY_DEFAULT_MODELS.join("\n"),
  modelMapping: "",
  proxyUrl: "",
  groupIds: [],
};

function CredentialDetection({ value }: { value: string }) {
  const { t } = useTranslation();
  const kind = detectCredentialInput(value);
  if (kind === "empty") return null;
  const config = {
    json: { label: t("antigravity.detectJson"), tone: "text-emerald-700 dark:text-emerald-300", icon: CheckCircle2 },
    refresh_token: { label: t("antigravity.detectRefreshToken"), tone: "text-emerald-700 dark:text-emerald-300", icon: CheckCircle2 },
    json_invalid: { label: t("antigravity.detectJsonInvalid"), tone: "text-red-600 dark:text-red-400", icon: CircleAlert },
    unknown: { label: t("antigravity.detectUnknown"), tone: "text-muted-foreground", icon: Info },
  }[kind];
  const Icon = config.icon;
  return (
    <span className={cn("inline-flex items-center gap-1 text-[11px] font-medium", config.tone)}>
      <Icon className="size-3" aria-hidden />
      {config.label}
    </span>
  );
}

/** 添加单个 Antigravity 账号:粘贴 OAuth 凭据(JSON 或单个 refresh token)或 API Key。批量导入走单独入口。 */
export default function AntigravityAddModal({
  show,
  proxies,
  groups,
  onCreateGroup,
  onClose,
  onAdded,
}: {
  show: boolean;
  proxies: ProxyRow[];
  groups: AccountGroup[];
  onCreateGroup: (name: string) => Promise<number | null>;
  onClose: () => void;
  onAdded: () => Promise<void> | void;
}) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const [draft, setDraft] = useState<AddDraft>(EMPTY_ADD_DRAFT);
  const [submitting, setSubmitting] = useState(false);

  const close = useCallback(() => {
    if (submitting) return;
    onClose();
    setDraft(EMPTY_ADD_DRAFT);
  }, [onClose, submitting]);

  const credentialKind = detectCredentialInput(draft.authJson);
  const canSubmit =
    draft.authKind === "oauth"
      ? credentialKind !== "empty" && credentialKind !== "json_invalid"
      : Boolean(draft.apiKey.trim());

  const handleSubmit = async () => {
    if (submitting) return;
    if (draft.authKind === "oauth" && !draft.authJson.trim()) {
      showToast(t("antigravity.authJsonRequired"), "error");
      return;
    }
    if (draft.authKind === "api_key" && !draft.apiKey.trim()) {
      showToast(t("antigravity.apiKeyRequired"), "error");
      return;
    }
    setSubmitting(true);
    try {
      const response = await api.addAntigravityAccount({
        name: draft.name.trim() || undefined,
        auth_kind: draft.authKind,
        auth_json: draft.authKind === "oauth" ? draft.authJson.trim() : undefined,
        api_key: draft.authKind === "api_key" ? draft.apiKey.trim() : undefined,
        models: draft.authKind === "api_key" ? parseModelList(draft.models) : undefined,
        model_mapping: draft.authKind === "api_key" ? draft.modelMapping.trim() : undefined,
        proxy_url: draft.proxyUrl.trim() || undefined,
        group_ids: draft.groupIds,
      });
      const warning = response.warning?.trim();
      showToast(
        warning || (response.synced ? t("antigravity.addSuccess") : t("antigravity.addDegraded")),
        warning || !response.synced ? "warning" : "success",
      );
      onClose();
      setDraft(EMPTY_ADD_DRAFT);
      await onAdded();
    } catch (addError) {
      showToast(getErrorMessage(addError), "error");
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Modal
      show={show}
      title={t("antigravity.addTitle")}
      onClose={close}
      contentClassName="sm:max-w-[620px]"
      footer={
        <>
          <Button variant="outline" onClick={close} disabled={submitting}>
            {t("common.cancel")}
          </Button>
          <Button onClick={() => void handleSubmit()} disabled={submitting || !canSubmit}>
            {submitting ? <Loader2 className="animate-spin" /> : <Plus />}
            {submitting ? t("antigravity.adding") : t("antigravity.addAccount")}
          </Button>
        </>
      }
    >
      <div className="space-y-4">
        <div className="space-y-1.5">
          <FieldLabel>{t("antigravity.authKind")}</FieldLabel>
          <ChoiceCards<AntigravityAuthKind>
            value={draft.authKind}
            onChange={(authKind) => setDraft((current) => ({ ...current, authKind }))}
            options={[
              {
                value: "oauth",
                title: t("antigravity.credentialTypeOAuth"),
                description: t("antigravity.credentialTypeOAuthDesc"),
                icon: <FileJson className="size-4" />,
              },
              {
                value: "api_key",
                title: t("antigravity.authKindApiKey"),
                description: t("antigravity.credentialTypeApiKeyDesc"),
                icon: <KeyRound className="size-4" />,
                badge: t("antigravity.experimental"),
              },
            ]}
          />
        </div>

        <label className="block space-y-1.5">
          <FieldLabel>{t("antigravity.accountName")}</FieldLabel>
          <Input
            value={draft.name}
            onChange={(event) => setDraft((current) => ({ ...current, name: event.target.value }))}
            placeholder={t("antigravity.accountNamePlaceholder")}
          />
        </label>

        {draft.authKind === "oauth" ? (
          <label className="block space-y-1.5">
            <span className="flex items-center justify-between gap-2">
              <FieldLabel>{t("antigravity.authJson")}</FieldLabel>
              <CredentialDetection value={draft.authJson} />
            </span>
            <textarea
              value={draft.authJson}
              onChange={(event) => setDraft((current) => ({ ...current, authJson: event.target.value }))}
              rows={7}
              spellCheck={false}
              placeholder={t("antigravity.authJsonPlaceholder")}
              className={cn(
                TEXTAREA_CLASS,
                credentialKind === "json_invalid" && "border-red-500/60 focus-visible:border-red-500 focus-visible:ring-red-500/20",
              )}
            />
            <FieldHint>{t("antigravity.authJsonHint")}</FieldHint>
          </label>
        ) : (
          <div className="space-y-3">
            <label className="block space-y-1.5">
              <FieldLabel>{t("antigravity.apiKey")}</FieldLabel>
              <SecretInput
                value={draft.apiKey}
                onChange={(apiKey) => setDraft((current) => ({ ...current, apiKey }))}
                placeholder={t("antigravity.apiKeyPlaceholder")}
              />
              <FieldHint>{t("antigravity.apiKeyHint")}</FieldHint>
            </label>
            <APIKeyModelFields
              models={draft.models}
              modelMapping={draft.modelMapping}
              onModelsChange={(models) => setDraft((current) => ({ ...current, models }))}
              onModelMappingChange={(modelMapping) => setDraft((current) => ({ ...current, modelMapping }))}
            />
          </div>
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
      </div>
    </Modal>
  );
}
