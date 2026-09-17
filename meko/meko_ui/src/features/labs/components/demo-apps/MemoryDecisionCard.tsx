// One extracted memory, with the two decisions a lesson can ask about it.
// Shared: Chapter 1 asks keep-or-discard, Chapter 4 asks promote-or-keep, and
// both render the same memory through this.

import { type ReactNode } from "react";
import { Check, Loader2 } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";

/** "unavailable": the row cannot take the primary action at all (e.g. a
 *  memory the listing returned without an id, which the promote API needs). */
export type MemoryDecisionState = "idle" | "working" | "done" | "unavailable";

export function MemoryDecisionCard({
  text,
  badges,
  caption,
  trace,
  primaryLabel,
  primaryDoneLabel,
  secondaryLabel,
  state,
  declined = false,
  outcome,
  onPrimary,
  onSecondary,
}: {
  text: string;
  /** Small labels above the memory, e.g. "memory extracted" or the agent
   *  that proposed it. The first is accented; the rest read as context. */
  badges: string[];
  /** One line under the badges saying where the memory came from. */
  caption?: string;
  /** How the agent got here, when the lesson is about the derivation rather
   *  than the memory itself. */
  trace?: string;
  primaryLabel: string;
  /** Replaces the primary button once the decision is made — the card stays
   *  on screen, so the label has to say what happened. */
  primaryDoneLabel: string;
  secondaryLabel: string;
  state: MemoryDecisionState;
  /** The secondary answer was taken. Settles the card without touching the
   *  primary button's label -- a declined memory must never read as though
   *  the primary action ran. */
  declined?: boolean;
  /** Shown under the card once either decision lands. */
  outcome?: ReactNode;
  onPrimary: () => void;
  onSecondary: () => void;
}) {
  const done = state === "done";
  const settled = done || declined;
  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-col gap-3 rounded-lg border border-border bg-card p-4">
        <div className="flex flex-wrap items-center gap-2">
          {badges.map((label, i) => (
            <Badge key={label} variant={i === 0 ? "brand" : "secondary"} className="text-[10px]">
              {label}
            </Badge>
          ))}
          {caption && <span className="text-xs text-muted-foreground">{caption}</span>}
        </div>

        <p className="text-sm font-medium text-foreground">{text}</p>

        {trace && (
          <p className="rounded-md border border-border/60 bg-background/40 p-2 text-xs text-muted-foreground">
            <span className="mr-2 font-mono text-[10px] uppercase tracking-wider">trace</span>
            {trace}
          </p>
        )}

        <div className="flex flex-wrap items-center gap-2">
          <Button
            size="sm"
            variant={declined ? "outline" : "default"}
            onClick={onPrimary}
            disabled={state !== "idle" || declined}
            title={state === "unavailable" ? "This memory has no id to promote." : undefined}
          >
            {state === "working" && <Loader2 className="h-3.5 w-3.5 animate-spin" aria-hidden />}
            {done && <Check className="h-3.5 w-3.5" aria-hidden />}
            {done ? primaryDoneLabel : primaryLabel}
          </Button>
          <Button
            size="sm"
            variant="ghost"
            onClick={onSecondary}
            disabled={state !== "idle" || declined}
          >
            {declined && <Check className="h-3.5 w-3.5" aria-hidden />}
            {secondaryLabel}
          </Button>
        </div>
      </div>

      {settled && outcome}
    </div>
  );
}
