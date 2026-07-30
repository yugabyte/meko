import * as React from "react"

import { cn } from "@/lib/utils"

const Input = React.forwardRef<HTMLInputElement, React.ComponentProps<"input">>(
  ({ className, type, ...props }, ref) => {
    return (
      <input
        type={type}
        className={cn(
          // `border-border` + `bg-input` (not shadcn's default `border-input` +
          // transparent): in dark mode `--input` equals the page background, so
          // an `--input`-colored border is invisible. `border-border` keeps the
          // field bounded on any surface; `bg-input` keeps it inset against
          // cards rather than blending with `bg-card` — see the `--input` note
          // in index.css.
          "flex h-9 w-full rounded-md border border-border bg-input px-3 py-1 text-base shadow-sm transition-colors file:border-0 file:bg-transparent file:text-sm file:font-medium file:text-foreground placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring disabled:cursor-not-allowed disabled:opacity-50 md:text-sm",
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
