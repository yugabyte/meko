// The sync timeline worked out from a run's phases: which step is under way, failed or done, how
// long each took, and the upload's pace.

import { expect, test } from "vitest";
import { clock } from "../src/lib/format";
import { SYNC_STEPS, syncElapsed, syncTimeline, uploadPace } from "../src/lib/syncTimeline";
import type { SyncRun } from "../src/lib/types";

const T0 = Date.parse("2026-11-05T10:00:00Z");
const at = (s: number) => new Date(T0 + s * 1000).toISOString();
const base: SyncRun = { datapack_id: "d", datapack_name: "invoices", state: "syncing", step: "importing",
  started_at: at(0) };
const status = (r: SyncRun, now: number) => Object.fromEntries(syncTimeline(r, now).map((s) => [s.name, s.status]));

test("steps before the latest are done, the latest is under way, the rest to come", () => {
  const run: SyncRun = { ...base, phases: [
    { name: "exporting", started_at: at(0), ended_at: at(2) },
    { name: "uploading", started_at: at(2), ended_at: at(10) },
    { name: "importing", started_at: at(10), ended_at: at(11) },
    { name: "knowledge_files", started_at: at(11) },
  ] };
  const steps = syncTimeline(run, T0 + 20_000);
  const by = Object.fromEntries(steps.map((s) => [s.name, s]));
  expect(by.exporting).toMatchObject({ status: "done", ms: 2000 });
  // Meko Cloud's quick steps between two polls are done, with no time of their own.
  expect(by.creating).toMatchObject({ status: "done", ms: undefined });
  expect(by.knowledge_files).toMatchObject({ status: "active", ms: 9000 });
  expect(by.artifacts.status).toBe("pending");
  expect(by.finishing.status).toBe("pending");
  expect(steps.map((s) => s.name)).toEqual(SYNC_STEPS.map((s) => s.name));
});

test("a failed run failed in its last phase; a done one is done throughout", () => {
  const failed: SyncRun = { ...base, state: "failed", ended_at: at(5), phases: [
    { name: "exporting", started_at: at(0), ended_at: at(1) },
    { name: "uploading", started_at: at(1), ended_at: at(5) },
  ] };
  expect(status(failed, T0)).toMatchObject({ exporting: "done", uploading: "failed", importing: "pending" });
  expect(syncElapsed(failed, T0 + 99_000)).toBe(5000);
  const done: SyncRun = { ...failed, state: "done" };
  expect(Object.values(status(done, T0)).every((s) => s === "done")).toBe(true);
});

test("an unknown phase from a newer Meko Cloud is left out, not shown as a step", () => {
  const run: SyncRun = { ...base, phases: [
    { name: "importing", started_at: at(0) },
    { name: "something_new", started_at: at(1) },
  ] };
  expect(status(run, T0).importing).toBe("active");
});

test("the upload's pace and time left", () => {
  const run: SyncRun = { ...base, step: "uploading", upload: { done: 4_000_000, total: 10_000_000 },
    phases: [{ name: "uploading", started_at: at(0) }] };
  const pace = uploadPace(run, T0 + 2000)!;
  expect(pace.fraction).toBe(0.4);
  expect(pace.rate).toBe(2_000_000);
  expect(pace.leftMs).toBe(3000);
  expect(uploadPace({ ...run, upload: undefined }, T0)).toBeUndefined();
});

test("clock reads like a clock", () => {
  expect(clock(8_400)).toBe("8s");
  expect(clock(125_000)).toBe("2m 05s");
  expect(clock(3_780_000)).toBe("1h 03m");
});
