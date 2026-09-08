// Shown at the top of each of the three "add a source" stages
// (KnowledgeBaseStage, MemoryStage, ConversationStage) so ContextIntroStage's
// three-source framing visibly continues into them -- same icons, same
// order, one highlighted as current and earlier ones checked off -- instead
// of each stage reading as an unrelated new page.

import { Check } from "lucide-react";

import { cn } from "@/lib/utils";
import { CONTEXT_SOURCES, type ContextSourceKey } from "../../data/adlcReviewAgentLab/contextSources";

export function ContextSourceTracker({ current }: { current: ContextSourceKey }) {
  const currentIndex = CONTEXT_SOURCES.findIndex((source) => source.key === current);

  return (
    <div className="flex items-center gap-1.5 rounded-lg border border-border bg-muted/30 px-3 py-2">
      {CONTEXT_SOURCES.map((source, index) => {
        const Icon = source.icon;
        const isDone = index < currentIndex;
        const isCurrent = index === currentIndex;
        return (
          <div key={source.key} className="flex items-center gap-1.5">
            {index > 0 && <span className="h-px w-4 shrink-0 bg-border" aria-hidden />}
            <span
              className={cn(
                "flex items-center gap-1.5 rounded-full px-2.5 py-1 text-xs font-medium",
                isCurrent && "bg-primary/10 text-primary",
                isDone && "text-emerald-700 dark:text-emerald-300",
                !isCurrent && !isDone && "text-muted-foreground",
              )}
            >
              {isDone ? <Check className="h-3.5 w-3.5" aria-hidden /> : <Icon className="h-3.5 w-3.5" aria-hidden />}
              {source.title}
            </span>
          </div>
        );
      })}
    </div>
  );
}
