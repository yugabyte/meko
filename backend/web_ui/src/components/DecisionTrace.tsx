// A turn's decision trace, collapsed to one summary line: what the agent read from Meko, why it
// answered so and the steps it took (its own reasoning and plan), and what it wrote. Each call opens
// its step timings, and the memories its searches returned light up on a small memory map. Meko
// builds it from the call log.

import { useState, type ReactNode } from "react";
import { ChevronDown, ChevronRight, Search, Sparkles, Wrench } from "lucide-react";
import { api } from "../lib/api";
import { CALL_RETENTION_DAYS, callResults, isRead, resultCount } from "../lib/calls";
import { duration } from "../lib/format";
import type { MekoCall, Turn } from "../lib/types";
import { useLoad } from "../lib/useLoad";
import { MemoryMapView, only, SHOWN_STATES } from "./MemoryMapView";
import { CallTraceDialog } from "./metrics/CallTraceDialog";
import { Badge, cx } from "./ui";

export function DecisionTrace({ datapackId, turn }: { datapackId: string; turn: Turn }) {
  const [open, setOpen] = useState(false);
  const [call, setCall] = useState<MekoCall>();
  const calls = turn.calls ?? [];
  const plan = turn.plan ?? [];
  const reads = calls.filter(isRead);
  const actions = calls.filter((c) => !isRead(c));
  // The call log keeps calls for 90 days; turns and their reasoning stay.
  const pruned = Date.now() - new Date(turn.started_at).getTime() > CALL_RETENTION_DAYS * 86_400_000;
  if (calls.length === 0 && !turn.reasoning && plan.length === 0 && !pruned) return null;

  const found = reads.reduce((n, c) => n + resultCount(c), 0);
  const saw = new Set<string>(
    reads.flatMap((c) => (c.steps ?? []).flatMap((s) => (s.step === "memory_search" ? (s.ids ?? []) : []))),
  );
  const ms = calls.reduce((n, c) => n + c.ms, 0);
  const summary = [
    reads.length > 0 && `${reads.length} ${reads.length === 1 ? "lookup" : "lookups"} · ${found} found`,
    actions.length > 0 && `${actions.length} ${actions.length === 1 ? "action" : "actions"}`,
    (turn.reasoning || plan.length > 0) && "reasoning",
    calls.length > 0 && duration(ms),
  ]
    .filter(Boolean)
    .join(" · ");

  return (
    <div className="rounded-lg border">
      <button
        type="button"
        onClick={() => setOpen(!open)}
        className="flex w-full items-center gap-2 px-3 py-2 text-left text-xs hover:bg-accent"
      >
        {open ? <ChevronDown className="size-3.5" /> : <ChevronRight className="size-3.5" />}
        <span className="font-medium text-primary">Decision trace</span>
        <span className="text-muted-foreground">{summary}</span>
      </button>
      {open && (
        <div className="space-y-3 border-t px-3 py-3">
          {pruned && (
            <p className="text-xs text-muted-foreground">
              Meko keeps calls for {CALL_RETENTION_DAYS} days, so this turn's lookups and actions may be gone.
            </p>
          )}
          <Phase icon={<Search className="size-3.5" />} title="Looked up" empty="Nothing read from Meko before this turn.">
            {reads.length > 0 && <Calls calls={reads} onOpen={setCall} />}
            {saw.size > 0 && <SawMap datapackId={datapackId} ids={saw} />}
          </Phase>
          <Phase icon={<Sparkles className="size-3.5" />} title="Reasoning and plan" empty="The agent sent no reasoning or plan.">
            {(turn.reasoning || plan.length > 0) && (
              <div className="space-y-2 text-sm">
                {turn.reasoning && <p className="whitespace-pre-wrap">{turn.reasoning}</p>}
                {plan.length > 0 && (
                  <ol className="list-decimal space-y-0.5 pl-5">
                    {plan.map((step, i) => (
                      <li key={i}>{step}</li>
                    ))}
                  </ol>
                )}
              </div>
            )}
          </Phase>
          <Phase icon={<Wrench className="size-3.5" />} title="Did" empty="Nothing saved or changed in Meko.">
            {actions.length > 0 && <Calls calls={actions} onOpen={setCall} />}
          </Phase>
        </div>
      )}
      {call && <CallTraceDialog call={call} onClose={() => setCall(undefined)} />}
    </div>
  );
}

function Phase({ icon, title, empty, children }: { icon: ReactNode; title: string; empty: string; children: ReactNode }) {
  return (
    <section>
      <h4 className="mb-1.5 flex items-center gap-1.5 text-xs font-medium text-muted-foreground">
        {icon}
        {title}
      </h4>
      {children || <p className="text-xs text-muted-foreground">{empty}</p>}
    </section>
  );
}

function Calls({ calls, onOpen }: { calls: MekoCall[]; onOpen: (c: MekoCall) => void }) {
  return (
    <div className="divide-y rounded-md border text-xs">
      {calls.map((c, i) => (
        <button
          key={c.id ?? `${c.at}-${i}`}
          type="button"
          onClick={() => onOpen(c)}
          className="flex w-full items-center gap-2 px-2.5 py-1.5 text-left hover:bg-accent"
        >
          <span className={cx("size-1.5 shrink-0 rounded-full", c.ok ? "bg-success" : "bg-destructive")} />
          <span className="font-medium">{c.tool}</span>
          <span className="text-muted-foreground">{callResults(c)}</span>
          <Badge>{duration(c.ms)}</Badge>
          <ChevronRight className="ml-auto size-3.5 text-muted-foreground" />
        </button>
      ))}
    </div>
  );
}

// The memories this turn's searches returned, in the order they came: a list to read, beside a
// small map of the datapack with them lit and every other memory faded.
function SawMap({ datapackId, ids }: { datapackId: string; ids: Set<string> }) {
  const loaded = useLoad(() => api.memoryMap(datapackId), [datapackId]);
  if (!loaded.data) return null;
  const map = only(loaded.data, SHOWN_STATES);
  const byId = new Map(map.points.map((p) => [p.id, p]));
  const seen = [...ids].flatMap((id) => byId.get(id) ?? []);
  if (seen.length === 0) return null;
  return (
    <div className="mt-3 space-y-2">
      <p className="text-xs font-medium text-muted-foreground">
        What the agent saw: {seen.length} {seen.length === 1 ? "memory" : "memories"}
      </p>
      <div className="grid gap-3 sm:grid-cols-[14rem_minmax(0,1fr)]">
        <MemoryMapView map={map} lit={ids} mini />
        <ol className="space-y-1 text-sm">
          {seen.map((p) => (
            <li key={p.id} className="flex items-start gap-2">
              <Badge>memory</Badge>
              <span className="min-w-0 break-words">{p.text}</span>
            </li>
          ))}
        </ol>
      </div>
      <p className="text-xs text-muted-foreground">
        On the map, the bright dots are these; the faded ones are the datapack's other memories, which the agent
        didn't get.
      </p>
    </div>
  );
}
