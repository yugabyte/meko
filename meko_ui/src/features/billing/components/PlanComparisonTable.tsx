import { useState } from "react";
import { ChevronDown, ChevronUp, Layers } from "lucide-react";
import { cn } from "@/lib/utils";
import { Card } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from "@/components/ui/collapsible";
import {
  COMPARE_GROUPS,
  PLANS,
  type PlanId,
} from "@/features/billing/constants/plans";

/**
 * "Compare every detail" section (screenshot 3). Collapsed by default
 * because most users never need it — the per-tier cards above already
 * surface the headline differences.
 */
export type PlanComparisonTableProps = {
  /** Highlights the column for the user's active tier. */
  currentPlanId: PlanId;
};

export function PlanComparisonTable({ currentPlanId }: PlanComparisonTableProps) {
  const [open, setOpen] = useState(false);

  return (
    <Card className="bg-card/60 shadow-none">
      <Collapsible open={open} onOpenChange={setOpen}>
        <CollapsibleTrigger asChild>
          <Button
            type="button"
            variant="ghost"
            className="flex h-auto w-full items-center justify-between gap-3 rounded-xl px-5 py-4 text-left hover:bg-foreground/[0.03]"
            aria-expanded={open}
          >
            <span className="flex items-center gap-2 text-sm font-medium text-foreground">
              <Layers className="h-4 w-4 text-muted-foreground" aria-hidden />
              Compare every detail
              <span className="text-xs font-normal text-muted-foreground">
                · platform features, deployment &amp; support
              </span>
            </span>
            {open ? (
              <ChevronUp className="h-4 w-4 text-muted-foreground" aria-hidden />
            ) : (
              <ChevronDown className="h-4 w-4 text-muted-foreground" aria-hidden />
            )}
          </Button>
        </CollapsibleTrigger>

        <CollapsibleContent>
          <div className="overflow-x-auto border-t border-border/60">
            <table className="w-full text-sm">
              <thead>
                <tr className="border-b border-border/60 text-[11px] font-medium uppercase tracking-wider text-muted-foreground">
                  <th className="px-5 py-2 text-left">Feature</th>
                  {PLANS.map((p) => (
                    <th
                      key={p.id}
                      className={cn(
                        "px-5 py-2 text-left font-medium",
                        p.id === currentPlanId
                          ? "text-purple-700 dark:text-purple-300"
                          : "text-muted-foreground",
                      )}
                    >
                      {p.name}
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {COMPARE_GROUPS.map((group) => (
                  <GroupRows
                    key={group.heading}
                    heading={group.heading}
                    rows={group.rows}
                    currentPlanId={currentPlanId}
                  />
                ))}
              </tbody>
            </table>
          </div>
        </CollapsibleContent>
      </Collapsible>
    </Card>
  );
}

function GroupRows({
  heading,
  rows,
  currentPlanId,
}: {
  heading: string;
  rows: readonly { feature: string; values: Record<PlanId, string> }[];
  currentPlanId: PlanId;
}) {
  return (
    <>
      <tr className="bg-foreground/[0.02]">
        <td
          colSpan={1 + PLANS.length}
          className="px-5 py-1.5 text-[10px] font-semibold uppercase tracking-wider text-muted-foreground"
        >
          {heading}
        </td>
      </tr>
      {rows.map((row) => (
        <tr
          key={row.feature}
          className="border-b border-border/40 last:border-0 hover:bg-foreground/[0.02]"
        >
          <td className="px-5 py-2.5 text-foreground">{row.feature}</td>
          {PLANS.map((p) => {
            const v = row.values[p.id];
            const isEmpty = v === "—";
            return (
              <td
                key={p.id}
                className={cn(
                  "px-5 py-2.5",
                  p.id === currentPlanId && "bg-purple-500/5",
                  isEmpty
                    ? "text-muted-foreground/50"
                    : v.startsWith("✓")
                      ? "text-emerald-600 dark:text-emerald-300"
                      : "text-foreground",
                )}
              >
                {v}
              </td>
            );
          })}
        </tr>
      ))}
    </>
  );
}
