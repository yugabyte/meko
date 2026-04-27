/**
 * Visual progress strip for the configure wizard (four steps with connectors).
 * Completed steps show a check; the current step is highlighted for orientation.
 */
import { Check } from "lucide-react";
import { cn } from "@/lib/utils";
import { WIZARD_STEPS } from "./wizardSteps";

type Props = {
  currentIndex: number;
};

export function DatapackConfigureWizardStepper({ currentIndex }: Props) {
  return (
    <div className="flex w-full flex-wrap items-center gap-0 sm:flex-nowrap">
      {WIZARD_STEPS.map((step, index) => {
        const done = index < currentIndex;
        const active = index === currentIndex;
        const last = index === WIZARD_STEPS.length - 1;
        return (
          <div key={step.id} className="flex min-w-0 flex-1 items-center">
            <div className="flex min-w-0 flex-col items-center gap-2 sm:flex-row sm:gap-3">
              <div
                className={cn(
                  "flex h-9 w-9 shrink-0 items-center justify-center rounded-full border-2 text-xs font-semibold transition-colors",
                  done && "border-emerald-500/80 bg-emerald-500/20 text-emerald-200",
                  active && !done && "border-blue-500 bg-blue-500/20 text-blue-100",
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
                {step.label}
              </span>
            </div>
            {!last ? (
              <div
                className={cn(
                  "mx-1 hidden h-px min-w-[12px] flex-1 sm:block",
                  index < currentIndex ? "bg-emerald-500/50" : "bg-border",
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
