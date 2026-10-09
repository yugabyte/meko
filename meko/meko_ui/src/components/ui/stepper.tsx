// Generic step progress strip: numbered circles joined by connectors; completed
// steps show a check, the current step is brand-highlighted. Mirrors the
// configure wizard's strip so every multi-step flow reads consistently.
import { Check } from "lucide-react";
import { cn } from "@/lib/utils";

export function Stepper({
  steps,
  current,
  className,
}: {
  steps: string[];
  current: number;
  className?: string;
}) {
  return (
    <div className={cn("flex w-full flex-wrap items-center gap-0 sm:flex-nowrap", className)}>
      {steps.map((label, index) => {
        const done = index < current;
        const active = index === current;
        const last = index === steps.length - 1;
        return (
          <div key={label} className="flex min-w-0 flex-1 items-center">
            <div className="flex min-w-0 flex-col items-center gap-2 sm:flex-row sm:gap-3">
              <div
                className={cn(
                  "flex h-9 w-9 shrink-0 items-center justify-center rounded-full border-2 text-xs font-semibold transition-colors",
                  done &&
                    "border-emerald-500/80 bg-emerald-500/20 text-emerald-700 dark:text-emerald-200",
                  active && !done && "border-brand bg-brand/20 text-brand",
                  !active && !done && "border-border bg-muted/40 text-muted-foreground",
                )}
              >
                {done ? <Check className="h-4 w-4" aria-hidden /> : index + 1}
              </div>
              <span
                className={cn(
                  "max-w-[5.5rem] text-center text-[11px] font-semibold uppercase tracking-wide sm:max-w-none sm:text-left sm:text-xs",
                  active ? "text-foreground" : "text-muted-foreground",
                )}
              >
                {label}
              </span>
            </div>
            {!last ? (
              <div
                className={cn(
                  "mx-1 hidden h-px min-w-[12px] flex-1 sm:block",
                  index < current ? "bg-emerald-500/50" : "bg-border",
                )}
                aria-hidden
              />
            ) : null}
          </div>
        );
      })}
    </div>
  );
}
