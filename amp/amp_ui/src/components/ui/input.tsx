import * as React from "react"

import { cn } from "@/lib/utils"

const Input = React.forwardRef<HTMLInputElement, React.ComponentProps<"input">>(
  ({ className, type, ...props }, ref) => {
    return (
      <input
        type={type}
        className={cn(
          // `border-border` + `bg-card` (not shadcn's default `border-input` +
          // transparent): in dark mode `--input` equals the page background, so
          // the unfocused field has no visible border. Mirrors the search-input
          // fix in layoutStyles.ts so fields read as a clear box on any surface.
          // aria-invalid is the field-level error state: a caller that marks the
          // input invalid gets the destructive border here rather than styling
          // it at every call site.
          "flex h-9 w-full rounded-md border border-border bg-card px-3 py-1 text-base shadow-sm transition-colors file:border-0 file:bg-transparent file:text-sm file:font-medium file:text-foreground placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring disabled:cursor-not-allowed disabled:opacity-50 aria-invalid:border-destructive aria-invalid:focus-visible:ring-destructive md:text-sm",
          className
        )}
        ref={ref}
        {...props}
      />
    )
  }
)
Input.displayName = "Input"

export { Input }
