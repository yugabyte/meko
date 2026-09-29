// Chapter 4's opener: the next six pull requests going past the agent with
// nobody watching, and the one that comes back.
//
// The lab's own review was a single pull request, which cannot show the thing
// this chapter is about. A batch can: five close without a human, one stops,
// and the one that stops is the destructive migration. The rows are a
// fixture (batchRun.ts) and the page says so.

import { useEffect, useState } from "react";
import { Loader2, Play } from "lucide-react";

import { cn } from "@/lib/utils";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { WarningBanner } from "@/components/ui/warning-banner";

import {
  BATCH_BLOCKING_ROW,
  BATCH_CAME_BACK,
  BATCH_CLOSED_WITHOUT_YOU,
  BATCH_COST_USD,
  BATCH_DURATION,
  BATCH_MODEL_LABEL,
  BATCH_ROWS,
  type BatchOutcome,
} from "../../data/adlcReviewAgentLab/batchRun";
import { LabStageHeader } from "./LabStageHeader";

/** Gap between rows landing. Long enough to read as a queue being worked
 *  through, short enough that nobody waits for the summary. */
const ROW_INTERVAL_MS = 550;

function outcomeLabel(outcome: BatchOutcome): string {
  if (outcome.kind === "auto-approved") return "auto-approved";
  if (outcome.kind === "blocking") return "blocking · waiting on you";
  return `${outcome.comments} comment${outcome.comments === 1 ? "" : "s"} · posted`;
}

export function LetItRunStage() {
  /** How many rows have their outcome. The run walks this up to the end. */
  const [shown, setShown] = useState(0);
  const [running, setRunning] = useState(false);

  // The timer id is a local, not a ref: the cleanup then closes over the
  // exact timer this pass created, so it can never clear a later one. React
  // runs this cleanup on unmount too, so no separate teardown is needed.
  useEffect(() => {
    if (!running) return;
    if (shown >= BATCH_ROWS.length) {
      setRunning(false);
      return;
    }
    const id = window.setTimeout(() => setShown((n) => n + 1), ROW_INTERVAL_MS);
    return () => window.clearTimeout(id);
  }, [running, shown]);

  const done = shown >= BATCH_ROWS.length;

  return (
    <div className="flex flex-1 flex-col gap-6 overflow-y-auto p-6">
      <LabStageHeader
        eyebrow="Chapter 4 · Step 1"
        title="Now let it run"
        lede="The context is built and the guardrail is set. This is what those four minutes bought: the next batch goes through without you, until it needs you."
        tags={["Velocity"]}
      />

      {/* The queue is visible from the start. Hiding it until the run left an
          empty box on screen, which reads as a broken page rather than as
          work waiting to happen. Only the OUTCOMES arrive one at a time. */}
      <div className="overflow-hidden rounded-lg border border-border">
        {BATCH_ROWS.map((row, i) => {
          const reviewed = i < shown;
          const inFlight = running && i === shown;
          return (
            <div
              key={row.pr}
              className={cn(
                "flex items-center gap-4 px-4 py-3",
                i > 0 && "border-t border-border",
                !reviewed && "text-muted-foreground",
              )}
            >
              <span className="w-12 shrink-0 font-mono text-xs text-muted-foreground">
                #{row.pr}
              </span>
              <span
                className={cn(
                  "min-w-0 flex-1 truncate text-sm",
                  reviewed ? "text-foreground" : "text-muted-foreground",
                )}
              >
                {row.title}
              </span>
              {reviewed ? (
                <Badge
                  variant={
                    row.outcome.kind === "auto-approved"
                      ? "success"
                      : row.outcome.kind === "blocking"
                        ? "destructive"
                        : "outline"
                  }
                  className={cn(
                    "shrink-0 text-[10px]",
                    row.outcome.kind === "posted" && "border-warning/40 text-warning",
                  )}
                >
                  {outcomeLabel(row.outcome)}
                </Badge>
              ) : inFlight ? (
                <span className="flex shrink-0 items-center gap-1.5 text-[10px] text-muted-foreground">
                  <Loader2 className="h-3 w-3 animate-spin" aria-hidden />
                  reviewing
                </span>
              ) : (
                <span className="shrink-0 text-[10px] text-muted-foreground/70">queued</span>
              )}
            </div>
          );
        })}
      </div>

      <Button
        busy={running}
        busyLabel="Reviewing the batch"
        done={done}
        doneLabel="Batch complete"
        onClick={() => setRunning(true)}
        className="w-full"
      >
        <Play aria-hidden />
        Run the batch
      </Button>

      {done && (
        <>
          <div className="grid grid-cols-2 gap-4 rounded-lg border border-border bg-card p-4 sm:grid-cols-4">
            <Stat
              value={String(BATCH_CLOSED_WITHOUT_YOU)}
              suffix={`/${BATCH_ROWS.length}`}
              label="closed without you"
            />
            <Stat value={String(BATCH_CAME_BACK)} label="came back for a human" />
            <Stat value={BATCH_DURATION} label="for the whole batch" />
            <Stat
              value={`$${BATCH_COST_USD.toFixed(2)}`}
              label={`total, on an ${BATCH_MODEL_LABEL}`}
            />
          </div>

          <WarningBanner tone="success">
            <b className="font-semibold">This is the shape of the win.</b> You did not review six
            pull requests. You built context once, and reviewed the one decision that actually
            needed judgment.{" "}
            {BATCH_BLOCKING_ROW && (
              <>
                #{BATCH_BLOCKING_ROW.pr} is waiting because dropping a table is not a call an agent
                should make alone.
              </>
            )}
          </WarningBanner>
        </>
      )}

      <p className="text-xs text-muted-foreground">
        These six are an illustration, not a run against your repository. The next lesson is
        Yugabyte's real deployment, with its own numbers.
      </p>
    </div>
  );
}

/** One figure from the batch. The suffix rides small next to the value so
 *  "5/6" reads as one number rather than two. */
function Stat({ value, suffix, label }: { value: string; suffix?: string; label: string }) {
  return (
    <div>
      <p className="text-2xl font-semibold tabular-nums text-foreground">
        {value}
        {suffix && <span className="ml-0.5 text-sm text-muted-foreground">{suffix}</span>}
      </p>
      <p className="mt-0.5 text-xs leading-tight text-muted-foreground">{label}</p>
    </div>
  );
}
