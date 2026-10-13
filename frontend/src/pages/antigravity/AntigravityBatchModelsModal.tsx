import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { Layers, Loader2 } from "lucide-react";
import { api } from "../../api";
import Modal from "../../components/Modal";
import { Button } from "@/components/ui/button";
import { useToast } from "../../hooks/useToast";
import { ANTIGRAVITY_DEFAULT_MODELS } from "../../lib/antigravityModels";
import { parseModelList } from "../../lib/antigravityQuota";
import { getErrorMessage } from "../../utils/error";
import { DefaultModelChips, FieldHint, FieldLabel, TEXTAREA_CLASS } from "./AntigravityFormFields";

/** 批量覆盖 API Key 账号的模型列表(OAuth 账号的目录来自 Google 控制面,不在此修改)。 */
export default function AntigravityBatchModelsModal({
  ids,
  skipped,
  show,
  onClose,
  onSaved,
}: {
  ids: number[];
  skipped: number;
  show: boolean;
  onClose: () => void;
  onSaved: () => Promise<void> | void;
}) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const [models, setModels] = useState(ANTIGRAVITY_DEFAULT_MODELS.join("\n"));
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    if (show) setModels(ANTIGRAVITY_DEFAULT_MODELS.join("\n"));
  }, [show]);

  const parsed = parseModelList(models);

  const handleSave = async () => {
    if (saving || ids.length === 0 || parsed.length === 0) return;
    setSaving(true);
    try {
      const result = await api.batchUpdateAntigravityModels({ ids, models: parsed });
      showToast(
        t("antigravity.batchModelsDone", { success: result.success ?? 0, fail: result.failed ?? 0 }),
        (result.failed ?? 0) > 0 ? "warning" : "success",
      );
      onClose();
      await onSaved();
    } catch (saveError) {
      showToast(t("antigravity.batchModelsFailed", { error: getErrorMessage(saveError) }), "error");
    } finally {
      setSaving(false);
    }
  };

  return (
    <Modal
      show={show}
      title={t("antigravity.batchModelsTitle")}
      onClose={() => {
        if (!saving) onClose();
      }}
      contentClassName="sm:max-w-[560px]"
      footer={
        <>
          <Button variant="outline" onClick={onClose} disabled={saving}>
            {t("common.cancel")}
          </Button>
          <Button onClick={() => void handleSave()} disabled={saving || ids.length === 0 || parsed.length === 0}>
            {saving ? <Loader2 className="animate-spin" /> : <Layers />}
            {t("antigravity.batchModelsApply", { count: ids.length })}
          </Button>
        </>
      }
    >
      <div className="space-y-3">
        <p className="text-xs leading-relaxed text-muted-foreground">
          {t("antigravity.batchModelsDescription", { count: ids.length })}
        </p>
        {skipped > 0 ? (
          <div className="rounded-lg border border-amber-500/30 bg-amber-500/5 px-3 py-2 text-xs text-amber-700 dark:text-amber-300">
            {t("antigravity.batchModelsSkipped", { count: skipped })}
          </div>
        ) : null}
        <label className="block space-y-1.5">
          <FieldLabel>
            {t("antigravity.models")}
            <span className="ml-1.5 font-normal text-muted-foreground/80">
              {t("antigravity.modelsCount", { count: parsed.length })}
            </span>
          </FieldLabel>
          <textarea
            value={models}
            onChange={(event) => setModels(event.target.value)}
            rows={6}
            spellCheck={false}
            placeholder={t("antigravity.modelsPlaceholder")}
            className={TEXTAREA_CLASS}
          />
        </label>
        <DefaultModelChips value={models} onChange={setModels} disabled={saving} />
        <FieldHint>{t("antigravity.batchModelsEmptyHint")}</FieldHint>
      </div>
    </Modal>
  );
}
