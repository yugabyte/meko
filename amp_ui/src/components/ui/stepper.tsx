// Stepper — horizontal numbered progress header for multi-step flows (wizards).
// Presentation-only: the parent owns the current index. Theme-token styled so it
// works in light/dark and is reusable across products.

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
    <div className={cn("flex gap-2", className)}>
      {steps.map((label, i) => {
        const active = i === current;
        const done = i < current;
        return (
          <div key={label} className="flex-1 space-y-2">
            <div className={cn("h-[3px] rounded-full bg-border", (active || done) && "bg-brand")} />
            <div
              className={cn(
                "flex items-center gap-2 text-xs text-muted-foreground",
                active && "text-foreground",
              )}
            >
              <span
                className={cn(
                  "flex h-[18px] w-[18px] items-center justify-center rounded-full border border-border text-[11px] tabular-nums text-muted-foreground",
                  active && "border-brand bg-brand text-white",
                  done && "border-brand text-brand",
                )}
              >
                {done ? <Check className="h-3 w-3" /> : i + 1}
              </span>
              {label}
            </div>
          </div>
        );
      })}
    </div>
  );
}
