/**
 * Summary strip on the Finish step: datapack name, selection counts, and primary Finish action.
 */
import { Button } from "@/components/ui/button";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";

type Props = {
  datapackName: string;
  knowledgeCount: number;
  conversationsCount: number;
  skillsCount: number;
  hasUploadedKnowledge: boolean;
  onFinish: () => void;
  /** True while createKnowledgeBase / pipeline poll is running from this flow. */
  finishBusy?: boolean;
};

export function ConfigureWizardFooterBar({
  datapackName,
  knowledgeCount,
  conversationsCount,
  skillsCount,
  hasUploadedKnowledge,
  onFinish,
  finishBusy = false,
}: Props) {
  const total = knowledgeCount + conversationsCount + skillsCount;
  const finishDisabled = finishBusy;
  const finishButton = (
    <Button
      type="button"
      disabled={finishDisabled}
      className="bg-emerald-600 text-white hover:bg-emerald-500 disabled:opacity-50"
      onClick={onFinish}
    >
      {finishBusy ? "Finishing…" : "Finish"}
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
        {total === 0 ? (
          <span className="block pt-1 text-muted-foreground/80">
            No files staged — Finish will complete setup without creating a knowledge base.
          </span>
        ) : !hasUploadedKnowledge ? (
          <span className="block pt-1 text-muted-foreground/80">
            Upload files on the previous step, or Finish to skip knowledge base creation.
          </span>
        ) : null}
      </div>
      <div className="flex flex-wrap items-center justify-end gap-3">
        {finishDisabled ? (
          <Tooltip>
            <TooltipTrigger asChild>
              <span className="inline-flex">{finishButton}</span>
            </TooltipTrigger>
            <TooltipContent side="top">
              Import is running. You can keep browsing; we will notify you when it finishes.
            </TooltipContent>
          </Tooltip>
        ) : (
          finishButton
        )}
      </div>
    </div>
  );
}
