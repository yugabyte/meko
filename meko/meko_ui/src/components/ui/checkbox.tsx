import * as React from "react"
import * as CheckboxPrimitive from "@radix-ui/react-checkbox"
import { cva, type VariantProps } from "class-variance-authority"
import { Check } from "lucide-react"

import { cn } from "@/lib/utils"

// The checked/border color is a `variant`, so a surface picks its tone instead
// of hard-coding classes per call-site. A checkbox is a selection control, not
// a CTA, so `default` is NEUTRAL (foreground fill); `purple` matches a
// semantically-coded surface (e.g. the Learnings review); `brand` is the
// opt-in orange for the rare case a checkbox is itself a brand selection.
const checkboxVariants = cva(
  "grid place-content-center peer h-4 w-4 shrink-0 rounded-sm border shadow focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring disabled:cursor-not-allowed disabled:opacity-50",
  {
    variants: {
      variant: {
        default:
          "border-muted-foreground/40 data-[state=checked]:bg-foreground data-[state=checked]:text-background",
        brand:
          "border-brand/60 data-[state=checked]:bg-brand data-[state=checked]:text-brand-foreground",
        purple:
          "border-purple-500 data-[state=checked]:bg-purple-500 data-[state=checked]:text-white",
      },
    },
    defaultVariants: { variant: "default" },
  }
)

const Checkbox = React.forwardRef<
  React.ElementRef<typeof CheckboxPrimitive.Root>,
  React.ComponentPropsWithoutRef<typeof CheckboxPrimitive.Root> &
    VariantProps<typeof checkboxVariants>
>(({ className, variant, ...props }, ref) => (
  <CheckboxPrimitive.Root
    ref={ref}
    className={cn(checkboxVariants({ variant }), className)}
    {...props}
  >
    <CheckboxPrimitive.Indicator className={cn("grid place-content-center text-current")}>
      <Check className="h-4 w-4" />
    </CheckboxPrimitive.Indicator>
  </CheckboxPrimitive.Root>
))
Checkbox.displayName = CheckboxPrimitive.Root.displayName

export { Checkbox }
