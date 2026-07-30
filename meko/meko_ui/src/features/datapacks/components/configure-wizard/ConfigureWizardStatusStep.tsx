/**
 * Finish step: summary tiles (knowledge + coming-soon sync columns). Footer lives in ConfigureWizardUnifiedFooter.
 */
import { ConfigureWizardFinishTiles } from "@/features/datapacks/components/configure-wizard/ConfigureWizardFinishTiles";
import type { ListedSource } from "@/features/datapacks/components/configure-wizard/types";

type Props = {
  knowledgeItems: ListedSource[];
};

export function ConfigureWizardStatusStep({ knowledgeItems }: Props) {
  return (
    <div className="space-y-6">
      <ConfigureWizardFinishTiles knowledgeItems={knowledgeItems} />
    </div>
  );
}
