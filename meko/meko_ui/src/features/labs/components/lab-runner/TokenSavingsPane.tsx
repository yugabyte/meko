// Token Savings: a collapsible wrapper around the same lifetime
// TokenSavingsSummary the datapack detail page's TOKEN BURN tile renders
// (useDatapackTokenSavings) -- reused as-is, no new endpoint. The headline
// percentage stays visible even collapsed; expanding shows the per-source
// breakdown.

import { useState } from "react";
import { ChevronDown, Zap } from "lucide-react";
import { cn } from "@/lib/utils";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { getApiErrorMessage } from "@/lib/api/mekoApi";
import { formatTokens, sourceBreakdown, tokenBurnPercent } from "@/lib/tokenSavings";
import { useDatapackTokenSavings } from "@/features/datapacks/hooks/useTokenSavings";

export function TokenSavingsPane({ datapackId }: { datapackId: string | null }) {
  const [open, setOpen] = useState(false);
  const { data: summary, isLoading, error } = useDatapackTokenSavings(datapackId);

  const hasData = Boolean(summary && summary.total_requests > 0);
  const headline = hasData && summary ? `${tokenBurnPercent(summary)}% saved` : "—";

  return (
    <Collapsible open={open} onOpenChange={setOpen} className="border-t border-border/70">
      <CollapsibleTrigger asChild>
        <button
          type="button"
          className="flex w-full items-center justify-between gap-2 px-4 py-3 text-left"
          aria-expanded={open}
        >
          <span className="flex min-w-0 items-center gap-2">
            <Zap className="h-4 w-4 shrink-0 text-amber-500" aria-hidden />
            <span className="text-sm font-medium text-foreground">Token Savings</span>
          </span>
          <span className="flex shrink-0 items-center gap-2">
            <span className="text-xs text-muted-foreground">
              {isLoading || !datapackId ? "…" : headline}
            </span>
            <ChevronDown
              className={cn("h-4 w-4 text-muted-foreground transition-transform", open && "rotate-180")}
              aria-hidden
            />
          </span>
        </button>
      </CollapsibleTrigger>
      <CollapsibleContent>
        <div className="px-4 pb-3">
          {error ? (
            <p className="text-xs text-destructive">
              {getApiErrorMessage(error, "Couldn't load token savings.")}
            </p>
          ) : !hasData ? (
            <p className="text-xs text-muted-foreground">No contextual searches recorded yet.</p>
          ) : (
            summary && (
              <ul className="space-y-1">
                {sourceBreakdown(summary).map((row) => (
                  <li key={row.key} className="flex items-center justify-between text-xs">
                    <span className="text-muted-foreground">{row.label}</span>
                    <span className="text-foreground">
                      {formatTokens(row.tokens)} tokens · {Math.round(row.fraction * 100)}%
                    </span>
                  </li>
                ))}
              </ul>
            )
          )}
        </div>
      </CollapsibleContent>
    </Collapsible>
  );
}
