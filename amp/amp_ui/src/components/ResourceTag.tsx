// Small reusable "value tag" — a compact, monospaced outline badge for
// key/value-ish metadata (compute size, counts, ids). Kept generic so the same
// look is reused wherever we surface these little labelled values.

import type { ReactNode } from "react";
import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";

/** Generic compact value tag. */
export function ValueTag({
  children,
  className,
}: {
  children: ReactNode;
  className?: string;
}) {
  return (
    <Badge variant="outline" className={cn("font-mono text-[10px]", className)}>
      {children}
    </Badge>
  );
}

/** Compute size tag: "<cpu> CPU / <memory>". Renders nothing when neither
 *  value is known. Missing-but-present fields show "?". */
export function ComputeResourceTag({
  cpu,
  memory,
  className,
}: {
  cpu?: string;
  memory?: string;
  className?: string;
}) {
  if (!cpu && !memory) return null;
  return (
    <ValueTag className={className}>
      {cpu ?? "?"} CPU / {memory ?? "?"}
    </ValueTag>
  );
}
