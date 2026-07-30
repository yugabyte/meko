import * as React from "react"
import * as TabsPrimitive from "@radix-ui/react-tabs"
import { cva, type VariantProps } from "class-variance-authority"

import { cn } from "@/lib/utils"

const Tabs = TabsPrimitive.Root

// Tab look is a `variant`, not per-file className. A call-site sets `variant`
// once on <TabsList> and every <TabsTrigger> inside inherits it via context, so
// the active-state color/shape lives here in one place:
//   - default   — pill track (muted bg), active = primary fill + shadow.
//   - segmented — flat control inside a bordered box, active = primary fill.
// (Bespoke one-off tab styles, e.g. the share dialog's underline invite tabs,
//  can still pass className; add a variant here if a look recurs.)
type TabsVariant = "default" | "segmented"
const TabsVariantContext = React.createContext<TabsVariant>("default")

const tabsListVariants = cva("inline-flex items-center text-muted-foreground", {
  variants: {
    variant: {
      default: "h-9 justify-center rounded-lg bg-muted p-1",
      segmented: "min-h-9 gap-1 rounded-lg border border-border bg-input/90 p-1",
    },
  },
  defaultVariants: { variant: "default" },
})

const tabsTriggerVariants = cva(
  "inline-flex items-center justify-center whitespace-nowrap text-sm font-medium text-muted-foreground ring-offset-background transition-all hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 disabled:pointer-events-none disabled:opacity-50 data-[state=active]:bg-primary data-[state=active]:text-primary-foreground",
  {
    variants: {
      variant: {
        default: "rounded-md px-3 py-1 data-[state=active]:shadow-md",
        segmented: "h-7 rounded-md px-3",
      },
    },
    defaultVariants: { variant: "default" },
  }
)

const TabsList = React.forwardRef<
  React.ElementRef<typeof TabsPrimitive.List>,
  React.ComponentPropsWithoutRef<typeof TabsPrimitive.List> & VariantProps<typeof tabsListVariants>
>(({ className, variant, ...props }, ref) => {
  const resolved: TabsVariant = variant ?? "default"
  return (
    <TabsVariantContext.Provider value={resolved}>
      <TabsPrimitive.List
        ref={ref}
        className={cn(tabsListVariants({ variant: resolved }), className)}
        {...props}
      />
    </TabsVariantContext.Provider>
  )
})
TabsList.displayName = TabsPrimitive.List.displayName

const TabsTrigger = React.forwardRef<
  React.ElementRef<typeof TabsPrimitive.Trigger>,
  React.ComponentPropsWithoutRef<typeof TabsPrimitive.Trigger> & VariantProps<typeof tabsTriggerVariants>
>(({ className, variant, ...props }, ref) => {
  // Inherit the list's variant unless a trigger overrides it explicitly.
  const ctx = React.useContext(TabsVariantContext)
  const resolved: TabsVariant = variant ?? ctx
  return (
    <TabsPrimitive.Trigger
      ref={ref}
      className={cn(tabsTriggerVariants({ variant: resolved }), className)}
      {...props}
    />
  )
})
TabsTrigger.displayName = TabsPrimitive.Trigger.displayName

const TabsContent = React.forwardRef<
  React.ElementRef<typeof TabsPrimitive.Content>,
  React.ComponentPropsWithoutRef<typeof TabsPrimitive.Content>
>(({ className, ...props }, ref) => (
  <TabsPrimitive.Content
    ref={ref}
    className={cn(
      "mt-2 ring-offset-background focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 data-[state=active]:animate-in data-[state=active]:fade-in-0 data-[state=active]:slide-in-from-right-2 data-[state=inactive]:animate-out data-[state=inactive]:fade-out-0 data-[state=inactive]:slide-out-to-left-2",
      className
    )}
    {...props}
  />
))
TabsContent.displayName = TabsPrimitive.Content.displayName

export { Tabs, TabsList, TabsTrigger, TabsContent }
