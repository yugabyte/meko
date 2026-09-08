// Pure explainer -- orients the user on what "building context" means
// before the three hands-on lessons (Knowledge Base, Memory, Conversation)
// that follow it. No data dependency, so unlike every other stage in this
// chapter it isn't gated behind LabStageNeedsDatapack.

import { BookOpen } from "lucide-react";

import { CONTEXT_SOURCES } from "../../data/adlcReviewAgentLab/contextSources";

export function ContextIntroStage() {
  return (
    <div className="flex flex-1 flex-col gap-6 overflow-y-auto p-4">
      <div className="flex items-center gap-2">
        <BookOpen className="h-5 w-5 text-primary" aria-hidden />
        <h2 className="text-lg font-semibold text-foreground">What is context?</h2>
      </div>
      <p className="text-sm text-muted-foreground">
        Before AgentK can review code against your team's standards, it needs
        context to check against. Meko builds that context from three
        sources — you'll add one of each in the next few steps.
      </p>
      <div className="grid gap-3 sm:grid-cols-3">
        {CONTEXT_SOURCES.map(({ key, icon: Icon, title, body }) => (
          <div key={key} className="rounded-xl border border-border bg-card p-4">
            <Icon className="h-4 w-4 text-primary" aria-hidden />
            <p className="mt-2 text-sm font-semibold text-foreground">{title}</p>
            <p className="mt-1 text-xs text-muted-foreground">{body}</p>
          </div>
        ))}
      </div>
    </div>
  );
}
