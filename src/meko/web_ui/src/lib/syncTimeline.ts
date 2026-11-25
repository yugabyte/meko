// The Sync to Meko Cloud timeline, worked out from a sync's phases: every step it goes through, in
// order, each done, under way, failed or still to come, with how long it took and the upload's pace.

import type { SyncRun } from "./types";

/** Every phase a sync goes through, in order: this Meko's, then Meko Cloud's import steps. */
export const SYNC_STEPS: { name: string; label: string; cloud?: boolean }[] = [
  { name: "exporting", label: "Pack it up" },
  { name: "uploading", label: "Upload it to Meko Cloud" },
  { name: "importing", label: "Wait for Meko Cloud to pick it up", cloud: true },
  { name: "reading", label: "Check the export", cloud: true },
  { name: "creating", label: "Create the datapack", cloud: true },
  { name: "memories_and_conversations", label: "Add memories and conversations", cloud: true },
  { name: "knowledge_files", label: "Index knowledge files", cloud: true },
  { name: "artifacts", label: "Store artifacts", cloud: true },
  { name: "finishing", label: "Remove it from this computer" },
];

export type StepStatus = "done" | "active" | "failed" | "pending";

export interface TimelineStep {
  name: string;
  label: string;
  cloud: boolean;
  status: StepStatus;
  /** How long it took, or has taken so far; undefined for a step too quick to be seen. */
  ms?: number;
}

const ms = (iso: string | undefined, now: number) => (iso ? new Date(iso).getTime() : now);

/**
 * The timeline at now. Steps before the latest phase are done (a Meko Cloud step can come and go
 * between two looks, so it may have no time of its own); the latest is under way, failed, or done.
 */
export function syncTimeline(run: SyncRun, now: number): TimelineStep[] {
  const phases = (run.phases ?? []).filter((p) => SYNC_STEPS.some((s) => s.name === p.name));
  const last = phases.at(-1);
  const current = last ? SYNC_STEPS.findIndex((s) => s.name === last.name) : -1;
  return SYNC_STEPS.map((s, i) => {
    const p = phases.find((ph) => ph.name === s.name);
    let status: StepStatus = "pending";
    if (run.state === "done" || i < current) status = "done";
    else if (i === current) status = run.state === "failed" ? "failed" : "active";
    const took = p ? ms(p.ended_at, now) - ms(p.started_at, now) : undefined;
    return { name: s.name, label: s.label, cloud: !!s.cloud, status, ms: took };
  });
}

/** How long the sync has run, or ran. */
export function syncElapsed(run: SyncRun, now: number): number {
  return run.started_at ? ms(run.ended_at, now) - ms(run.started_at, now) : 0;
}

export interface UploadPace {
  done: number;
  total: number;
  /** 0 to 1. */
  fraction: number;
  /** Bytes a second so far; 0 before there's enough to tell. */
  rate: number;
  /** Time left at that pace; undefined until it's known. */
  leftMs?: number;
}

/** The upload's progress and pace at now, once it has started. */
export function uploadPace(run: SyncRun, now: number): UploadPace | undefined {
  const up = run.upload;
  const phase = run.phases?.find((p) => p.name === "uploading");
  if (!up || !up.total || !phase) return undefined;
  const secs = (ms(phase.ended_at, now) - ms(phase.started_at, now)) / 1000;
  const rate = secs >= 0.5 ? up.done / secs : 0;
  const left = up.total - up.done;
  return {
    done: up.done,
    total: up.total,
    fraction: Math.min(1, up.done / up.total),
    rate,
    leftMs: rate > 0 && left > 0 ? (left / rate) * 1000 : undefined,
  };
}
