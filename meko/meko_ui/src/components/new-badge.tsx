// The "new" pill, and NewBadge: that pill shown only until a set number of days
// after a feature's launch date, so it drops off without a code change.

import { Sparkles } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";

/** How long a feature counts as new after its launch date. */
export const NEW_BADGE_DAYS = 20;

const DAY_MS = 24 * 60 * 60 * 1000;

/** True from `since` until `days` days after it. An unparseable date is never new. */
export function isNewSince(
  since: string,
  now: Date = new Date(),
  days: number = NEW_BADGE_DAYS,
): boolean {
  const start = Date.parse(since);
  if (Number.isNaN(start)) return false;
  const t = now.getTime();
  return t >= start && t < start + days * DAY_MS;
}

export function NewPill({ className }: { className?: string }) {
  return (
    <Badge
      variant="brand"
      className={cn("gap-1 rounded-full px-2 py-0.5 text-[11px] tracking-wide", className)}
    >
      <Sparkles className="size-3 shrink-0" aria-hidden />
      new
    </Badge>
  );
}

/** `since` is the launch date, e.g. "2026-10-09"; it defaults to today.
 *  Marks a new FEATURE, so it is filled rather than outlined like NewPill
 *  (a new datapack): it has to stand out next to a nav label. */
export function NewBadge({
  since,
  days = NEW_BADGE_DAYS,
  className,
}: {
  since?: string;
  days?: number;
  className?: string;
}) {
  const now = new Date();
  if (!isNewSince(since ?? now.toISOString(), now, days)) return null;
  return (
    <span
      className={cn(
        "inline-flex shrink-0 select-none items-center gap-0.5 rounded-full px-1.5 py-[3px]",
        "bg-brand text-brand-foreground",
        "text-[9px] font-bold uppercase leading-none tracking-[0.08em]",
        "shadow-[0_0_0_1px_hsl(var(--brand)/0.35),0_2px_10px_-2px_hsl(var(--brand)/0.65)]",
        className,
      )}
    >
      <Sparkles className="size-2.5 shrink-0" aria-hidden />
      New
    </span>
  );
}
