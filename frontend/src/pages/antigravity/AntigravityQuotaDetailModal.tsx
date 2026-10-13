import { useTranslation } from "react-i18next";
import { CheckCircle2, Settings2 } from "lucide-react";
import type { AccountRow } from "../../types";
import Modal from "../../components/Modal";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  accountLabel,
  identityVerified,
  modelQuotaEntries,
  quotaUpdatedAt,
} from "../../lib/antigravityQuota";
import { formatRelativeTime } from "../../utils/time";
import { AntigravityModelQuotaList, AntigravityQuotaOverview } from "./AntigravityQuotaCells";
import {
  AccountAvatar,
  AntigravityAuthKindChip,
  AntigravityPermissionSection,
  AntigravityStateSection,
  AntigravitySyncWarning,
  PanelSection,
  type AntigravityStateProps,
} from "./AntigravityStatePanel";

/**
 * "额度与权限"宽版窗口:配额分组、状态与能力、Code Assist 权限、逐模型额度一屏看全。
 * 与详情抽屉共用同一份管理状态(同一个详情账号),两者之间可以直接切换。
 */
export default function AntigravityQuotaDetailModal({
  account,
  onClose,
  onOpenManage,
  ...stateProps
}: AntigravityStateProps & {
  account: AccountRow | null;
  onClose: () => void;
  onOpenManage: () => void;
}) {
  const { t } = useTranslation();
  const isAPIKey = account?.antigravity_auth_kind === "api_key";
  const modelCount = account ? modelQuotaEntries(account.antigravity_quota).length : 0;
  const updatedAt = account ? quotaUpdatedAt(account.antigravity_quota) : "";

  return (
    <Modal
      show={account !== null}
      title={
        account ? (
          <span className="flex min-w-0 items-center gap-3">
            <AccountAvatar account={account} size={40} />
            <span className="min-w-0">
              <span className="flex min-w-0 items-center gap-1.5">
                <span className="truncate">{accountLabel(account)}</span>
                {identityVerified(account) ? (
                  <CheckCircle2 className="size-4 shrink-0 text-emerald-500" aria-label={t("antigravity.verified")} />
                ) : null}
                <AntigravityAuthKindChip account={account} />
              </span>
              {account.name && account.email ? (
                <span className="mt-0.5 block truncate text-xs font-normal text-muted-foreground">{account.email}</span>
              ) : null}
            </span>
          </span>
        ) : (
          t("antigravity.quotaDetailTitle")
        )
      }
      onClose={onClose}
      contentClassName="sm:max-w-[800px]"
      footer={
        <>
          <Button variant="outline" onClick={onOpenManage}>
            <Settings2 className="size-4" />
            {t("antigravity.openManage")}
          </Button>
          <Button variant="outline" onClick={onClose}>
            {t("common.close")}
          </Button>
        </>
      }
    >
      {account ? (
        <div className="space-y-5">
          <AntigravitySyncWarning account={account} />

          {!isAPIKey ? (
            <PanelSection layout="wide" title={t("antigravity.quotaGroupsTitle")}>
              <div className="rounded-xl border border-border bg-card p-3.5">
                <AntigravityQuotaOverview account={account} />
              </div>
            </PanelSection>
          ) : null}

          {!isAPIKey ? <AntigravityPermissionSection account={account} layout="wide" /> : null}

          <AntigravityStateSection {...stateProps} layout="wide" />

          {!isAPIKey ? (
            <PanelSection
              layout="wide"
              title={t("antigravity.modelQuotaTitle")}
              description={
                updatedAt
                  ? t("antigravity.updatedAt", { time: formatRelativeTime(updatedAt, { variant: "compact" }) })
                  : t("antigravity.quotaNotSynced")
              }
              action={<Badge variant="secondary">{t("antigravity.modelsCount", { count: modelCount })}</Badge>}
            >
              <AntigravityModelQuotaList account={account} variant="wide" />
            </PanelSection>
          ) : null}
        </div>
      ) : null}
    </Modal>
  );
}
