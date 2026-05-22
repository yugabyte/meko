// Full-width inline error strip with light red background, used for load/API failures outside modals.
// Matches the red banner styling used in datapack panels; optional trailing action (e.g. Retry).
import type { ReactNode } from "react";
import { AlertCircle } from "lucide-react";
import { cn } from "@/lib/utils";

type Props = {
  children: ReactNode;
  /** e.g. a Retry button aligned to the right on wide screens */
  action?: ReactNode;
  className?: string;
};

export function InlineErrorBanner({ children, action, className }: Props) {
  return (
    <div
      role="alert"
      className={cn(
        "flex flex-col gap-3 rounded-lg border border-red-500/30 bg-red-500/10 px-4 py-3 text-sm text-red-700 dark:text-red-200 sm:flex-row sm:items-center sm:justify-between",
        className,
      )}
    >
      <div className="flex min-w-0 items-start gap-2.5">
        <AlertCircle className="mt-0.5 h-4 w-4 shrink-0 text-red-700 dark:text-red-300" aria-hidden />
        <div className="min-w-0 leading-snug">{children}</div>
      </div>
      {action ? <div className="flex shrink-0 justify-end sm:pl-2">{action}</div> : null}
    </div>
  );
}
