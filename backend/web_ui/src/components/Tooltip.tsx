// A hover and focus tooltip. It wraps its child in a span, so it also works on a disabled button,
// which fires no pointer events of its own.

import type { ReactNode } from "react";

export function Tooltip({ text, children }: { text: string; children: ReactNode }) {
  return (
    <span className="group relative inline-flex" tabIndex={0} aria-label={text}>
      {children}
      <span
        role="tooltip"
        className="pointer-events-none absolute right-0 top-full z-40 mt-2 w-64 rounded-lg border bg-card p-2.5 text-xs text-foreground opacity-0 shadow-lg transition group-hover:opacity-100 group-focus:opacity-100"
      >
        {text}
      </span>
    </span>
  );
}
