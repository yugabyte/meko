// Shared "section card" used by datapack-detail panels (Conversations,
// Learnings/MemoryPromotion, etc.). Owns the consistent visual chrome:
//   - Outer card (raised surface via `bg-card`, rounded, bordered).
//   - Header row with leading icon-square, title (+ optional count suffix),
//     supporting description, and a Refresh button on the right.
//   - Optional red error banner(s) below the header.
//
// The body — list of rows, table, pagination, etc. — is supplied as
// `children` so each panel can stay opinionated about its own row layout
// while sharing the wrapper.

import type { ReactNode } from "react";
import { Loader2, RefreshCw, AlertCircle } from "lucide-react";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

export type DatapackPanelShellProps = {
  /** Lucide icon (or any node) shown inside the leading rounded square. */
  icon: ReactNode;
  /**
   * Tailwind classes that color the icon-square background + ring.
   * Default = neutral slate. Each panel picks its own accent (purple for
   * learnings, slate for conversations, etc.) to stay visually distinct.
   */
  iconAccentClass?: string;
  /** Header title; pass JSX so callers can append " · 12 sessions" etc. */
  title: ReactNode;
  /** Sub-line under the title — keeps the same muted styling everywhere. */
  description?: ReactNode;

  /** Refresh button is rendered only when onRefresh is supplied. */
  onRefresh?: () => void;
  refreshDisabled?: boolean;
  refreshLoading?: boolean;
  refreshLabel?: string;
  refreshLoadingLabel?: string;

  /**
   * Error messages rendered as red banners between the header and children.
   * Falsy entries are skipped so callers can pass conditional errors
   * inline without wrapping each in a ternary.
   */
  errors?: Array<string | null | undefined>;

  className?: string;
  children?: ReactNode;
};

export default function DatapackPanelShell({
  icon,
  iconAccentClass = "bg-slate-500/15 ring-slate-500/10",
  title,
  description,
  onRefresh,
  refreshDisabled,
  refreshLoading,
  refreshLabel = "Refresh",
  refreshLoadingLabel = "Loading…",
  errors,
  className,
  children,
}: DatapackPanelShellProps) {
  const visibleErrors = (errors ?? []).filter(
    (e): e is string => typeof e === "string" && e.length > 0,
  );

  return (
    <div className={cn("rounded-xl border border-border bg-card p-6 space-y-4", className)}>
      <div className="flex items-center justify-between gap-4">
        <div className="flex items-center gap-4 min-w-0">
          <div
            className={cn(
              "flex h-10 w-10 shrink-0 items-center justify-center rounded-lg ring-1",
              iconAccentClass,
            )}
          >
            {icon}
          </div>
          <div className="min-w-0">
            <h3 className="text-base font-semibold tracking-tight text-foreground">{title}</h3>
            {description ? (
              <p className="mt-1 text-xs leading-relaxed text-muted-foreground">{description}</p>
            ) : null}
          </div>
        </div>
        {onRefresh ? (
          <Button
            variant="outline"
            className="gap-2 border-border bg-background text-muted-foreground hover:bg-muted hover:text-foreground"
            onClick={onRefresh}
            disabled={refreshDisabled ?? refreshLoading}
          >
            {refreshLoading ? (
              <Loader2 className="w-4 h-4 animate-spin" />
            ) : (
              <RefreshCw className="w-4 h-4" />
            )}
            {refreshLoading ? refreshLoadingLabel : refreshLabel}
          </Button>
        ) : null}
      </div>

      {visibleErrors.map((msg, i) => (
        <div
          key={i}
          className="flex items-center gap-2 rounded-lg border border-red-500/30 bg-red-500/10 px-4 py-2 text-xs text-red-700 dark:text-red-300"
        >
          <AlertCircle className="w-4 h-4 shrink-0" />
          {msg}
        </div>
      ))}

      {children}
    </div>
  );
}
