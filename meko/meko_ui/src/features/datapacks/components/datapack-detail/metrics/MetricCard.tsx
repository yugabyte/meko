// The shell every metrics card sits in, and the reason cards in a row line up:
// a three-band column — fixed-height header, body that grows, footer pinned to
// the bottom. Cards in a row only share a baseline while all three use it.

import type { ReactNode } from "react";

import { cn } from "@/lib/utils";
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty";

/** Two lines of heading in every card, so a one-line subtitle and a wrapped
 *  two-line one still leave the body starting at the same y. */
const HEADER_MIN_H = "min-h-[2.75rem]";

export function MetricCard({
  title,
  subtitle,
  id,
  children,
  footer,
  empty,
  className,
}: {
  title: ReactNode;
  subtitle?: ReactNode;
  id?: string;
  children: ReactNode;
  /** Pinned to the bottom edge, level with every sibling's footer. */
  footer?: ReactNode;
  /** When set, the card shows this instead of its body and footer. Routed
   *  through the shared `Empty` primitive rather than each card inventing a
   *  line of muted text (or, worse, a status banner in a tone that means
   *  nothing — "no data yet" is not a warning). */
  empty?: { title: string; description?: string };
  className?: string;
}) {
  return (
    <div
      id={id}
      className={cn("flex flex-col gap-4 rounded-xl border border-border bg-card p-5", className)}
    >
      <div className={HEADER_MIN_H}>
        <p className="text-lg font-bold leading-tight text-foreground">{title}</p>
        {subtitle && <p className="mt-0.5 text-xs text-muted-foreground">{subtitle}</p>}
      </div>
      {empty ? (
        // Tighter than Empty's own page-sized padding: this sits inside a
        // card in a three-up row, not on a blank page.
        <Empty className="gap-2 p-4 md:p-6">
          <EmptyHeader className="gap-1">
            <EmptyTitle className="text-sm">{empty.title}</EmptyTitle>
            {empty.description && (
              <EmptyDescription className="text-xs">{empty.description}</EmptyDescription>
            )}
          </EmptyHeader>
        </Empty>
      ) : (
        <>
          <div className="flex flex-1 flex-col gap-4">{children}</div>
          {footer && <div className="mt-auto">{footer}</div>}
        </>
      )}
    </div>
  );
}

/** The "big number over a caption" readout, repeated across four cards.
 *  `tabular-nums` so a changing value doesn't reflow the caption under it. */
export function MetricStat({
  value,
  label,
  size = "lg",
}: {
  value: ReactNode;
  label: string;
  size?: "lg" | "sm";
}) {
  return (
    <div>
      <p
        className={cn(
          "font-bold tabular-nums text-foreground",
          size === "lg" ? "text-3xl" : "text-2xl",
        )}
      >
        {value}
      </p>
      <p className="mt-0.5 text-xs text-muted-foreground">{label}</p>
    </div>
  );
}

/** Two or three MetricStats laid out in a row, callers interleaving
 *  MetricStatDivider where they want a rule. */
export function MetricStatRow({ children }: { children: ReactNode }) {
  return <div className="flex gap-6">{children}</div>;
}

/** The vertical hairline between two stats. Its own component so callers
 *  don't repeat a bare `<div className="w-px bg-border" />`. */
export function MetricStatDivider() {
  return <div className="w-px self-stretch bg-border" aria-hidden />;
}
