/**
 * Finish step: summary tiles (knowledge + coming-soon sync columns) and footer with Finish.
 */
import { ConfigureWizardFinishTiles } from "@/features/datapacks/components/configure-wizard/ConfigureWizardFinishTiles";
import { ConfigureWizardFooterBar } from "@/features/datapacks/components/configure-wizard/ConfigureWizardFooterBar";
import type { ListedSource } from "@/features/datapacks/components/configure-wizard/types";
import { useKbImportJobDetail } from "@/hooks/useKbImportJob";

type Props = {
  datapackName: string;
  knowledgeItems: ListedSource[];
  knowledgeCount: number;
  conversationsCount: number;
  skillsCount: number;
  hasUploadedKnowledge: boolean;
  onFinish: () => void;
};

export function ConfigureWizardStatusStep({
  datapackName,
  knowledgeItems,
  knowledgeCount,
  conversationsCount,
  skillsCount,
  hasUploadedKnowledge,
  onFinish,
}: Props) {
  const job = useKbImportJobDetail();
  const finishBusy = job.phase === "creating" || job.phase === "polling";

  return (
    <div className="space-y-6">
      <ConfigureWizardFinishTiles knowledgeItems={knowledgeItems} />

      <ConfigureWizardFooterBar
        datapackName={datapackName}
        knowledgeCount={knowledgeCount}
        conversationsCount={conversationsCount}
        skillsCount={skillsCount}
        hasUploadedKnowledge={hasUploadedKnowledge}
        onFinish={onFinish}
        finishBusy={finishBusy}
      />
    </div>
  );
}
