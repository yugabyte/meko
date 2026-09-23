import { type ReactNode } from "react";
import { Info } from "lucide-react";
import { cn } from "@/lib/utils";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";

type HoverInfoBadgeProps = {
  /** The badge's text. In `icon` variant it is not drawn, and becomes the
   *  accessible name of the icon button instead. */
  label: string;
  ariaLabel?: string;
  /**
   * `badge` (default) draws `label` as a tinted pill, for annotating a value
   * inline. `icon` draws a muted ⓘ, for "what is this?" beside a heading,
   * where a pill would compete with the heading for attention.
   */
  variant?: "badge" | "icon";
  triggerClassName?: string;
  contentClassName?: string;
  children: ReactNode;
};

/**
 * Reusable trigger that opens rich hover content.
 * Radix Tooltip handles hover / focus / touch natively in uncontrolled
 * mode — we only stop the trigger's mousedown so a click doesn't focus
 * the button and pin the tooltip open after the cursor leaves.
 */
export function HoverInfoBadge({
  label,
  ariaLabel,
  variant = "badge",
  triggerClassName,
  contentClassName,
  children,
}: HoverInfoBadgeProps) {
  const isIcon = variant === "icon";
  return (
    <Tooltip delayDuration={0}>
      <TooltipTrigger asChild>
        <button
          type="button"
          className={cn(
            isIcon
              ? "inline-flex shrink-0 items-center rounded-full text-muted-foreground/70 transition-colors hover:text-foreground"
              : "rounded-full border border-blue-400/20 bg-blue-400/10 px-1.5 py-0.5 text-[10px] text-blue-700 dark:text-blue-200/80",
            triggerClassName,
          )}
          aria-label={ariaLabel || label}
          onMouseDown={(e) => e.preventDefault()}
        >
          {isIcon ? <Info className="h-3.5 w-3.5" aria-hidden /> : label}
        </button>
      </TooltipTrigger>
      <TooltipContent
        sideOffset={8}
        side="top"
        align="start"
        collisionPadding={12}
        className={cn(
          "z-[90] w-[280px] rounded-lg border border-border bg-popover px-2.5 py-2 text-left text-[10px] text-popover-foreground shadow-lg",
          // An icon tip carries a sentence, not the dense metric block the
          // badge was built for, so 10px would be unreadable prose.
          isIcon && "w-[300px] text-xs font-normal leading-relaxed",
          contentClassName,
        )}
      >
        {children}
      </TooltipContent>
    </Tooltip>
  );
}
