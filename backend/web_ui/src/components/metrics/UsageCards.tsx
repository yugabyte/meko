// Meko local's own Usage section, from the call log: each tool's calls, errors and latency, the
// agents using this datapack, and the latest calls. Meko Cloud's dashboard has no such view.

import { CheckCircle2, XCircle } from "lucide-react";
import { count, timeAgo } from "../../lib/format";
import { UI_AGENT_ID } from "../../lib/api";
import type { Usage } from "../../lib/types";
import { Badge } from "../ui";
import { MetricCard } from "./MetricCard";

const th = "px-3 py-2 text-left text-xs font-medium text-muted-foreground";
const td = "px-3 py-2";
const ms = (n: number) => (n >= 1000 ? `${(n / 1000).toFixed(1)} s` : `${n} ms`);

export function ToolsCard({ usage }: { usage: Usage }) {
  const title = "Tools";
  const subtitle = `${count(usage.calls)} calls, ${count(usage.errors)} failed · latency over 90 days`;
  if (usage.tools.length === 0) {
    return <MetricCard title={title} subtitle={subtitle} empty={["No calls yet", "Calls from agents, the CLI and this UI show here."]} />;
  }
  return (
    <MetricCard title={title} subtitle={subtitle}>
      <div className="-mx-5 overflow-x-auto">
        <table className="w-full text-sm">
          <thead>
            <tr>
              <th className={th}>Tool</th>
              <th className={`${th} text-right`}>Calls</th>
              <th className={`${th} text-right`}>Errors</th>
              <th className={`${th} text-right`}>p50</th>
              <th className={`${th} text-right`}>p95</th>
            </tr>
          </thead>
          <tbody className="divide-y">
            {usage.tools.map((t) => (
              <tr key={t.tool}>
                <td className={`${td} font-mono text-xs`}>{t.tool}</td>
                <td className={`${td} text-right tabular-nums`}>{count(t.calls)}</td>
                <td className={`${td} text-right tabular-nums ${t.errors > 0 ? "text-destructive" : "text-muted-foreground"}`}>
                  {count(t.errors)}
                </td>
                <td className={`${td} text-right tabular-nums`}>{ms(t.p50_ms)}</td>
                <td className={`${td} text-right tabular-nums`}>{ms(t.p95_ms)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {usage.steps.length > 0 && (
        <div className="mt-4 border-t pt-3">
          <div className="mb-2 text-xs font-medium text-muted-foreground">Search time per source (p50 / p95)</div>
          <div className="flex flex-wrap gap-2 text-xs">
            {usage.steps.map((s) => (
              <span key={s.step} className="rounded-md bg-muted px-2 py-1">
                <span className="font-mono">{s.step}</span> {ms(s.p50_ms)} / {ms(s.p95_ms)}
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
            <span className="truncate font-mono text-xs">{a.agent_id}</span>
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
  const title = "Recent calls";
  const subtitle = "The latest 25, newest first";
  if (usage.recent.length === 0) return <MetricCard title={title} subtitle={subtitle} empty={["No calls yet", "They show here as they happen."]} />;
  return (
    <MetricCard title={title} subtitle={subtitle}>
      <div className="-mx-5 overflow-x-auto">
        <table className="w-full text-sm">
          <thead>
            <tr>
              <th className={th} />
              <th className={th}>Tool</th>
              <th className={th}>Agent</th>
              <th className={th}>Results</th>
              <th className={`${th} text-right`}>Time</th>
              <th className={`${th} text-right`}>When</th>
            </tr>
          </thead>
          <tbody className="divide-y">
            {usage.recent.map((c, i) => {
              const results = c.memory_n + c.kb_n + c.conversation_n;
              return (
                <tr key={c.at + i}>
                  <td className={td} title={c.error_code || "ok"}>
                    {c.ok ? <CheckCircle2 className="size-4 text-success" /> : <XCircle className="size-4 text-destructive" />}
                  </td>
                  <td className={`${td} font-mono text-xs`}>{c.tool}</td>
                  <td className={`${td} max-w-[12rem] truncate font-mono text-xs text-muted-foreground`}>{c.agent_id || "—"}</td>
                  <td className={`${td} text-xs text-muted-foreground`}>
                    {c.ok ? (results > 0 ? `${c.memory_n} mem · ${c.kb_n} kb · ${c.conversation_n} conv` : "—") : c.error_code}
                  </td>
                  <td className={`${td} text-right tabular-nums`}>{ms(c.ms)}</td>
                  <td className={`${td} text-right text-xs text-muted-foreground`}>{timeAgo(c.at)}</td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
    </MetricCard>
  );
}
