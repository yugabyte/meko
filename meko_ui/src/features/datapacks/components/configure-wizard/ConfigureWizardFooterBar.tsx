/**
 * Summary strip on Sync Knowledge: datapack name, selection counts, skip, and import when uploads exist.
 */
import { Button } from "@/components/ui/button";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";

type Props = {
  datapackName: string;
  knowledgeCount: number;
  conversationsCount: number;
  skillsCount: number;
  onSkip: () => void;
  canImportKnowledge: boolean;
  onImportSelected: () => void;
  /** True while createKnowledgeBase / pipeline poll is running from this flow. */
  importBusy?: boolean;
};

export function ConfigureWizardFooterBar({
  datapackName,
  knowledgeCount,
  conversationsCount,
  skillsCount,
  onSkip,
  canImportKnowledge,
  onImportSelected,
  importBusy = false,
}: Props) {
  const total = knowledgeCount + conversationsCount + skillsCount;
  const importDisabled = !canImportKnowledge || importBusy;
  const importButton = (
    <Button
      type="button"
      disabled={importDisabled}
      className="bg-emerald-600 text-white hover:bg-emerald-500 disabled:opacity-50"
      onClick={onImportSelected}
    >
      {importBusy ? "Importing…" : "Import knowledge →"}
    </Button>
  );

  return (
    <div className="mt-8 flex flex-col gap-4 rounded-xl border border-border bg-card/60 px-4 py-4 shadow-sm sm:flex-row sm:items-center sm:justify-between sm:gap-6 sm:px-6">
      <div className="space-y-1">
        <div className="text-[10px] font-semibold uppercase tracking-wide text-muted-foreground">
          Datapack name
        </div>
        <code className="block truncate text-sm text-foreground">{datapackName}</code>
      </div>
      <div className="min-w-0 flex-1 text-center text-xs text-muted-foreground sm:text-left">
        <span className="text-foreground/90">{knowledgeCount}</span> knowledge file{knowledgeCount === 1 ? "" : "s"}
        {", "}
        <span className="text-foreground/90">{conversationsCount}</span> conversation
        {conversationsCount === 1 ? "" : "s"}
        {", "}
        <span className="text-foreground/90">{skillsCount}</span> skill{skillsCount === 1 ? "" : "s"}
        {total === 0 ? <span className="block pt-1 text-muted-foreground/80">Nothing selected yet.</span> : null}
      </div>
      <div className="flex flex-wrap items-center justify-end gap-3">
        <Button type="button" variant="ghost" className="text-muted-foreground hover:text-foreground" onClick={onSkip}>
          Skip for now
        </Button>
        {!importDisabled ? (
          importButton
        ) : (
          <Tooltip>
            <TooltipTrigger asChild>
              <span className="inline-flex">{importButton}</span>
            </TooltipTrigger>
            <TooltipContent side="top">
              {importBusy
                ? "Import is running. You can keep browsing; we will notify you when it finishes."
                : "Upload knowledge files on the previous step, then return here to import."}
            </TooltipContent>
          </Tooltip>
        )}
      </div>
    </div>
  );
}
