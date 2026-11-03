// Every MCP call Meko answered, newest first, a page at a time, filtered by agent, tool, AgentK's own
// and failed ones; a call opens to its trace and, while Settings keeps call contents, what it sent and
// got back. Reached from Settings; the filters are in the address (/calls?agent=<agent id>).

import { useEffect, useRef, useState, type FormEvent } from "react";
import { CheckCircle2, ChevronRight, XCircle } from "lucide-react";
import { api } from "../lib/api";
import { callResults } from "../lib/calls";
import { duration, timeAgo } from "../lib/format";
import { callFilter, callsHref } from "../lib/route";
import type { CallFilter, MekoCall } from "../lib/types";
import { CallTraceDialog } from "../components/metrics/CallTraceDialog";
import { Table } from "../components/Table";
import { Button, Card, ErrorNote, Input, Spinner } from "../components/ui";

export function CallsPage() {
  const [filter, setFilter] = useState<CallFilter>(() => callFilter(window.location.search));
  const [calls, setCalls] = useState<MekoCall[]>([]);
  const [more, setMore] = useState(false);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<Error>();
  const [open, setOpen] = useState<MekoCall>();
  const [tick, setTick] = useState(0);

  // A new filter starts from the newest calls, and the address keeps it, so a reload shows the same.
  useEffect(() => {
    history.replaceState(null, "", callsHref(filter));
    let live = true;
    setLoading(true);
    api
      .calls(filter)
      .then((p) => live && (setCalls(p.calls), setMore(p.more), setError(undefined)))
      .catch((e: Error) => live && setError(e))
      .finally(() => live && setLoading(false));
    return () => {
      live = false;
    };
  }, [filter, tick]);

  // The filter of the page shown now: an older page that comes back after the filter changed is dropped.
  const shown = useRef(filter);
  shown.current = filter;
  const older = async () => {
    const last = calls.at(-1)?.id;
    if (last === undefined) return;
    const asked = filter;
    setLoading(true);
    try {
      const p = await api.calls(asked, last);
      if (shown.current !== asked) return;
      setCalls((cs) => [...cs, ...p.calls]);
      setMore(p.more);
    } catch (e) {
      if (shown.current === asked) setError(e as Error);
    } finally {
      if (shown.current === asked) setLoading(false);
    }
  };

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between gap-4">
        <div>
          <h2 className="text-lg font-semibold">Calls</h2>
          <p className="text-sm text-muted-foreground">
            Every call your agents and extensions made to Meko, newest first. Open one to see what it sent and got back.
          </p>
        </div>
        <Button onClick={() => setTick((t) => t + 1)} disabled={loading}>
          Refresh
        </Button>
      </div>
      <Filters filter={filter} onChange={setFilter} />
      {error && <ErrorNote error={error} />}
      <Card className="overflow-hidden">
        {calls.length === 0 && !loading ? (
          <p className="p-4 text-sm text-muted-foreground">No calls match.</p>
        ) : (
          <Table
            rows={calls}
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
              { label: "When", cell: (c) => timeAgo(c.at), className: "whitespace-nowrap text-xs text-muted-foreground" },
              { label: "Tool", cell: (c) => c.tool, className: "text-xs" },
              { label: "Agent", cell: (c) => c.agent_id || "—", className: "max-w-[14rem] truncate text-xs text-muted-foreground" },
              { label: "By", cell: (c) => (c.client === "agentk" ? "AgentK" : "Agent"), className: "text-xs" },
              { label: "Results", cell: callResults, className: "text-xs text-muted-foreground" },
              { label: "Took", right: true, cell: (c) => duration(c.ms) },
              {
                label: "",
                right: true,
                cell: () => (
                  <span className="inline-flex items-center gap-0.5 text-xs font-medium text-primary group-hover:underline">
                    Open <ChevronRight className="size-3.5" />
                  </span>
                ),
              },
            ]}
          />
        )}
      </Card>
      {loading && <Spinner />}
      {more && !loading && <Button onClick={() => void older()}>Load older</Button>}
      {open && <CallTraceDialog call={open} onClose={() => setOpen(undefined)} />}
    </div>
  );
}

// The agent and tool are typed and applied with Enter; the checkboxes apply at once.
function Filters({ filter, onChange }: { filter: CallFilter; onChange: (f: CallFilter) => void }) {
  const [agent, setAgent] = useState(filter.agent ?? "");
  const [tool, setTool] = useState(filter.tool ?? "");
  const submit = (e: FormEvent) => {
    e.preventDefault();
    onChange({ ...filter, agent: agent.trim() || undefined, tool: tool.trim() || undefined });
  };
  return (
    <form onSubmit={submit} className="flex flex-wrap items-center gap-3 text-sm">
      <Input
        id="calls-agent"
        aria-label="Agent"
        placeholder="Agent id"
        value={agent}
        onChange={(e) => setAgent(e.target.value)}
        className="w-64 max-w-full"
      />
      <Input
        id="calls-tool"
        aria-label="Tool"
        placeholder="Tool, such as context_search"
        value={tool}
        onChange={(e) => setTool(e.target.value)}
        className="w-56 max-w-full"
      />
      <Button type="submit">Filter</Button>
      <label className="flex items-center gap-2">
        <input
          id="calls-agentk"
          type="checkbox"
          checked={filter.client === "agentk"}
          onChange={(e) => onChange({ ...filter, client: e.target.checked ? "agentk" : undefined })}
        />
        <span>AgentK's only</span>
      </label>
      <label className="flex items-center gap-2">
        <input
          id="calls-failed"
          type="checkbox"
          checked={filter.failed ?? false}
          onChange={(e) => onChange({ ...filter, failed: e.target.checked || undefined })}
        />
        <span>Failed only</span>
      </label>
    </form>
  );
}
