// Shared amber warning banner.
//
// The codebase had the same inline JSX repeated across several
// datapack pages (DatapackKnowledgeSummaryPage, DatapackLearningsPage,
// DatapackDetailPage, DatapackMemorySummaryPage,
// DatapackCollectiveMemorySummaryPage) and a few wizard steps — same
// className, same icon, same color tokens. This component centralises
// that pattern so future usages stay consistent in one place.
//
// Use it for soft warnings that don't require a dismiss button
// (missing configuration, recoverable error states, "you need to do X
// before continuing"). For destructive confirmations use AlertDialog.

import * as React from "react";
import { AlertTriangle } from "lucide-react";
import { cn } from "@/lib/utils";

type Props = React.HTMLAttributes<HTMLDivElement> & {
  /** Hide the leading icon — useful for inline / dense layouts where
   *  the surrounding context already conveys severity. */
  hideIcon?: boolean;
};

/** Amber-tinted soft warning. Always block-level; aligns icon and
 *  children on the start so multi-line copy wraps cleanly under the
 *  text rather than the icon. */
export function WarningBanner({
  className,
  hideIcon = false,
  children,
  ...rest
}: Props) {
  return (
    <div
      role="alert"
      className={cn(
        "flex items-start gap-2.5 rounded-md border border-amber-500/40 bg-amber-500/10 px-4 py-3 text-sm text-amber-100",
        className,
      )}
      {...rest}
    >
      {!hideIcon ? (
        <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0 text-amber-300" aria-hidden />
      ) : null}
      <div className="min-w-0 flex-1 leading-relaxed">{children}</div>
    </div>
  );
}
