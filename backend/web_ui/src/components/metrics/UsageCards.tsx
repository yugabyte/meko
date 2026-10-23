// Meko local's own Usage section, from the call log: each tool's calls, errors and latency, the
// agents using this datapack, and the latest calls. Meko Cloud's dashboard has no such view.

import { useState } from "react";
import { CheckCircle2, ChevronRight, XCircle } from "lucide-react";
import { count, duration, timeAgo } from "../../lib/format";
import { UI_AGENT_ID } from "../../lib/api";
import { callResults } from "../../lib/calls";
import type { MekoCall, Usage } from "../../lib/types";
import { Table } from "../Table";
import { CallTraceDialog } from "./CallTraceDialog";
import { Badge } from "../ui";
import { MetricCard } from "./MetricCard";

export function ToolsCard({ usage }: { usage: Usage }) {
  const title = "Tools";
  const subtitle = `${count(usage.calls)} calls, ${count(usage.errors)} failed`;
  if (usage.tools.length === 0) {
    return <MetricCard title={title} subtitle={subtitle} empty={["No calls yet", "Calls from agents, the CLI and this UI show here."]} />;
  }
  return (
    <MetricCard title={title} subtitle={subtitle}>
      <div className="-mx-5">
        <Table
          rows={usage.tools}
          rowKey={(t) => t.tool}
          columns={[
            { label: "Tool", cell: (t) => t.tool, className: "text-xs" },
            { label: "Calls", right: true, cell: (t) => count(t.calls) },
            {
              label: "Errors",
              right: true,
              cell: (t) => <span className={t.errors > 0 ? "text-destructive" : "text-muted-foreground"}>{count(t.errors)}</span>,
            },
            { label: "p50", right: true, cell: (t) => duration(t.p50_ms) },
            { label: "p95", right: true, cell: (t) => duration(t.p95_ms) },
          ]}
        />
      </div>
      {usage.steps.length > 0 && (
        <div className="mt-4 border-t pt-3">
          <div className="mb-2 text-xs font-medium text-muted-foreground">Search time per source (p50 / p95)</div>
          <div className="flex flex-wrap gap-2 text-xs">
            {usage.steps.map((s) => (
              <span key={s.step} className="rounded-md bg-muted px-2 py-1">
                <span>{s.step}</span> {duration(s.p50_ms)} / {duration(s.p95_ms)}
              </span>
            ))}
          </div>
        </div>
      )}
    </MetricCard>
  );
}

export function AgentsCard({ usage }: { usage: Usage }) {
  const title = "Agents";
  const subtitle = "Who reads and writes this datapack";
  if (usage.agents.length === 0) {
    return <MetricCard title={title} subtitle={subtitle} empty={["No agents yet", "Connect one from the Connect tab."]} />;
  }
  return (
    <MetricCard title={title} subtitle={subtitle}>
      <div className="divide-y text-sm">
        {usage.agents.map((a) => (
          <div key={a.agent_id} className="flex items-center gap-2 py-2">
            <span className="truncate text-xs">{a.agent_id}</span>
            {a.agent_id === UI_AGENT_ID && <Badge>this UI</Badge>}
            <span className="ml-auto shrink-0 text-xs text-muted-foreground">
              {count(a.calls)} calls · {count(a.searches)} searches · {timeAgo(a.last_at)}
            </span>
          </div>
        ))}
      </div>
    </MetricCard>
  );
}

export function RecentCallsCard({ usage }: { usage: Usage }) {
  const [open, setOpen] = useState<MekoCall>();
  const title = "Recent calls";
  const subtitle = "The latest 25, newest first. Open one to see how long each step took.";
  if (usage.recent.length === 0) return <MetricCard title={title} subtitle={subtitle} empty={["No calls yet", "They show here as they happen."]} />;
  return (
    <MetricCard title={title} subtitle={subtitle}>
      <div className="-mx-5">
        <Table
          rows={usage.recent}
          rowKey={(c, i) => String(c.id ?? `${c.at}-${i}`)}
          onRow={setOpen}
          columns={[
            {
              label: "",
              cell: (c) =>
                c.ok ? (
                  <CheckCircle2 className="size-4 text-success" />
                ) : (
                  <span title={c.error_code}>
                    <XCircle className="size-4 text-destructive" />
                  </span>
                ),
            },
            { label: "Tool", cell: (c) => c.tool, className: "text-xs" },
            { label: "Agent", cell: (c) => c.agent_id || "—", className: "max-w-[12rem] truncate text-xs text-muted-foreground" },
            { label: "Results", cell: callResults, className: "text-xs text-muted-foreground" },
            { label: "Time", right: true, cell: (c) => duration(c.ms) },
            { label: "When", right: true, cell: (c) => timeAgo(c.at), className: "text-xs text-muted-foreground" },
            {
              label: "",
              right: true,
              cell: () => (
                <span className="inline-flex items-center gap-0.5 text-xs font-medium text-primary group-hover:underline">
                  Trace <ChevronRight className="size-3.5" />
                </span>
              ),
            },
          ]}
        />
      </div>
      {open && <CallTraceDialog call={open} onClose={() => setOpen(undefined)} />}
    </MetricCard>
  );
}
