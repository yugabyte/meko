// A turn's decision trace, collapsed to one summary line: what the agent looked up in Meko, why it
// answered so and the steps it took (its own reasoning and plan), and what it saved. Each call opens
// its step timings. Meko local builds it from the call log; Cloud's comes from Langfuse.

import { useState, type ReactNode } from "react";
import { ChevronDown, ChevronRight, Search, Sparkles, Wrench } from "lucide-react";
import { callResults, isSearch, resultCount } from "../lib/calls";
import { duration } from "../lib/format";
import type { MekoCall, Turn } from "../lib/types";
import { CallTraceDialog } from "./metrics/CallTraceDialog";
import { Badge, cx } from "./ui";

export function DecisionTrace({ turn }: { turn: Turn }) {
  const [open, setOpen] = useState(false);
  const [call, setCall] = useState<MekoCall>();
  const calls = turn.calls ?? [];
  const plan = turn.plan ?? [];
  const searches = calls.filter(isSearch);
  const actions = calls.filter((c) => !isSearch(c));
  if (calls.length === 0 && !turn.reasoning && plan.length === 0) return null;

  const found = searches.reduce((n, c) => n + resultCount(c), 0);
  const ms = calls.reduce((n, c) => n + c.ms, 0);
  const summary = [
    searches.length > 0 && `${searches.length} ${searches.length === 1 ? "search" : "searches"} · ${found} found`,
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
          <Phase icon={<Search className="size-3.5" />} title="Looked up" empty="No Meko searches before this turn.">
            {searches.length > 0 && <Calls calls={searches} onOpen={setCall} />}
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
