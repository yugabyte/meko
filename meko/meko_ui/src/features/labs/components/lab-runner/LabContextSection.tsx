// One context source in the runner's right pane — a colour-coded heading
// with a count, then its rows. Shared by all four sections of LabContextPane.

import type { ReactNode } from "react";

import { cn } from "@/lib/utils";

/** Which of Meko's three context sources this section shows. Drives the
 *  colour coding, which is the same everywhere a source appears. */
export type LabContextKind = "knowledge" | "memory" | "conversation";

const KIND_TEXT: Record<LabContextKind, string> = {
  knowledge: "text-context-knowledge",
  memory: "text-context-memory",
  conversation: "text-context-conversation",
};

const KIND_MARKER: Record<LabContextKind, string> = {
  knowledge: "bg-context-knowledge",
  memory: "bg-context-memory",
  conversation: "bg-context-conversation",
};

export function LabContextSection({
  kind,
  label,
  count,
  emptyHint,
  children,
}: {
  kind: LabContextKind;
  label: string;
  count: number;
  /** Shown instead of rows before the user has added anything — says what
   *  will land here, not just "nothing yet". */
  emptyHint: string;
  children: ReactNode;
}) {
  return (
    <section className="flex flex-col gap-1.5">
      <div className="flex items-center justify-between gap-2">
        <h4 className={cn("text-xs font-semibold uppercase tracking-wide", KIND_TEXT[kind])}>{label}</h4>
        <span className="tabular-nums text-xs text-muted-foreground">{count}</span>
      </div>
      {count === 0 ? (
        <p className="rounded-md border border-dashed border-border px-2.5 py-2 text-xs text-muted-foreground">
          {emptyHint}
        </p>
      ) : (
        <div className="flex flex-col gap-1">{children}</div>
      )}
    </section>
  );
}

/** One item inside a section. `kind` repeats the section's colour on a left
 *  marker so a row still reads as knowledge/memory/conversation when the
 *  pane is scrolled past its heading. */
export function LabContextRow({
  kind,
  title,
  meta,
}: {
  kind: LabContextKind;
  title: ReactNode;
  meta?: string;
}) {
  return (
    <div className="flex items-start gap-2 rounded-md bg-card px-2.5 py-2">
      <i aria-hidden className={cn("mt-1.5 h-1.5 w-1.5 shrink-0 rounded-full", KIND_MARKER[kind])} />
      <div className="min-w-0 flex-1">
        <p className="line-clamp-2 text-xs text-foreground">{title}</p>
        {meta && <p className="mt-0.5 truncate text-[11px] text-muted-foreground">{meta}</p>}
      </div>
    </div>
  );
}
