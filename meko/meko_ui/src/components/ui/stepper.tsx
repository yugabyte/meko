// Generic step progress strip: numbered circles joined by connectors; completed
// steps show a check, the current step is brand-highlighted. Mirrors the
// configure wizard's strip so every multi-step flow reads consistently.
import { Check } from "lucide-react";
import { cn } from "@/lib/utils";

export function Stepper({
  steps,
  current,
  className,
  compact = false,
  onSelect,
}: {
  steps: string[];
  current: number;
  className?: string;
  /** Forces the stacked (circle-above-label, no connector) layout at every
   *  width instead of switching to a side-by-side layout past Tailwind's
   *  `sm` breakpoint. That breakpoint is viewport-width-based, so a Stepper
   *  confined to a narrow column (not the full viewport) still gets the
   *  side-by-side variant on any normal desktop window, which is what
   *  crams "SETUP"/"RUN THE REVIEW"/"SEE WHAT MEKO DID" against their
   *  circles in the Labs runner's left rail. Set this wherever the
   *  Stepper's own container is narrow regardless of viewport width. */
  compact?: boolean;
  /** When set, each step becomes a real, keyboard/AT-accessible button that
   *  calls this with its index — turns the strip into a click-to-navigate
   *  nav instead of a presentational progress indicator. Omitted (the only
   *  usage before this) keeps rendering plain, non-interactive divs. */
  onSelect?: (index: number) => void;
}) {
  return (
    <div
      className={cn("flex w-full flex-wrap items-center gap-0", !compact && "sm:flex-nowrap", className)}
    >
      {steps.map((label, index) => {
        const done = index < current;
        const active = index === current;
        const last = index === steps.length - 1;
        const content = (
          <>
            <div
              className={cn(
                "flex h-9 w-9 shrink-0 items-center justify-center rounded-full border-2 text-xs font-semibold transition-colors",
                done && "border-emerald-500/80 bg-emerald-500/20 text-emerald-700 dark:text-emerald-200",
                active && !done && "border-brand bg-brand/20 text-brand",
                !active && !done && "border-border bg-muted/40 text-muted-foreground",
              )}
            >
              {done ? <Check className="h-4 w-4" aria-hidden /> : index + 1}
            </div>
            <span
              className={cn(
                "max-w-[5.5rem] text-center text-[11px] font-semibold uppercase tracking-wide",
                !compact && "sm:max-w-none sm:text-left sm:text-xs",
                active ? "text-foreground" : "text-muted-foreground",
              )}
            >
              {label}
            </span>
          </>
        );
        const contentClassName = cn(
          "flex min-w-0 flex-col items-center gap-2",
          !compact && "sm:flex-row sm:gap-3",
        );
        return (
          <div key={label} className="flex min-w-0 flex-1 items-center">
            {onSelect ? (
              <button
                type="button"
                onClick={() => onSelect(index)}
                aria-current={active ? "step" : undefined}
                className={cn(
                  contentClassName,
                  "rounded-md focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
                )}
              >
                {content}
              </button>
            ) : (
              <div className={contentClassName}>{content}</div>
            )}
            {!last ? (
              <div
                className={cn(
                  "mx-1 hidden h-px min-w-[12px] flex-1",
                  !compact && "sm:block",
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
