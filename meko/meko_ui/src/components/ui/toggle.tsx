import * as React from "react";
import * as TogglePrimitive from "@radix-ui/react-toggle";
import { cva, type VariantProps } from "class-variance-authority";

import { cn } from "@/lib/utils";

const toggleVariants = cva(
  "inline-flex items-center justify-center gap-2 rounded-md text-sm font-medium transition-colors hover:bg-muted hover:text-muted-foreground focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring disabled:pointer-events-none disabled:opacity-50 data-[state=on]:bg-accent data-[state=on]:text-accent-foreground [&_svg]:pointer-events-none [&_svg]:size-4 [&_svg]:shrink-0",
  {
    variants: {
      variant: {
        default: "bg-transparent",
        outline:
          "border border-border bg-transparent shadow-sm hover:bg-accent hover:text-accent-foreground",
        /* A joined picker where exactly one option is always chosen (a chart's
         * Line/Bar). `outline` can't serve it: its selected state is `accent`,
         * which is also its hover, so the chosen option is indistinguishable
         * from the one under the cursor. Square inner corners and a -1px pull
         * collapse adjacent borders into the hairline between options — pair
         * with `className="gap-0"`, since ToggleGroup's root sets `gap-1`. */
        segmented:
          "border border-border bg-background text-muted-foreground shadow-sm rounded-none first:rounded-l-md last:rounded-r-md -ml-px first:ml-0 hover:bg-muted hover:text-foreground data-[state=on]:bg-primary data-[state=on]:text-primary-foreground data-[state=on]:hover:bg-primary",
      },
      size: {
        /* Smallest that still clears the 24px pointer-target minimum — for a
         * secondary control beside a heading, where `sm` reads as heavier
         * than the thing it belongs to. */
        xs: "h-7 px-2 min-w-7 text-xs",
        default: "h-9 px-2 min-w-9",
        sm: "h-8 px-1.5 min-w-8",
        lg: "h-10 px-2.5 min-w-10",
      },
    },
    defaultVariants: {
      variant: "default",
      size: "default",
    },
  },
);

const Toggle = React.forwardRef<
  React.ElementRef<typeof TogglePrimitive.Root>,
  React.ComponentPropsWithoutRef<typeof TogglePrimitive.Root> & VariantProps<typeof toggleVariants>
>(({ className, variant, size, ...props }, ref) => (
  <TogglePrimitive.Root
    ref={ref}
    className={cn(toggleVariants({ variant, size, className }))}
    {...props}
  />
));

Toggle.displayName = TogglePrimitive.Root.displayName;

export { Toggle, toggleVariants };
