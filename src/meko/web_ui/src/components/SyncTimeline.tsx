// The live timeline of a datapack's sync to Meko Cloud: every step in order, done, under way, failed
// or to come, with each one's time, the upload's progress and pace, and the time the sync has taken.

import { Check, Cloud, Laptop, X } from "lucide-react";
import { bytes, clock } from "../lib/format";
import { syncElapsed, syncTimeline, uploadPace, type TimelineStep } from "../lib/syncTimeline";
import type { SyncRun } from "../lib/types";
import { Spinner, cx } from "./ui";

export function SyncTimeline({ run, now }: { run: SyncRun; now: number }) {
  const steps = syncTimeline(run, now);
  const done = steps.filter((s) => s.status === "done").length;
  const at = steps.findIndex((s) => s.status === "active" || s.status === "failed");
  const pace = uploadPace(run, now);
  const firstCloud = steps.findIndex((s) => s.cloud);
  return (
    <div className="space-y-3">
      <div className="space-y-1.5">
        <div className="flex items-baseline justify-between text-xs text-muted-foreground">
          <span>
            {run.state === "done"
              ? "All steps done"
              : run.state === "failed"
                ? `Stopped at step ${at + 1} of ${steps.length}`
                : `Step ${at + 1} of ${steps.length}`}
          </span>
          <span className="tabular-nums">{clock(syncElapsed(run, now))}</span>
        </div>
        <Bar fraction={done / steps.length} tone={run.state === "failed" ? "failed" : "ok"} />
      </div>
      <ol aria-label="Sync steps">
        {steps.map((s, i) => (
          <li key={s.name}>
            {(i === 0 || i === firstCloud || (i > firstCloud && !s.cloud && steps[i - 1].cloud)) && (
              <Where cloud={s.cloud} />
            )}
            <Row step={s} last={i === steps.length - 1} detail={s.name === "uploading" ? pace : undefined} />
          </li>
        ))}
      </ol>
    </div>
  );
}

function Where({ cloud }: { cloud: boolean }) {
  const Icon = cloud ? Cloud : Laptop;
  return (
    <p className="flex items-center gap-1.5 pb-1 pt-2 text-[11px] font-medium uppercase tracking-wide text-muted-foreground first:pt-0">
      <Icon className="size-3" /> {cloud ? "In Meko Cloud" : "On this computer"}
    </p>
  );
}

function Row({ step, last, detail }: { step: TimelineStep; last: boolean; detail?: ReturnType<typeof uploadPace> }) {
  const { status } = step;
  return (
    <div className="flex gap-3" aria-current={status === "active" ? "step" : undefined} data-status={status}>
      <div className="flex flex-col items-center">
        <Node status={status} />
        {!last && <div className={cx("my-0.5 w-px flex-1", status === "done" ? "bg-success/60" : "bg-border")} />}
      </div>
      <div className={cx("min-w-0 flex-1 pb-3", status === "pending" && "text-muted-foreground")}>
        <div className="flex items-baseline justify-between gap-2 text-sm">
          <span className={cx(status === "active" && "font-medium", status === "failed" && "text-destructive")}>
            {step.label}
          </span>
          {step.ms !== undefined && status !== "pending" && (
            <span className="shrink-0 text-xs tabular-nums text-muted-foreground">{clock(step.ms)}</span>
          )}
        </div>
        {detail && status !== "pending" && (
          <div className="mt-1.5 space-y-1">
            <Bar fraction={detail.fraction} tone="ok" />
            <p className="text-xs tabular-nums text-muted-foreground">
              {bytes(detail.done)} of {bytes(detail.total)}
              {status === "active" && detail.rate > 0 && ` · ${bytes(Math.round(detail.rate))}/s`}
              {status === "active" && detail.leftMs !== undefined && ` · about ${clock(detail.leftMs)} left`}
            </p>
          </div>
        )}
      </div>
    </div>
  );
}

function Node({ status }: { status: TimelineStep["status"] }) {
  const base = "flex size-5 shrink-0 items-center justify-center rounded-full";
  switch (status) {
    case "done":
      return (
        <span className={cx(base, "bg-success text-white")}>
          <Check className="size-3" strokeWidth={3} aria-label="done" />
        </span>
      );
    case "active":
      return (
        <span className={cx(base, "ring-2 ring-primary/40")}>
          <Spinner className="size-3.5 text-primary" />
        </span>
      );
    case "failed":
      return (
        <span className={cx(base, "bg-destructive text-white")}>
          <X className="size-3" strokeWidth={3} aria-label="failed" />
        </span>
      );
    default:
      return <span className={cx(base, "border-2 border-border")} />;
  }
}

function Bar({ fraction, tone }: { fraction: number; tone: "ok" | "failed" }) {
  return (
    <div
      className="h-1.5 overflow-hidden rounded-full bg-muted"
      role="progressbar"
      aria-valuemin={0}
      aria-valuemax={100}
      aria-valuenow={Math.round(fraction * 100)}
    >
      <div
        className={cx("h-full rounded-full transition-[width] duration-500", tone === "failed" ? "bg-destructive" : "bg-primary")}
        style={{ width: `${Math.round(fraction * 100)}%` }}
      />
    </div>
  );
}
