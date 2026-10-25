// How this Meko is doing: the server, the memory engine with its embedder and reranker, and the
// child processes the server supervises. Refreshes every few seconds.

import { useEffect } from "react";
import { api, ApiError } from "../lib/api";
import { dateTime } from "../lib/format";
import type { ServerStatus } from "../lib/types";
import { useLoad } from "../lib/useLoad";
import { KeyValues } from "../components/KeyValues";
import { UpdatesCard } from "../components/UpdatesCard";
import { MetricCard } from "../components/metrics/MetricCard";
import { SectionHeader } from "../components/metrics/SectionHeader";
import { Badge, ErrorNote, Loading } from "../components/ui";

const REFRESH_MS = 5000;

// A server older than this page has no /api/v1/server.
const OLDER_SERVER =
  "This Meko server is older than the web UI, so it can't report its status. Restart it with meko restart, then reload.";

function uptime(s: number): string {
  const d = Math.floor(s / 86400);
  const h = Math.floor((s % 86400) / 3600);
  const m = Math.floor((s % 3600) / 60);
  return d > 0 ? `${d}d ${h}h` : h > 0 ? `${h}h ${m}m` : `${m}m ${s % 60}s`;
}

function State({ state }: { state: string }) {
  const tone =
    state === "ready" || state === "running"
      ? "success"
      : ["error", "failed", "unavailable"].includes(state)
        ? "danger"
        : "warning";
  return <Badge tone={tone}>{state}</Badge>;
}

export function StatusPage() {
  const status = useLoad(() => api.server(), []);
  const { reload } = status;
  useEffect(() => {
    const t = setInterval(reload, REFRESH_MS);
    return () => clearInterval(t);
  }, [reload]);

  if (status.loading && !status.data) return <Loading />;
  if (status.error instanceof ApiError && status.error.status === 404) {
    return <ErrorNote error={new Error(OLDER_SERVER)} />;
  }
  const s: ServerStatus | undefined = status.data;
  // Once loaded, a failed refresh (as while Meko restarts into an update) keeps the page, and the
  // Updates card waiting for Meko, on screen.
  if (!s) return status.error ? <ErrorNote error={status.error} /> : null;
  const e = s.memory_engine;

  return (
    <div className="max-w-4xl">
      <h1 className="mb-6 text-xl font-semibold">Status</h1>
      <SectionHeader label="This Meko" onRefresh={reload} refreshing={status.loading} />
      {status.error && (
        <div className="mb-4">
          <ErrorNote error={new Error(`Meko isn't answering: ${status.error.message}`)} />
        </div>
      )}
      <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
        <MetricCard title="Server">
          <KeyValues
            rows={[
              ["Version", s.version],
              ["Uptime", `${uptime(s.uptime_seconds)} (since ${dateTime(s.started_at)})`],
              ["Address", `127.0.0.1:${s.port}`],
              ["Data folder", s.data_dir],
              ["Auth", s.auth === "none" ? <Badge tone="warning">off</Badge> : "token"],
              ["PID", s.pid],
            ]}
          />
        </MetricCard>
        <UpdatesCard />
        <MetricCard title="Memory engine">
          <KeyValues
            rows={[
              ["State", <State state={e.state} />],
              ...(e.reason ? [["Why", e.reason] as [string, string]] : []),
              ["Embedder", e.embedder ? `${e.embedder.model} (${e.embedder.dims} dims)` : "—"],
              [
                "Reranker",
                e.reranker ? (
                  <span className="flex flex-wrap items-center gap-2">
                    <State state={e.reranker.state} /> {e.reranker.model}
                  </span>
                ) : (
                  "—"
                ),
              ],
            ]}
          />
        </MetricCard>
      </div>
      <SectionHeader label="Processes" />
      <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
        {s.children.map((c) => (
          <MetricCard key={c.name} title={c.name}>
            <KeyValues
              rows={[
                ["State", <State state={c.state} />],
                ["Restarts", c.restarts],
                ["Since", dateTime(c.since)],
                ...(c.last_error ? [["Last error", c.last_error] as [string, string]] : []),
              ]}
            />
          </MetricCard>
        ))}
      </div>
    </div>
  );
}
