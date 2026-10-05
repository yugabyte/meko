// Single-line text that shows a tooltip with the full value, but only when it
// is actually cut off — a short name gets no tooltip at all.
// Shared: any list, rail or header rendering user-supplied names in a fixed width.

import { useCallback, useEffect, useRef } from "react";
import { useState } from "react";
import { Tooltip, TooltipContent, TooltipTrigger, TooltipProvider } from "@/components/ui/tooltip";
import { cn } from "@/lib/utils";

export function TruncatedText({
  text,
  className,
  side = "right",
}: {
  text: string;
  className?: string;
  side?: "top" | "right" | "bottom" | "left";
}) {
  const [truncated, setTruncated] = useState(false);
  const elRef = useRef<HTMLSpanElement | null>(null);
  const roRef = useRef<ResizeObserver | null>(null);

  // A callback ref rather than useRef + useEffect, because the root element
  // CHANGES when `truncated` flips: a bare <span> becomes a Tooltip-wrapped
  // one, so React mounts a new node. An effect keyed on [text] would keep
  // observing the detached old span and never fire again — the rail widens,
  // the name fits, and the tooltip stays. Re-attaching on every node swap is
  // what keeps the measurement live.
  const attach = useCallback((el: HTMLSpanElement | null) => {
    roRef.current?.disconnect();
    roRef.current = null;
    elRef.current = el;
    if (!el) return;

    // scrollWidth exceeds clientWidth exactly when the ellipsis is showing.
    const measure = () => setTruncated(el.scrollWidth > el.clientWidth);
    measure();
    if (typeof ResizeObserver === "undefined") return;
    const ro = new ResizeObserver(measure);
    ro.observe(el);
    roRef.current = ro;
  }, []);

  // The node survives a text change, so the observer never re-attaches;
  // re-measure explicitly.
  useEffect(() => {
    const el = elRef.current;
    if (el) setTruncated(el.scrollWidth > el.clientWidth);
  }, [text]);

  useEffect(() => () => roRef.current?.disconnect(), []);

  const span = (
    <span ref={attach} className={cn("block truncate", className)}>
      {text}
    </span>
  );

  if (!truncated) return span;

  return (
    <TooltipProvider delayDuration={200}>
      <Tooltip>
        <TooltipTrigger asChild>{span}</TooltipTrigger>
        <TooltipContent side={side} className="max-w-xs break-words">
          {text}
        </TooltipContent>
      </Tooltip>
    </TooltipProvider>
  );
}
