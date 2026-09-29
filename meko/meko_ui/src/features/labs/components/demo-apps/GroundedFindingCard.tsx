// One review comment, shown the way a reviewer would leave it: the claim, the
// code it points at with the offending lines marked, and the rules behind it.
//
// The code excerpt comes from the diff the agent was given, renumbered into
// the new file's frame (lib/diffExcerpt.ts). It renders only when the
// finding's own location resolves against that diff; a location the diff does
// not contain shows no code rather than marking a line at random.

import { cn } from "@/lib/utils";
import { Badge } from "@/components/ui/badge";

import type { ReviewFinding } from "../../data/adlcReviewAgentLab/anthropicReferenceReview";
import { excerptForFinding, type DiffFile } from "../../lib/diffExcerpt";
import { groundFinding, type RetrievedSource } from "../../lib/findingGrounding";
import { ContextCitation } from "./ContextCitation";
import { InlineMarkdown } from "./InlineMarkdown";

const SEVERITY: Record<ReviewFinding["severity"], { bar: string; badge: string; label: string }> = {
  critical: { bar: "bg-destructive", badge: "border-destructive/40 text-destructive", label: "blocking" },
  major: { bar: "bg-warning", badge: "border-warning/40 text-warning", label: "major" },
  minor: { bar: "bg-border", badge: "border-border text-muted-foreground", label: "minor" },
};

/** The agent writes one paragraph. Its first sentence is the claim and reads
 *  as a title; anything after it is the explanation. */
function splitComment(comment: string): { title: string; body: string | null } {
  const match = /^([\s\S]*?[.!?])\s+([\s\S]*)$/.exec(comment.trim());
  if (!match) return { title: comment.trim(), body: null };
  return { title: match[1].trim(), body: match[2].trim() };
}

export function GroundedFindingCard({
  finding,
  diffFiles,
  sources,
}: {
  finding: ReviewFinding;
  /** The diff this review was run against, already parsed. */
  diffFiles: DiffFile[];
  /** Everything the run retrieved. Matched to this finding by word overlap,
   *  which is a match and not an attribution: see lib/findingGrounding.ts. */
  sources: RetrievedSource[];
}) {
  const tone = SEVERITY[finding.severity];
  const { title, body } = splitComment(finding.comment);
  const excerpt = finding.location ? excerptForFinding(diffFiles, finding.location) : null;
  // Only the comments worth arguing with get their rules attached. A minor
  // nit grounded in a standards chunk reads as over-claiming.
  const grounded =
    finding.severity === "minor" ? [] : groundFinding(finding.comment, sources);

  return (
    <article className="flex overflow-hidden rounded-lg border border-border bg-card">
      <span className={cn("w-1 shrink-0", tone.bar)} aria-hidden />
      <div className="flex min-w-0 flex-1 flex-col gap-3 p-4">
        <div className="flex flex-wrap items-start gap-2">
          <h4 className="min-w-0 flex-1 text-sm font-semibold text-foreground">
            <InlineMarkdown text={title} />
          </h4>
          <Badge variant="outline" className={cn("shrink-0 border", tone.badge)}>
            {tone.label}
          </Badge>
        </div>

        {body && (
          <p className="text-xs leading-relaxed text-muted-foreground">
            <InlineMarkdown text={body} />
          </p>
        )}

        {excerpt ? (
          <div className="overflow-hidden rounded-md border border-border">
            <div className="border-b border-border bg-muted/40 px-3 py-1.5 font-mono text-[11px] text-muted-foreground">
              {excerpt.path}
            </div>
            <pre className="overflow-x-auto py-1 font-mono text-xs leading-relaxed">
              {excerpt.lines.map((line) => {
                const flagged =
                  line.lineNumber !== null &&
                  line.lineNumber >= excerpt.highlightStart &&
                  line.lineNumber <= excerpt.highlightEnd;
                return (
                  <div
                    key={line.lineNumber}
                    className={cn(
                      "flex border-l-2 px-3",
                      flagged
                        ? "border-destructive bg-destructive/10 text-destructive"
                        : "border-transparent text-muted-foreground",
                    )}
                  >
                    <span className="mr-3 w-7 shrink-0 select-none text-right opacity-60">
                      {line.lineNumber}
                    </span>
                    <span className="min-w-0 flex-1 whitespace-pre">{line.text || " "}</span>
                  </div>
                );
              })}
            </pre>
          </div>
        ) : (
          finding.location && (
            <p className="font-mono text-[11px] text-muted-foreground">{finding.location}</p>
          )
        )}

        {grounded.length > 0 && (
          <div className="flex flex-col gap-2">
            <p className="text-[11px] font-semibold uppercase tracking-wide text-muted-foreground">
              What it read to say this
            </p>
            {grounded.map((source, i) => (
              <ContextCitation
                key={i}
                kind={source.kind}
                source={source.source}
                quote={source.quote}
              />
            ))}
          </div>
        )}
      </div>
    </article>
  );
}
