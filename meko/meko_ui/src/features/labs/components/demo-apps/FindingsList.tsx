import { cn } from "@/lib/utils";
import type { ReviewFinding } from "../../data/adlcReviewAgentLab/anthropicReferenceReview";

const SEVERITY_STYLE: Record<ReviewFinding["severity"], string> = {
  critical: "bg-red-500/10 text-red-700 dark:text-red-300",
  major: "bg-amber-500/10 text-amber-700 dark:text-amber-300",
  minor: "bg-muted text-muted-foreground",
};

/** Renders a review's structured findings (severity + location + comment),
 *  in place of a free-text prose review -- see review.py's
 *  REVIEW_SYSTEM_PROMPT/_parse_findings, which is where this shape comes
 *  from. Used for both the static Claude reference and a live model run;
 *  never for a follow-up answer, which stays free-text (see
 *  deploymentInvokeApi.ts's ReviewStreamEvent). */
export function FindingsList({ findings }: { findings: ReviewFinding[] }) {
  if (findings.length === 0) {
    return <p className="text-sm text-muted-foreground">No issues found.</p>;
  }
  return (
    <div className="flex flex-col divide-y divide-border overflow-hidden rounded-md border border-border">
      {findings.map((finding, i) => (
        <div key={i} className="flex gap-3 p-3">
          <span
            className={cn(
              "h-fit flex-none rounded px-1.5 py-0.5 text-[10.5px] font-semibold uppercase tracking-wide",
              SEVERITY_STYLE[finding.severity],
            )}
          >
            {finding.severity}
          </span>
          <div className="min-w-0 flex-1">
            {finding.location && (
              <div className="font-mono text-xs text-muted-foreground">{finding.location}</div>
            )}
            <p className="text-sm">{finding.comment}</p>
          </div>
        </div>
      ))}
    </div>
  );
}
