// The violet "Shared by <owner>" strip on a card for something another account
// shared with you. Used by the datapack tile and the project card.

import { Share2 } from "lucide-react";

/** Owner label for a strip: email when known, else display name, else "—". */
export function sharedByLabel(email?: string, name?: string): string {
  return (email ?? "").trim() || (name ?? "").trim() || "—";
}

export function SharedByStrip({ label }: { label: string }) {
  return (
    // Subtle in light, saturated in dark: a soft violet pill with darker text
    // in light mode, the deep brand tint against the dark card background.
    <div className="flex items-center justify-between gap-2 rounded-lg border border-violet-500/30 bg-violet-100 dark:bg-violet-950/35 px-3 py-2">
      <div className="flex min-w-0 items-center gap-2 text-xs text-violet-700 dark:text-violet-100/90">
        <Share2
          className="h-3.5 w-3.5 shrink-0 text-violet-700 dark:text-violet-300/90"
          aria-hidden
        />
        <span className="min-w-0 leading-snug">
          <span className="text-violet-700 dark:text-violet-200/80">Shared by </span>
          <span
            className="font-medium text-fuchsia-700 dark:text-fuchsia-300/95 [overflow-wrap:anywhere]"
            title={label !== "—" ? label : undefined}
          >
            {label}
          </span>
        </span>
      </div>
    </div>
  );
}
