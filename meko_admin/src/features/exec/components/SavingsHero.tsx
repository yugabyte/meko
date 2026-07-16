// SavingsHero: the executive hero band — the account-wide "token burn saved"
// headline % on the left and dollars-saved-vs-premier-model bars on the right.
import { Flame } from "lucide-react";
import { Card, CardContent } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import {
  dollarsSavedByModel,
  formatUsd,
  tokenBurnPercent,
  OUR_INFERENCE_MODEL,
} from "@/lib/tokenSavings";

type Props = {
  totalRequests: number;
  totalTokensSaved: number;
  isLoading?: boolean;
};

export function SavingsHero({ totalRequests, totalTokensSaved, isLoading }: Props) {
  const percent = tokenBurnPercent(totalRequests, totalTokensSaved);
  const bars = dollarsSavedByModel(totalRequests, totalTokensSaved);
  const maxDollars = Math.max(...bars.map((b) => b.dollars), 1);
  const hasData = totalRequests > 0;

  return (
    <Card className="overflow-hidden">
      <CardContent className="grid gap-8 p-6 lg:grid-cols-[minmax(0,0.9fr)_minmax(0,1.1fr)]">
        {/* Headline: model-agnostic context reduction. */}
        <div className="flex flex-col justify-center gap-1 lg:border-r lg:border-border lg:pr-8">
          <div className="flex items-center gap-2 text-xs font-medium uppercase tracking-wider text-muted-foreground">
            <Flame className="h-4 w-4 text-orange-400" />
            Token Burn Saved
          </div>
          {isLoading ? (
            <Skeleton className="mt-1 h-14 w-40" />
          ) : (
            <p className="text-6xl font-bold leading-none text-orange-400">
              {hasData ? `${percent}%` : "—"}
            </p>
          )}
          <p className="mt-2 text-sm text-muted-foreground">
            {hasData
              ? `Context trimmed across ${totalRequests.toLocaleString()} contextual searches`
              : "No contextual searches recorded yet"}
          </p>
        </div>

        {/* Dollars saved by also running GPT-OSS-20B instead of each premier model. */}
        <div className="flex flex-col justify-center gap-3">
          <div className="flex items-center justify-between text-xs font-medium uppercase tracking-wider text-muted-foreground">
            <span>Money Saved</span>
            <span className="normal-case text-[11px]">vs premier model · on {OUR_INFERENCE_MODEL.label}</span>
          </div>
          {isLoading ? (
            <div className="flex flex-col gap-3">
              {[0, 1, 2].map((i) => (
                <Skeleton key={i} className="h-7 w-full" />
              ))}
            </div>
          ) : hasData ? (
            bars.map((b) => (
              <div key={b.key} className="flex items-center gap-3">
                <span className="w-16 shrink-0 text-sm font-medium text-muted-foreground">
                  {b.label}
                </span>
                <div className="h-7 flex-1 overflow-hidden rounded-md bg-muted">
                  <div
                    className="flex h-full items-center justify-end rounded-md bg-gradient-to-r from-emerald-600 to-emerald-400 px-2 text-xs font-semibold text-white transition-all"
                    style={{ width: `${Math.max(8, (b.dollars / maxDollars) * 100)}%` }}
                  >
                    {formatUsd(b.dollars)}
                  </div>
                </div>
              </div>
            ))
          ) : (
            <p className="text-sm text-muted-foreground">
              Dollars saved appear once contextual search is in use.
            </p>
          )}
          <p className="text-[11px] text-muted-foreground">
            Illustrative $/1M-token rates — swap in real card rates.
          </p>
        </div>
      </CardContent>
    </Card>
  );
}
