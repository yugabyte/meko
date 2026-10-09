// Shared soft status banner: amber for a warning, emerald for a good
// outcome. Same shape either way -- a tinted block with a leading icon --
// so a screen that shows one and then the other doesn't change layout
// between them.
//
// The codebase had the same inline JSX repeated across several
// datapack pages (DatapackKnowledgeSummaryPage, DatapackLearningsPage,
// DatapackDetailPage, DatapackMemorySummaryPage,
// DatapackCollectiveMemorySummaryPage) and a few wizard steps — same
// className, same icon, same color tokens. This component centralises
// that pattern so future usages stay consistent in one place.
//
// Use it for soft messages that don't require a dismiss button (missing
// configuration, recoverable error states, "you need to do X before
// continuing", or the confirmation that a choice worked out). For
// destructive confirmations use AlertDialog.

import * as React from "react";
import { AlertTriangle, CheckCircle2 } from "lucide-react";
import { cn } from "@/lib/utils";

/** Named for what the message MEANS, not the colour it renders — the
 *  palette is this component's business, not the caller's. */
export type WarningBannerTone = "warning" | "success";

/** Same amber and emerald the rest of the app already uses for these two
 *  meanings (see FindingsList's severity chips, KnowledgeBaseStage's done
 *  state) rather than a second green invented here. */
const TONE: Record<
  WarningBannerTone,
  { container: string; icon: string; Icon: typeof AlertTriangle }
> = {
  warning: {
    container: "border-amber-500/40 bg-amber-500/10 text-amber-700 dark:text-amber-100",
    icon: "text-amber-700 dark:text-amber-300",
    Icon: AlertTriangle,
  },
  success: {
    container: "border-emerald-500/40 bg-emerald-500/10 text-emerald-800 dark:text-emerald-100",
    icon: "text-emerald-700 dark:text-emerald-300",
    Icon: CheckCircle2,
  },
};

type Props = React.HTMLAttributes<HTMLDivElement> & {
  /** Hide the leading icon — useful for inline / dense layouts where
   *  the surrounding context already conveys severity. */
  hideIcon?: boolean;
  /** Defaults to "warning", which is every pre-existing call site. */
  tone?: WarningBannerTone;
};

/** Amber-tinted soft warning. Always block-level; aligns icon and
 *  children on the start so multi-line copy wraps cleanly under the
 *  text rather than the icon. */
export function WarningBanner({
  className,
  hideIcon = false,
  tone = "warning",
  children,
  ...rest
}: Props) {
  const { container, icon, Icon } = TONE[tone];
  return (
    <div
      // A success message isn't an alert; announcing it as one interrupts a
      // screen reader for good news.
      role={tone === "warning" ? "alert" : "status"}
      className={cn(
        "flex items-start gap-2.5 rounded-md border px-4 py-3 text-sm",
        container,
        className,
      )}
      {...rest}
    >
      {!hideIcon ? <Icon className={cn("mt-0.5 h-4 w-4 shrink-0", icon)} aria-hidden /> : null}
      <div className="min-w-0 flex-1 leading-relaxed">{children}</div>
    </div>
  );
}
