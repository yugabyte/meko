// The rule-and-label divider between metrics sections, with an optional
// Refresh on the right. Three call sites shared one copy-pasted block before.

import { RefreshCw } from "lucide-react";

import { cn } from "@/lib/utils";

export function MetricsSectionHeader({
  label,
  id,
  className,
  onRefresh,
  refreshing,
}: {
  label: string;
  id?: string;
  className?: string;
  /** Omit to render the divider with no action on the right. */
  onRefresh?: () => void;
  refreshing?: boolean;
}) {
  return (
    <div id={id} className={cn("mb-4 flex items-center gap-3", className)}>
      <span className="text-xs font-semibold uppercase tracking-widest text-muted-foreground">
        {label}
      </span>
      <div className="h-px flex-1 bg-border" />
      {onRefresh && (
        <button
          type="button"
          onClick={onRefresh}
          disabled={refreshing}
          className="flex items-center gap-1 text-xs text-muted-foreground transition-colors hover:text-foreground disabled:opacity-50"
          title="Refresh metrics"
        >
          <RefreshCw className={cn("h-3.5 w-3.5", refreshing && "animate-spin")} aria-hidden />
          Refresh
        </button>
      )}
    </div>
  );
}
