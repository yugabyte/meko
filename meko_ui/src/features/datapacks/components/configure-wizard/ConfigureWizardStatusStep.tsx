/**
 * Import step: explains ingest, shows the same summary strip as the former sync footer, and starts KB creation.
 */
import { Database } from "lucide-react";
import { ConfigureWizardFooterBar } from "@/features/datapacks/components/configure-wizard/ConfigureWizardFooterBar";
import { useKbImportJobDetail } from "@/hooks/useKbImportJob";

type Props = {
  datapackName: string;
  knowledgeCount: number;
  conversationsCount: number;
  skillsCount: number;
  canImportKnowledge: boolean;
  onSkip: () => void;
  onStartImport: () => void;
};

export function ConfigureWizardStatusStep({
  datapackName,
  knowledgeCount,
  conversationsCount,
  skillsCount,
  canImportKnowledge,
  onSkip,
  onStartImport,
}: Props) {
  const job = useKbImportJobDetail();
  const importBusy = job.phase === "creating" || job.phase === "polling";

  return (
    <div className="space-y-6">
      <div className="rounded-xl border border-border bg-card/30 p-4 sm:p-6">
        <div className="flex flex-col gap-3 sm:flex-row sm:items-start sm:gap-4">
          <div className="flex h-10 w-10 shrink-0 items-center justify-center rounded-lg bg-emerald-500/15 border border-emerald-500/25">
            <Database className="h-5 w-5 text-emerald-400" aria-hidden />
          </div>
          <div className="min-w-0 space-y-2">
            <h2 className="text-lg font-semibold tracking-tight text-foreground">Import knowledge</h2>
            <p className="text-sm leading-relaxed text-muted-foreground">
              Start indexing the files you uploaded into this datapack&apos;s knowledge base. You can leave this page
              while import runs; progress appears in the bar at the top of the app.
            </p>
          </div>
        </div>
      </div>

      <ConfigureWizardFooterBar
        datapackName={datapackName}
        knowledgeCount={knowledgeCount}
        conversationsCount={conversationsCount}
        skillsCount={skillsCount}
        onSkip={onSkip}
        canImportKnowledge={canImportKnowledge}
        onImportSelected={onStartImport}
        importBusy={importBusy}
      />
    </div>
  );
}
