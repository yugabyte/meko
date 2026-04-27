/**
 * Summary strip on Sync Data and Status: datapack name, selection counts, skip, and disabled import.
 * Import stays disabled with a tooltip until backend import exists (“Coming Soon”).
 */
import { Button } from "@/components/ui/button";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";

type Props = {
  datapackName: string;
  knowledgeCount: number;
  conversationsCount: number;
  skillsCount: number;
  onSkip: () => void;
};

export function ConfigureWizardFooterBar({
  datapackName,
  knowledgeCount,
  conversationsCount,
  skillsCount,
  onSkip,
}: Props) {
  const total = knowledgeCount + conversationsCount + skillsCount;
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
        <Tooltip>
          <TooltipTrigger asChild>
            <span className="inline-flex">
              <Button type="button" disabled className="bg-emerald-600 text-white hover:bg-emerald-500">
                Import selected →
              </Button>
            </span>
          </TooltipTrigger>
          <TooltipContent side="top">Coming Soon</TooltipContent>
        </Tooltip>
      </div>
    </div>
  );
}
