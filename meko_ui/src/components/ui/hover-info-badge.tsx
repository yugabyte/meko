import { type ReactNode } from "react";
import { cn } from "@/lib/utils";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";

type HoverInfoBadgeProps = {
  label: string;
  ariaLabel?: string;
  triggerClassName?: string;
  contentClassName?: string;
  children: ReactNode;
};

/**
 * Reusable compact badge that opens rich hover content.
 * Radix Tooltip handles hover / focus / touch natively in uncontrolled
 * mode — we only stop the trigger's mousedown so a click doesn't focus
 * the button and pin the tooltip open after the cursor leaves.
 */
export function HoverInfoBadge({
  label,
  ariaLabel,
  triggerClassName,
  contentClassName,
  children,
}: HoverInfoBadgeProps) {
  return (
    <Tooltip delayDuration={0}>
      <TooltipTrigger asChild>
        <button
          type="button"
          className={cn(
            "rounded-full border border-blue-400/20 bg-blue-400/10 px-1.5 py-0.5 text-[10px] text-blue-700 dark:text-blue-200/80",
            triggerClassName,
          )}
          aria-label={ariaLabel || label}
          onMouseDown={(e) => e.preventDefault()}
        >
          {label}
        </button>
      </TooltipTrigger>
      <TooltipContent
        sideOffset={8}
        side="top"
        align="start"
        collisionPadding={12}
        className={cn(
          "z-[90] w-[280px] rounded-lg border border-border bg-popover px-2.5 py-2 text-left text-[10px] text-popover-foreground shadow-lg",
          contentClassName,
        )}
      >
        {children}
      </TooltipContent>
    </Tooltip>
  );
}
