// Inline pill helper for the conversation header strip. Kept local to the
// conversations-viewer rather than promoted to a shared `Badge` because the
// palette here is opinionated (we map our own per-accent colour choices)
// and we want the freedom to evolve it without disturbing every Badge
// caller in the app.

import type { ReactNode } from "react";
import { cn } from "@/lib/utils";

export type ChipAccent = "slate" | "blue" | "emerald" | "amber" | "purple";

const PALETTE: Record<ChipAccent, string> = {
  slate: "bg-card text-foreground/85 border-border",
  blue: "bg-blue-500/10 text-blue-700 dark:text-blue-300 border-blue-500/30",
  emerald: "bg-emerald-500/10 text-emerald-700 dark:text-emerald-300 border-emerald-500/30",
  amber: "bg-amber-500/10 text-amber-700 dark:text-amber-300 border-amber-500/30",
  purple: "bg-purple-500/10 text-purple-700 dark:text-purple-300 border-purple-500/30",
};

type Props = {
  icon?: ReactNode;
  accent: ChipAccent;
  children: ReactNode;
};

export function Chip({ icon, accent, children }: Props) {
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1 rounded-full border px-2 py-0.5 text-[11px]",
        PALETTE[accent],
      )}
    >
      {icon}
      {children}
    </span>
  );
}
