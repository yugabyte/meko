// Reusable “Coming Soon” overlay for share settings and other areas where the feature is not live yet.
// Renders an optional blurred preview behind a centered badge so layouts stay visually consistent.
import type { ReactNode } from "react";

type Props = {
  /** Blurred fake UI behind the overlay (e.g. link sharing preview). Omit for a simple centered badge. */
  blurredPreview?: ReactNode;
  "aria-label"?: string;
};

export function ShareComingSoonPlaceholder({ blurredPreview, "aria-label": ariaLabel }: Props) {
  return (
    <div
      className="relative min-h-[112px] overflow-hidden rounded-lg border border-dashed border-border bg-muted/5"
      role="status"
      aria-label={ariaLabel ?? "Coming soon"}
    >
      {blurredPreview ? (
        /* Decorative only — no interaction until the real feature ships. */
        <div className="pointer-events-none select-none px-4 py-5 blur-[3px] opacity-60">{blurredPreview}</div>
      ) : null}

      <div className="absolute inset-0 flex items-center justify-center bg-background/50 backdrop-blur-[2px]">
        <p className="rounded-lg border border-border bg-card px-4 py-2 text-sm font-semibold tracking-tight text-foreground shadow-sm">
          Coming Soon
        </p>
      </div>
    </div>
  );
}
