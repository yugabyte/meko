// X-Ray: the MCP tool calls (memory_add, context_search, etc.) the most
// recently completed lesson made, with latency -- or the equivalent API
// calls for the one lesson (Knowledge Base) that isn't MCP-based. Collapsed
// by default. The trace is persisted per lab (see useLabRunnerSession), so
// it survives a reload rather than emptying out under a user who can still
// see that lesson's results in the pane above.

import { useState } from "react";
import { ChevronDown, ScanLine } from "lucide-react";
import { cn } from "@/lib/utils";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";

import type { LessonTrace, XRayCallStatus } from "../../lib/lessonTrace";

const STATUS_TONE: Record<XRayCallStatus, string> = {
  ok: "bg-emerald-500",
  error: "bg-red-500",
  skipped: "bg-muted-foreground/50",
};

export function XRayPane({ trace }: { trace: LessonTrace | null }) {
  const [open, setOpen] = useState(false);

  return (
    <Collapsible open={open} onOpenChange={setOpen} className="border-t border-border/70">
      <CollapsibleTrigger asChild>
        <button
          type="button"
          className="flex w-full items-center justify-between gap-2 px-4 py-3 text-left"
          aria-expanded={open}
        >
          <span className="flex min-w-0 items-center gap-2">
            <ScanLine className="h-4 w-4 shrink-0 text-muted-foreground" aria-hidden />
            <span className="text-sm font-medium text-foreground">X-Ray</span>
            {trace && (
              <span className="truncate text-xs text-muted-foreground">{trace.lessonTitle}</span>
            )}
          </span>
          <ChevronDown
            className={cn(
              "h-4 w-4 shrink-0 text-muted-foreground transition-transform",
              open && "rotate-180",
            )}
            aria-hidden
          />
        </button>
      </CollapsibleTrigger>
      <CollapsibleContent>
        <div className="px-4 pb-3">
          {!trace ? (
            <p className="text-xs text-muted-foreground">
              Nothing recorded in this browser yet. The calls a lesson makes appear here as you
              complete it.
            </p>
          ) : (
            <ul className="max-h-[300px] space-y-1.5 overflow-y-auto">
              {trace.calls.map((call, i) => (
                <li
                  key={`${call.name}-${i}`}
                  className="flex items-start justify-between gap-2 rounded-md border border-border/60 bg-muted/30 px-2.5 py-1.5"
                >
                  <div className="min-w-0">
                    <div className="flex items-center gap-1.5">
                      <span
                        className={cn(
                          "h-1.5 w-1.5 shrink-0 rounded-full",
                          STATUS_TONE[call.status],
                        )}
                        aria-hidden
                      />
                      <code className="truncate text-xs font-medium text-foreground">
                        {call.name}
                      </code>
                    </div>
                    {call.detail && (
                      <p className="mt-0.5 truncate text-[11px] text-muted-foreground">
                        {call.detail}
                      </p>
                    )}
                  </div>
                  <span className="shrink-0 text-[11px] text-muted-foreground">
                    {call.durationMs}ms
                  </span>
                </li>
              ))}
            </ul>
          )}
        </div>
      </CollapsibleContent>
    </Collapsible>
  );
}
