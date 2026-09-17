// The three sources Meko builds context from, colour-coded the same way the
// runner's right pane codes them. Each card shows its name alone until the
// reader asks for more, so the step reads as three words rather than three
// paragraphs.

import { useState } from "react";

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
  // Hover and focus both open a card. No onClick: a tap focuses the button,
  // so focus already covers touch — and a click handler fought the pointer,
  // since onMouseEnter had opened the card before the click arrived, so the
  // toggle closed the card under the cursor and it stayed shut until the
  // pointer left and came back. The card is a button so the definition is
  // reachable without a pointer at all.
  const [openKey, setOpenKey] = useState<string | null>(null);

  return (
    <div className="grid gap-3 sm:grid-cols-3">
      {CONTEXT_SOURCES.map(({ key, icon: Icon, title, body }) => {
        const open = openKey === key;
        return (
          <button
            key={key}
            type="button"
            aria-expanded={open}
            onMouseEnter={() => setOpenKey(key)}
            onMouseLeave={() => setOpenKey((k) => (k === key ? null : k))}
            onFocus={() => setOpenKey(key)}
            onBlur={() => setOpenKey((k) => (k === key ? null : k))}
            className={cn(
              "rounded-lg border bg-card p-4 text-left transition-all duration-300 ease-out",
              "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
              "motion-reduce:transition-none",
              open ? "-translate-y-0.5 border-border shadow-sm" : "border-border/60",
            )}
          >
            <Icon
              className={cn(
                "h-4 w-4 transition-transform duration-300 ease-out motion-reduce:transition-none",
                SOURCE_ACCENT[key] ?? "text-muted-foreground",
                open && "scale-110",
              )}
              aria-hidden
            />
            <p className="mt-2 text-sm font-semibold text-foreground">{title}</p>
            {/* 0fr → 1fr is what lets the description animate to its own
                height; a fixed max-height would either clip a long body or
                leave a gap under a short one. */}
            <div
              className={cn(
                "grid transition-[grid-template-rows,opacity] duration-300 ease-out motion-reduce:transition-none",
                open ? "grid-rows-[1fr] opacity-100" : "grid-rows-[0fr] opacity-0",
              )}
            >
              <div className="overflow-hidden">
                <p className="mt-1 text-xs text-muted-foreground">{body}</p>
              </div>
            </div>
          </button>
        );
      })}
    </div>
  );
}
