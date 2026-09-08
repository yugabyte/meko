// Sets the premise before the deploy picker: the same PR, reviewed with vs
// without the context Chapter 1 built. A before/after, not a data-flow
// diagram -- this stage's job is showing the payoff of the work already
// done, not explaining context_search/memory_search mechanics (those are
// already covered by Chapter 1's own X-Ray panes once a review runs).

import { Check, Eye, EyeOff, X } from "lucide-react";

export function ReviewContextComparison({ agentDisplayName }: { agentDisplayName: string }) {
  return (
    <div className="rounded-md border border-border p-4">
      <p className="mb-1 text-sm font-medium">What context changes</p>
      <p className="mb-3 text-xs text-muted-foreground">
        The same pull request, reviewed by {agentDisplayName} — before and after the context you just built.
      </p>
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
        <div className="rounded-md border border-border bg-muted/20 p-3">
          <div className="mb-2 flex items-center gap-1.5 text-xs font-medium text-muted-foreground">
            <EyeOff className="h-3.5 w-3.5" aria-hidden />
            Without context
          </div>
          <p className="mb-2 rounded-md bg-background/60 px-2.5 py-2 text-xs text-muted-foreground">
            Reviewing pull request…
          </p>
          <div className="border-l-2 border-red-400/60 pl-2.5">
            <p className="text-xs text-foreground">"Looks good to me."</p>
            <p className="mt-1 flex items-center gap-1 text-[11px] text-red-600 dark:text-red-400">
              <X className="h-3 w-3" aria-hidden />
              Missed the raw SQL string concat
            </p>
          </div>
        </div>
        <div className="rounded-md border border-primary/40 bg-muted/20 p-3">
          <div className="mb-2 flex items-center gap-1.5 text-xs font-medium text-primary">
            <Eye className="h-3.5 w-3.5" aria-hidden />
            With context
          </div>
          <p className="mb-2 rounded-md bg-background/60 px-2.5 py-2 text-xs text-muted-foreground">
            Reviewing pull request…
          </p>
          <div className="border-l-2 border-emerald-400/60 pl-2.5">
            <p className="text-xs text-foreground">
              "Line 42 builds SQL by string concat — your standards require parameterized queries."
            </p>
            <p className="mt-1 flex items-center gap-1 text-[11px] text-emerald-700 dark:text-emerald-400">
              <Check className="h-3 w-3" aria-hidden />
              Caught it, cites your knowledge base
            </p>
          </div>
        </div>
      </div>
    </div>
  );
}
