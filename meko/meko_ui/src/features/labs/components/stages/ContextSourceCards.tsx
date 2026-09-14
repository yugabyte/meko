// The three sources Meko builds context from, colour-coded the same way the
// runner's right pane codes them. Shown on the create-datapack stage to
// frame the three lessons that follow it.

import { cn } from "@/lib/utils";

import { CONTEXT_SOURCES } from "../../data/adlcReviewAgentLab/contextSources";

/** Keyed by the CONTEXT_SOURCES entry's own `key`, so a source added there
 *  without a colour here falls back to the neutral border rather than
 *  silently borrowing another source's colour. */
const SOURCE_ACCENT: Record<string, string> = {
  "knowledge-base": "text-context-knowledge",
  memory: "text-context-memory",
  conversation: "text-context-conversation",
};

export function ContextSourceCards() {
  return (
    <div className="grid gap-3 sm:grid-cols-3">
      {CONTEXT_SOURCES.map(({ key, icon: Icon, title, body }) => (
        <div key={key} className="rounded-lg border border-border bg-card p-4">
          <Icon className={cn("h-4 w-4", SOURCE_ACCENT[key] ?? "text-muted-foreground")} aria-hidden />
          <p className="mt-2 text-sm font-semibold text-foreground">{title}</p>
          <p className="mt-1 text-xs text-muted-foreground">{body}</p>
        </div>
      ))}
    </div>
  );
}
