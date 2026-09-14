// A large, selectable option card with a radio marker, body copy and an
// optional metric line. Shared by the lab stages that ask the user to pick
// between a few framed choices (retrieval cost, guardrail).

import type { ReactNode } from "react";

import { cn } from "@/lib/utils";

export function LabChoiceCard({
  title,
  body,
  metric,
  badge,
  selected,
  onSelect,
}: {
  title: string;
  body: string;
  /** Optional emphasised figure, e.g. an estimated token cost. */
  metric?: ReactNode;
  /** Optional short qualifier beside the title, e.g. "recommended". */
  badge?: ReactNode;
  selected: boolean;
  onSelect: () => void;
}) {
  return (
    <button
      type="button"
      onClick={onSelect}
      aria-pressed={selected}
      className={cn(
        "flex flex-col gap-2 rounded-lg border p-4 text-left transition-colors",
        selected ? "border-primary bg-primary/5" : "border-border bg-card hover:border-muted-foreground/40",
      )}
    >
      <span className="flex items-center gap-2">
        <span
          aria-hidden
          className={cn(
            "flex h-4 w-4 shrink-0 items-center justify-center rounded-full border",
            selected ? "border-primary" : "border-muted-foreground/50",
          )}
        >
          {selected && <span className="h-2 w-2 rounded-full bg-primary" />}
        </span>
        <span className="text-sm font-semibold text-foreground">{title}</span>
        {badge}
      </span>
      <span className="text-sm text-muted-foreground">{body}</span>
      {metric && <span className="font-mono text-xs text-foreground">{metric}</span>}
    </button>
  );
}
