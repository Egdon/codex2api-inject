import { useTranslation } from "react-i18next";
import { Layers } from "lucide-react";
import type { AccountRow } from "../types";

/** Whether the account list should render an Excel Basispoints badge. */
export function accountShowsExcelBpsBadge(account: Pick<AccountRow, "openai_excel_bps_effective">): boolean {
  return Boolean(account.openai_excel_bps_effective);
}

interface ExcelBpsBadgeProps {
  account: Pick<AccountRow, "openai_excel_bps" | "openai_excel_bps_effective">;
  /** Card view uses the compact flag style shared by the other account flags. */
  variant?: "row" | "card";
}

export default function ExcelBpsBadge({ account, variant = "row" }: ExcelBpsBadgeProps) {
  const { t } = useTranslation();
  if (!accountShowsExcelBpsBadge(account)) return null;
  const title = account.openai_excel_bps
    ? t("accounts.excelBpsBadgeForcedTitle")
    : t("accounts.excelBpsBadgeGlobalTitle");
  if (variant === "card") {
    return (
      <span className="codex-account-card__flag" title={title}>
        <Layers className="size-3" />
        BPS
      </span>
    );
  }
  return (
    <span
      title={title}
      className="inline-flex items-center gap-0.5 rounded-md bg-emerald-50 px-1.5 py-0.5 text-[10px] font-medium text-emerald-700 ring-1 ring-inset ring-emerald-600/20 dark:bg-emerald-950 dark:text-emerald-400 dark:ring-emerald-400/20"
    >
      <Layers className="size-2.5" />
      BPS
    </span>
  );
}
