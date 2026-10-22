// An Analytics section's label, a rule, and an optional refresh.

import { RefreshCw } from "lucide-react";
import { Button, cx } from "../ui";

export function SectionHeader({
  label,
  onRefresh,
  refreshing,
}: {
  label: string;
  onRefresh?: () => void;
  refreshing?: boolean;
}) {
  return (
    <div className="mb-3 mt-8 flex items-center gap-3 first:mt-0">
      <span className="text-xs font-semibold uppercase tracking-[0.12em] text-muted-foreground">{label}</span>
      <div className="h-px flex-1 bg-border" />
      {onRefresh && (
        <Button size="sm" variant="ghost" onClick={onRefresh} disabled={refreshing}>
          <RefreshCw className={cx("size-3.5", refreshing && "animate-spin")} /> Refresh
        </Button>
      )}
    </div>
  );
}
