// The Status page's Updates card: the running version, the newest release the server found and when
// it looked, Check now, and Update, which installs the release and restarts Meko into it. While
// Meko restarts the card waits for it to answer again and reloads the page into the new version;
// after the reload it says how the update went.

import { useEffect, useRef, useState } from "react";
import { ExternalLink } from "lucide-react";
import { api, ApiError } from "../lib/api";
import { timeAgo } from "../lib/format";
import {
  clearTarget,
  notifyUpdateChanged,
  readTarget,
  saveTarget,
  useUpdateStatus,
  type UpdateTarget,
} from "../lib/update";
import type { UpdateStatus } from "../lib/types";
import { KeyValues } from "./KeyValues";
import { MetricCard } from "./metrics/MetricCard";
import { Badge, Button, ErrorNote, Spinner } from "./ui";

// How often the card follows an install, and Meko's restart.
const POLL_MS = 2000;
// How long a restart may take before the card says Meko hasn't come back.
const GIVE_UP_MS = 3 * 60_000;

function installs(s: UpdateStatus): string {
  if (s.auto) return "On their own";
  if (s.service) return "With Update ([update] auto is off)";
  return "With Update (Meko runs without its service)";
}

// What the update asked for before the reload came to.
function outcome(t: UpdateTarget, s: UpdateStatus): { tone: "success" | "warning"; text: string } {
  if (s.running === t.to) return { tone: "success", text: `Updated to ${t.to} (from ${t.from}).` };
  if (s.rolled_back.includes(t.to)) {
    return {
      tone: "warning",
      text: `${t.to} failed to start and was rolled back; ${s.running} is running. See meko logs.`,
    };
  }
  return { tone: "warning", text: `The update to ${t.to} didn't start; ${s.running} is running. See meko logs.` };
}

// waitForRestart waits for Meko to answer again after the restart into to, then reloads the page.
// It reloads once the version changed, or once Meko answers after it stopped answering (as after a
// rollback, which brings back the old version); it gives up after GIVE_UP_MS.
function useRestartWait(target: UpdateTarget | null, onLost: () => void) {
  useEffect(() => {
    if (!target) return;
    const started = Date.now();
    let away = false;
    const t = setInterval(async () => {
      try {
        const h = await api.health();
        if (h.version !== target.from || away) {
          clearInterval(t);
          window.location.reload();
          return;
        }
      } catch {
        away = true;
      }
      if (Date.now() - started > GIVE_UP_MS) {
        clearInterval(t);
        onLost();
      }
    }, POLL_MS);
    return () => clearInterval(t);
  }, [target, onLost]);
}

export function UpdatesCard() {
  const status = useUpdateStatus();
  const { reload } = status;
  const s = status.data;
  const job = s?.job;
  // What this tab asked for before the reload: said once, then forgotten.
  const [asked] = useState(readTarget);
  useEffect(clearTarget, []);
  const [restarting, setRestarting] = useState<UpdateTarget | null>(null);
  const [lost, setLost] = useState(false);
  const [busy, setBusy] = useState<"check" | "apply" | null>(null);
  const [error, setError] = useState<Error>();

  const jobRunning = job?.state === "running";
  // Follow an install until it ends or Meko restarts.
  useEffect(() => {
    if (!jobRunning || restarting) return;
    const t = setInterval(reload, POLL_MS);
    return () => clearInterval(t);
  }, [jobRunning, restarting, reload]);
  // The server restarts as soon as the new version is current, so a poll rarely sees the
  // restarting step: a poll that fails while the job installs a version is the restart too.
  const from = s?.running;
  const step = jobRunning ? job?.step : undefined;
  const to = job?.to;
  const unreachable = !!status.error;
  useEffect(() => {
    if (restarting || !from || !to) return;
    if (step === "restarting" || (step === "installing" && unreachable)) {
      const t = { from, to };
      saveTarget(t);
      setRestarting(t);
    }
  }, [from, step, to, unreachable, restarting]);
  // The sidebar's chip follows what the card shows, once the install ends.
  const wasRunning = useRef(false);
  useEffect(() => {
    if (wasRunning.current && !jobRunning) notifyUpdateChanged();
    wasRunning.current = jobRunning;
  }, [jobRunning]);
  const [onLost] = useState(() => () => setLost(true));
  useRestartWait(restarting, onLost);

  const act = async (what: "check" | "apply") => {
    setBusy(what);
    setError(undefined);
    try {
      await (what === "check" ? api.checkUpdate() : api.applyUpdate());
      notifyUpdateChanged();
    } catch (e) {
      setError(e as Error);
    } finally {
      setBusy(null);
      reload();
    }
  };

  if (status.error instanceof ApiError && status.error.status === 404) {
    return (
      <MetricCard title="Updates">
        <p className="text-sm text-muted-foreground">This Meko server is older than the web UI. Restart it with meko restart.</p>
      </MetricCard>
    );
  }
  if (!s) {
    return (
      <MetricCard title="Updates">{status.error ? <ErrorNote error={status.error} /> : <Spinner />}</MetricCard>
    );
  }
  if (!s.release) {
    return (
      <MetricCard title="Updates">
        <p className="text-sm text-muted-foreground">
          This Meko was built from source, so it doesn't update itself: rebuild it, or install a release.
        </p>
      </MetricCard>
    );
  }

  if (s.off) {
    return (
      <MetricCard title="Updates">
        <p className="text-sm text-muted-foreground">
          Meko {s.running} can't update itself: {s.off}. See meko logs.
        </p>
      </MetricCard>
    );
  }

  const done = asked && outcome(asked, s);
  const working = jobRunning || !!restarting || busy === "apply";
  const canUpdate = s.available && !s.pending && !working && !!s.latest;
  return (
    <MetricCard title="Updates">
      <div className="space-y-3">
        <KeyValues
          rows={[
            ["Running", s.running],
            [
              "Newest",
              s.latest ? (
                <span className="flex flex-wrap items-center gap-2">
                  {s.latest}
                  {s.available && <Badge tone="primary">available</Badge>}
                  {s.latest_url && (
                    <a
                      href={s.latest_url}
                      target="_blank"
                      rel="noreferrer"
                      className="inline-flex items-center gap-1 text-xs text-primary hover:underline"
                    >
                      Release notes <ExternalLink className="size-3" />
                    </a>
                  )}
                </span>
              ) : (
                "—"
              ),
            ],
            ["Last checked", s.checked_at ? timeAgo(s.checked_at) : "Not yet"],
            ["Installs", installs(s)],
            ...(s.channel === "off" ? [["Checks", 'Off ([update] channel = "off")'] as [string, string]] : []),
          ]}
        />
        {done && !working && (
          <p className={done.tone === "success" ? "text-sm text-success" : "text-sm text-warning"}>{done.text}</p>
        )}
        {s.error && !working && <p className="text-sm text-destructive">The last check failed: {s.error}</p>}
        {s.pending && (
          <p className="text-sm text-muted-foreground">
            Updated from {s.pending.from}. It counts as installed once {s.pending.to} has run 5 minutes (start{" "}
            {s.pending.attempts} of 3).
          </p>
        )}
        {lost ? (
          <p className="text-sm text-warning">
            Meko hasn't answered for 3 minutes since it stopped to restart into {restarting?.to}. Run meko status and
            meko logs on its computer.
          </p>
        ) : restarting ? (
          <p className="flex items-center gap-2 text-sm">
            <Spinner /> Restarting into {restarting.to}. This page reloads when Meko is back.
          </p>
        ) : jobRunning ? (
          <p className="flex items-center gap-2 text-sm">
            <Spinner />
            {job?.step === "installing" ? `Installing ${job.to}` : "Looking for the newest release"}
          </p>
        ) : job?.state === "failed" ? (
          <p className="text-sm text-destructive">
            The update{job.to ? ` to ${job.to}` : ""} failed: {job.error}. This version keeps running.
          </p>
        ) : job?.state === "done" && !job.to ? (
          <p className="text-sm text-muted-foreground">Nothing newer to install.</p>
        ) : null}
        {canUpdate && (
          <p className="text-xs text-muted-foreground">
            Updating restarts Meko. Its pages, and the extensions' pages, reconnect after a few seconds; the extensions
            keep running.
            {!s.service &&
              ` Meko runs without its service, so if ${s.latest} can't start, nothing starts it again until meko start.`}
          </p>
        )}
        {error && <ErrorNote error={error} />}
        <div className="flex flex-wrap gap-2">
          {canUpdate && (
            <Button id="update-apply" variant="primary" size="sm" onClick={() => act("apply")}>
              Update to {s.latest}
            </Button>
          )}
          <Button id="update-check" size="sm" disabled={working || busy === "check"} onClick={() => act("check")}>
            {busy === "check" ? "Checking…" : "Check now"}
          </Button>
        </div>
      </div>
    </MetricCard>
  );
}
