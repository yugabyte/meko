// One quoted thing a review read: a soft-tinted pill naming the source kind,
// then where it came from and what it said. Colour-coded the way the runner's
// context pane codes knowledge and memory, so a row means the same thing
// wherever it appears.

import { Badge } from "@/components/ui/badge";

export function ContextCitation({
  kind,
  source,
  quote,
}: {
  kind: "knowledge" | "memory";
  source: string;
  quote: string;
}) {
  const tint =
    kind === "knowledge"
      ? "border-context-knowledge/25 bg-context-knowledge-soft text-context-knowledge"
      : "border-context-memory/25 bg-context-memory-soft text-context-memory";
  const name = kind === "knowledge" ? "text-context-knowledge" : "text-context-memory";

  return (
    <div className="flex items-start gap-3 rounded-md border border-border bg-card p-3">
      <Badge variant="outline" className={`shrink-0 border ${tint}`}>
        {kind}
      </Badge>
      <div className="min-w-0 flex-1">
        <p className={`min-w-0 truncate font-mono text-xs ${name}`}>{source}</p>
        <p className="mt-1 text-xs leading-relaxed text-foreground">“{quote}”</p>
      </div>
    </div>
  );
}
