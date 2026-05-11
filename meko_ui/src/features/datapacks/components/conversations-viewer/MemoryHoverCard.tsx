// Floating popover above the "N memories extracted" badge that lazily
// fetches the trace spans for this message and surfaces the extracted
// memory texts. Hover to peek; click anywhere on the badge or popover to
// open the full trace drawer.
//
// Lazy by design — no network call until the cursor enters the badge.
// `fetchedRef` makes sure repeated hover-in/out cycles only hit the API
// once per mount.

import { useCallback, useRef, useState } from "react";
import { Loader2 } from "lucide-react";
import { cn } from "@/lib/utils";
import { MemoryIcon } from "@/components/conversation-telemetry";
import { getDatapackMessageTrace } from "@/lib/mekoApi";
import { parseMemoryOutput } from "./parseMemoryOutput";

const MEMORY_PREVIEW_CHARS = 120;

type Props = {
  count: number;
  messageId: string;
  datapackName: string;
  datapackId?: string;
  conversationId: string;
  onClickOpenTrace: () => void;
};

export function MemoryHoverCard({
  count,
  messageId,
  datapackName,
  datapackId,
  conversationId,
  onClickOpenTrace,
}: Props) {
  const [open, setOpen] = useState(false);
  const [memories, setMemories] = useState<string[] | null>(null);
  const [fetching, setFetching] = useState(false);
  const fetchedRef = useRef(false);
  const closeTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  const fetchMemories = useCallback(() => {
    if (fetchedRef.current) return;
    fetchedRef.current = true;
    setFetching(true);
    getDatapackMessageTrace(datapackName, conversationId, messageId, {
      datapackId,
    })
      .then((resp) => {
        const spans = resp.spans ?? [];
        // Top-level memory spans only — Mem0 emits nested spans (one per
        // extracted fact) under a parent "memory_add" span, and we want
        // the parent's aggregated output, not each leaf.
        const memoryIds = new Set(
          spans.filter((s) => /memor/i.test(s.name) && s.output).map((s) => s.id),
        );
        const topLevel = spans.filter(
          (s) =>
            /memor/i.test(s.name) &&
            s.output &&
            (!s.parent_observation_id || !memoryIds.has(s.parent_observation_id)),
        );
        setMemories(topLevel.flatMap((s) => parseMemoryOutput(s.output ?? "")));
      })
      .catch(() => setMemories([]))
      .finally(() => setFetching(false));
  }, [datapackName, conversationId, messageId, datapackId]);

  const openPopover = useCallback(() => {
    if (closeTimerRef.current) clearTimeout(closeTimerRef.current);
    setOpen(true);
    fetchMemories();
  }, [fetchMemories]);

  // Short close delay so the cursor can travel from badge → popover
  // without the popover dismissing in between.
  const closePopover = useCallback(() => {
    closeTimerRef.current = setTimeout(() => setOpen(false), 150);
  }, []);

  const label = `${count.toLocaleString()} ${count === 1 ? "memory" : "memories"} extracted`;

  return (
    <span className="relative inline-flex items-center">
      <span
        role="button"
        tabIndex={0}
        className={cn(
          "inline-flex cursor-pointer select-none items-center gap-1 rounded px-1 py-0.5 transition-colors",
          open ? "bg-blue-500/15 text-blue-300" : "hover:bg-blue-500/10 hover:text-blue-300",
        )}
        onMouseEnter={openPopover}
        onMouseLeave={closePopover}
        onClick={onClickOpenTrace}
        onKeyDown={(e) => e.key === "Enter" && onClickOpenTrace()}
      >
        <MemoryIcon className="text-[13px]" />
        {label}
      </span>

      {open ? (
        <div
          className="absolute bottom-full left-0 z-50 mb-2 w-72 overflow-hidden rounded-xl border border-blue-500/25 bg-[#0E1525] shadow-2xl"
          onMouseEnter={openPopover}
          onMouseLeave={closePopover}
          onClick={onClickOpenTrace}
        >
          {/* Caret */}
          <div className="absolute -bottom-1.5 left-4 h-3 w-3 rotate-45 border-b border-r border-blue-500/25 bg-[#0E1525]" />

          <div className="px-3.5 py-3">
            <p className="mb-2.5 flex items-center gap-1.5 text-[11px] font-medium text-blue-300">
              <MemoryIcon className="text-[13px]" />
              Extracted memories
              <span className="ml-auto text-[10px] font-normal text-blue-300/50">
                Click to open trace
              </span>
            </p>

            {fetching ? (
              <div className="flex items-center gap-2 text-[11px] text-muted-foreground">
                <Loader2 className="h-3 w-3 animate-spin" aria-hidden />
                Loading…
              </div>
            ) : !memories || memories.length === 0 ? (
              <p className="text-[11px] italic text-muted-foreground">
                No memory text captured in trace.
              </p>
            ) : (
              <ul className="space-y-2">
                {memories.map((text, i) => (
                  // eslint-disable-next-line react/no-array-index-key
                  <li key={i} className="flex items-start gap-2">
                    <span className="mt-[5px] h-1.5 w-1.5 shrink-0 rounded-full bg-blue-400/70" />
                    <span className="text-[12px] leading-relaxed text-foreground/85">
                      {text.length > MEMORY_PREVIEW_CHARS
                        ? `${text.slice(0, MEMORY_PREVIEW_CHARS)}…`
                        : text}
                    </span>
                  </li>
                ))}
              </ul>
            )}
          </div>
        </div>
      ) : null}
    </span>
  );
}
