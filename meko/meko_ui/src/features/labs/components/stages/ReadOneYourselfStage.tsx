// Chapter 4's third step: one real thread, read end to end.
//
// The list before this one proves the reviews exist. This one is about what
// is IN them, and specifically that the loop closes: AgentK makes a specific,
// checkable claim about a line of C++, and the engineer who received it
// agrees and fixes it. A wall of agent comments with nobody answering would
// show the opposite.

import { CornerDownRight, ExternalLink } from "lucide-react";

import { cn } from "@/lib/utils";
import { Badge } from "@/components/ui/badge";

import {
  AGENTK_DASHBOARD_URL,
  FEATURED_REVIEW,
  PUBLIC_REVIEWS_READ_ON,
  pullRequestUrl,
  reviewCommentUrl,
} from "../../data/adlcReviewAgentLab/publicReviews";
import { InlineMarkdown } from "../demo-apps/InlineMarkdown";
import { LabStageHeader } from "./LabStageHeader";

const R = FEATURED_REVIEW;

export function ReadOneYourselfStage() {
  return (
    <div className="flex flex-1 flex-col gap-6 overflow-y-auto p-6">
      <LabStageHeader
        eyebrow="Chapter 4 · Step 3"
        title="Read one for yourself"
        lede="One pull request, in full. Every comment below is on GitHub right now, and so is the reply under it. Nothing here is a screenshot."
        tags={["Production"]}
      />

      <a
        href={pullRequestUrl(R.pr)}
        target="_blank"
        rel="noreferrer noopener"
        className="flex items-center gap-3 rounded-lg border border-border bg-card p-4 transition-colors hover:border-foreground/25"
      >
        <span className="shrink-0 font-mono text-xs text-muted-foreground">#{R.pr}</span>
        <span className="min-w-0 flex-1">
          <span className="block truncate text-sm font-semibold text-foreground">{R.title}</span>
          <span className="mt-0.5 block text-[11px] text-muted-foreground">
            opened by {R.author} on yugabyte/yugabyte-db
          </span>
        </span>
        <ExternalLink className="h-4 w-4 shrink-0 text-muted-foreground" aria-hidden />
      </a>

      <div className="flex flex-col gap-4">
        {R.comments.map((comment) => (
          <article
            key={comment.id}
            className="flex overflow-hidden rounded-lg border border-border bg-card"
          >
            <span
              className={cn(
                "w-1 shrink-0",
                comment.severity === "major" ? "bg-destructive" : "bg-warning",
              )}
              aria-hidden
            />
            <div className="flex min-w-0 flex-1 flex-col gap-3 p-4">
              <div className="flex flex-wrap items-center gap-2">
                <Badge
                  variant="outline"
                  className={cn(
                    "shrink-0 border text-[10px]",
                    comment.severity === "major"
                      ? "border-destructive/40 text-destructive"
                      : "border-warning/40 text-warning",
                  )}
                >
                  {comment.severity}
                </Badge>
                <code className="min-w-0 flex-1 truncate font-mono text-[11px] text-muted-foreground">
                  {comment.path}
                  {comment.line !== null && `:${comment.line}`}
                </code>
                <a
                  href={reviewCommentUrl(R.pr, comment.id)}
                  target="_blank"
                  rel="noreferrer noopener"
                  className="inline-flex shrink-0 items-center gap-1 text-[11px] text-primary hover:underline"
                >
                  <ExternalLink className="h-3 w-3" aria-hidden />
                  on GitHub
                </a>
              </div>

              {comment.body.split("\n\n").map((para, i) => (
                <p key={i} className="whitespace-pre-line text-xs leading-relaxed text-foreground">
                  <InlineMarkdown text={para} />
                </p>
              ))}

              {/* The reply is the half that makes this worth reading. An
                  agent's claim is a claim; an author agreeing with it is
                  evidence. */}
              <div className="flex gap-2.5 rounded-md border border-context-knowledge/25 bg-context-knowledge-soft p-3">
                <CornerDownRight
                  className="mt-0.5 h-3.5 w-3.5 shrink-0 text-context-knowledge"
                  aria-hidden
                />
                <div className="min-w-0 flex-1">
                  <p className="text-[11px] font-semibold text-context-knowledge">
                    {comment.replyAuthor} replied
                  </p>
                  <p className="mt-1 text-xs leading-relaxed text-foreground">
                    <InlineMarkdown text={comment.reply} />
                  </p>
                </div>
              </div>
            </div>
          </article>
        ))}
      </div>

      <div className="flex flex-wrap items-center gap-2">
        <Badge variant="secondary" className="text-[10px]">
          read {PUBLIC_REVIEWS_READ_ON}
        </Badge>
        <span className="text-xs text-muted-foreground">
          Comments are trimmed to the claim and its reasoning. The real ones run several times
          longer, which is what the GitHub links are for.
        </span>
        <a
          href={AGENTK_DASHBOARD_URL}
          target="_blank"
          rel="noreferrer noopener"
          className="ml-auto inline-flex items-center gap-1 text-xs text-primary hover:underline"
        >
          <ExternalLink className="h-3 w-3" aria-hidden />
          Where AgentK runs
        </a>
      </div>
    </div>
  );
}
