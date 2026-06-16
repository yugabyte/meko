// Floating popover above the "N memories extracted" badge that surfaces
// the extracted memory texts. Hover to peek; click anywhere on the badge
// or popover to open the full trace drawer.
//
// The memory texts come straight from the thread message (`memory_texts` /
// `promoted_memory_texts`), which the backend sources from the same DB
// queries as the badge counts — so the preview list and the counts stay in
// lockstep. Surviving memories render normally; promoted ones are listed
// separately under a "Promoted to KB" heading.

import { useCallback, useRef, useState } from "react";
import { BookUp } from "lucide-react";
import { cn } from "@/lib/utils";
import { MemoryIcon } from "@/components/conversation-telemetry";

const MEMORY_PREVIEW_CHARS = 120;

type Props = {
  /** Surviving (non-promoted) memory count + texts. */
  count: number;
  memories: string[];
  /** Promoted-to-KB memory count + texts (extracted here, now in the KB). */
  promotedCount: number;
  promotedMemories: string[];
  onClickOpenTrace: () => void;
};

function preview(text: string): string {
  return text.length > MEMORY_PREVIEW_CHARS ? `${text.slice(0, MEMORY_PREVIEW_CHARS)}…` : text;
}

export function MemoryHoverCard({
  count,
  memories,
  promotedCount,
  promotedMemories,
  onClickOpenTrace,
}: Props) {
  const [open, setOpen] = useState(false);
  const closeTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  const openPopover = useCallback(() => {
    if (closeTimerRef.current) clearTimeout(closeTimerRef.current);
    setOpen(true);
  }, []);

  // Short close delay so the cursor can travel from badge → popover
  // without the popover dismissing in between.
  const closePopover = useCallback(() => {
    closeTimerRef.current = setTimeout(() => setOpen(false), 150);
  }, []);

  const survivingLabel =
    count > 0 ? `${count.toLocaleString()} ${count === 1 ? "memory" : "memories"} extracted` : "";
  const promotedLabel =
    promotedCount > 0
      ? `${promotedCount.toLocaleString()} ${promotedCount === 1 ? "memory" : "memories"} promoted to KB`
      : "";

  return (
    <span className="relative inline-flex items-center">
      <span
        role="button"
        tabIndex={0}
        className={cn(
          "inline-flex cursor-pointer select-none items-center gap-1 rounded px-1 py-0.5 transition-colors",
          open
            ? "bg-blue-500/15 text-blue-700 dark:text-blue-300"
            : "hover:bg-blue-500/10 hover:text-blue-700 dark:text-blue-300",
        )}
        onMouseEnter={openPopover}
        onMouseLeave={closePopover}
        onClick={onClickOpenTrace}
        onKeyDown={(e) => e.key === "Enter" && onClickOpenTrace()}
      >
        {survivingLabel ? <MemoryIcon className="text-[13px]" /> : null}
        {survivingLabel}
        {survivingLabel && promotedLabel ? (
          <span className="text-muted-foreground/60">·</span>
        ) : null}
        {promotedLabel ? (
          <span className="inline-flex items-center gap-1 text-emerald-700 dark:text-emerald-300">
            <BookUp className="h-3 w-3" aria-hidden />
            {promotedLabel}
          </span>
        ) : null}
      </span>

      {open ? (
        <div
          className="absolute bottom-full left-0 z-50 mb-2 w-72 overflow-hidden rounded-xl border border-blue-500/25 bg-input shadow-2xl"
          onMouseEnter={openPopover}
          onMouseLeave={closePopover}
          onClick={onClickOpenTrace}
        >
          {/* Caret */}
          <div className="absolute -bottom-1.5 left-4 h-3 w-3 rotate-45 border-b border-r border-blue-500/25 bg-input" />

          <div className="px-3.5 py-3">
            <p className="mb-2.5 flex items-center gap-1.5 text-[11px] font-medium text-blue-700 dark:text-blue-300">
              <MemoryIcon className="text-[13px]" />
              Extracted memories
              <span className="ml-auto text-[10px] font-normal text-blue-700 dark:text-blue-300/50">
                Click to open trace
              </span>
            </p>

            {memories.length === 0 && promotedMemories.length === 0 ? (
              <p className="text-[11px] italic text-muted-foreground">
                No memory text captured in trace.
              </p>
            ) : (
              <>
                {memories.length > 0 ? (
                  <ul className="space-y-2">
                    {memories.map((text, i) => (
                      // eslint-disable-next-line react/no-array-index-key
                      <li key={i} className="flex items-start gap-2">
                        <span className="mt-[5px] h-1.5 w-1.5 shrink-0 rounded-full bg-blue-400/70" />
                        <span className="text-[12px] leading-relaxed text-foreground/85">
                          {preview(text)}
                        </span>
                      </li>
                    ))}
                  </ul>
                ) : null}

                {promotedMemories.length > 0 ? (
                  <div
                    className={cn(memories.length > 0 && "mt-3 border-t border-border/60 pt-2.5")}
                  >
                    <p className="mb-2 flex items-center gap-1.5 text-[10px] font-semibold uppercase tracking-wider text-emerald-700 dark:text-emerald-300">
                      <BookUp className="h-3 w-3" aria-hidden />
                      Promoted to KB
                    </p>
                    <ul className="space-y-2">
                      {promotedMemories.map((text, i) => (
                        // eslint-disable-next-line react/no-array-index-key
                        <li key={i} className="flex items-start gap-2">
                          <span className="mt-[5px] h-1.5 w-1.5 shrink-0 rounded-full bg-emerald-400/70" />
                          <span className="text-[12px] leading-relaxed text-foreground/85">
                            {preview(text)}
                          </span>
                        </li>
                      ))}
                    </ul>
                  </div>
                ) : null}
              </>
            )}
          </div>
        </div>
      ) : null}
    </span>
  );
}
