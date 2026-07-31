// Shared chrome for the data-backed workspace pages (Shared pools, Fleet,
// Multitenancy). Centralises the eyebrow + title + description header and the
// loading / error / empty states so the three pages stay visually consistent
// and don't each re-roll the same markup.

import type { ReactNode } from "react";
import { AlertCircle, Loader2 } from "lucide-react";
import { cn } from "@/lib/utils";

export function PageContainer({ children }: { children: ReactNode }) {
  return <div className="mx-auto max-w-7xl px-6 py-10">{children}</div>;
}

export function PageHeader({
  eyebrow,
  eyebrowBadge,
  title,
  description,
  action,
}: {
  eyebrow: string;
  /** Optional teal pill next to the eyebrow (e.g. "TENANT ADMIN"). */
  eyebrowBadge?: string;
  title: string;
  description?: ReactNode;
  action?: ReactNode;
}) {
  return (
    <div className="mb-8 flex flex-wrap items-start justify-between gap-4">
      <div className="min-w-0">
        <div className="flex items-center gap-2">
          <span className="text-xs font-semibold uppercase tracking-[0.14em] text-brand">
            {eyebrow}
          </span>
          {eyebrowBadge && (
            <span className="rounded-md border border-teal-500/40 px-2 py-0.5 text-[10px] font-semibold uppercase tracking-[0.1em] text-teal-600 dark:text-teal-400">
              {eyebrowBadge}
            </span>
          )}
        </div>
        <h1 className="mt-2 text-3xl font-bold tracking-tight text-foreground">
          {title}
        </h1>
        {description && (
          <p className="mt-2 max-w-2xl text-sm leading-relaxed text-muted-foreground">
            {description}
          </p>
        )}
      </div>
      {action && <div className="shrink-0">{action}</div>}
    </div>
  );
}

export function LoadingState({ label = "Loading…" }: { label?: string }) {
  return (
    <div className="flex items-center gap-2 py-16 text-sm text-muted-foreground">
      <Loader2 className="h-4 w-4 animate-spin" />
      {label}
    </div>
  );
}

export function ErrorState({
  error,
  className,
}: {
  error: unknown;
  className?: string;
}) {
  const message =
    error instanceof Error ? error.message : "Something went wrong.";
  return (
    <div
      className={cn(
        "flex items-start gap-3 rounded-xl border border-destructive/40 bg-destructive/5 p-4 text-sm",
        className,
      )}
    >
      <AlertCircle className="mt-0.5 h-4 w-4 shrink-0 text-destructive" />
      <div>
        <p className="font-medium text-foreground">Couldn&apos;t load this data</p>
        <p className="mt-0.5 text-muted-foreground">{message}</p>
        <p className="mt-1 text-xs text-muted-foreground">
          Is the controller API reachable at <code>/api/v1</code>?
        </p>
      </div>
    </div>
  );
}
