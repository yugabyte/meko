// Chapter 4's second step: AgentK's real reviews on a public repository,
// every row linking out to the thing it describes.
//
// The deep links are the whole point. Yugabyte's main repo is open source, so
// nothing here has to be taken on trust: a reader who doubts a row can open
// it and read the thread. That is also why the counts are per severity and
// why a review that found nothing is listed.

import { ExternalLink } from "lucide-react";

import { cn } from "@/lib/utils";
import { Badge } from "@/components/ui/badge";

import {
  AGENTK_BOT_LOGIN,
  PUBLIC_REVIEWS_READ_ON,
  PUBLIC_RUNS,
  pullRequestUrl,
  YUGABYTE_DB_REPO,
  type PublicRun,
} from "../../data/adlcReviewAgentLab/publicReviews";
import { LabStageHeader } from "./LabStageHeader";

const TOTAL_FINDINGS = PUBLIC_RUNS.reduce((sum, run) => sum + run.major + run.minor, 0);

export function PublicRunsStage() {
  return (
    <div className="flex flex-1 flex-col gap-6 overflow-y-auto p-6">
      <LabStageHeader
        eyebrow="Chapter 4 · Step 2"
        title="The public runs"
        lede={`Yugabyte's main repository is open source, so AgentK's reviews are public too. These are real pull requests. Open any of them and read the thread yourself.`}
        tags={["Production"]}
      />

      <div className="flex flex-wrap items-center gap-2">
        <Badge variant="secondary" className="text-[10px]">
          read {PUBLIC_REVIEWS_READ_ON}
        </Badge>
        <span className="text-xs text-muted-foreground">
          Posted as <code className="font-mono">{AGENTK_BOT_LOGIN}</code> on{" "}
          <code className="font-mono">{YUGABYTE_DB_REPO}</code>. The threads have moved on since.
        </span>
      </div>

      <div className="overflow-hidden rounded-lg border border-border">
        {PUBLIC_RUNS.map((run, i) => (
          <a
            key={run.pr}
            href={pullRequestUrl(run.pr)}
            target="_blank"
            rel="noreferrer noopener"
            className={cn(
              "flex items-center gap-4 px-4 py-3 transition-colors hover:bg-muted/40",
              i > 0 && "border-t border-border",
            )}
          >
            <span className="w-14 shrink-0 font-mono text-xs text-muted-foreground">#{run.pr}</span>
            <span className="min-w-0 flex-1">
              <span className="block truncate text-sm text-foreground">{run.title}</span>
              <span className="mt-0.5 block text-[11px] text-muted-foreground">
                {run.author} · {run.state}
              </span>
            </span>
            <Findings run={run} />
            <ExternalLink className="h-3.5 w-3.5 shrink-0 text-muted-foreground" aria-hidden />
          </a>
        ))}
      </div>

      <p className="text-xs text-muted-foreground">
        {TOTAL_FINDINGS} comments across these {PUBLIC_RUNS.length} pull requests, each one citing
        the rule it came from. This is the same agent, the same datapack and the same loop you just
        ran, pointed at a repository with real consequences.
      </p>
    </div>
  );
}

/** Severity counts, or the fact that there were none. "Nothing flagged" is a
 *  result the reader should be able to see, not a blank cell. */
function Findings({ run }: { run: PublicRun }) {
  if (run.major === 0 && run.minor === 0) {
    return (
      <Badge variant="success" className="shrink-0 text-[10px]">
        nothing flagged
      </Badge>
    );
  }
  return (
    <span className="flex shrink-0 items-center gap-1.5">
      {run.major > 0 && (
        <Badge variant="outline" className="border-destructive/40 text-[10px] text-destructive">
          {run.major} major
        </Badge>
      )}
      {run.minor > 0 && (
        <Badge variant="outline" className="border-warning/40 text-[10px] text-warning">
          {run.minor} minor
        </Badge>
      )}
    </span>
  );
}
