// One call from the call log: what it was, how it went, and the time each of its steps took (a
// search's sources run side by side, so steps can add up to more than the whole).

import { dateTime, duration } from "../../lib/format";
import type { Usage } from "../../lib/types";
import { Dialog } from "../Dialog";
import { KeyValues } from "../KeyValues";
import { Badge, Button } from "../ui";

type Call = Usage["recent"][number];

export function CallTraceDialog({ call, onClose }: { call: Call; onClose: () => void }) {
  const longest = Math.max(1, call.ms, ...call.steps.map((s) => s.ms));
  const rows: [string, string][] = [
    ["Agent", call.agent_id || "—"],
    ["When", dateTime(call.at)],
    ["Took", duration(call.ms)],
    ["Results", `${call.memory_n} memories · ${call.kb_n} knowledge · ${call.conversation_n} conversation`],
  ];
  return (
    <Dialog title={call.tool} onClose={onClose} actions={<Button onClick={onClose}>Close</Button>}>
      <div className="flex items-center gap-2">
        {call.ok ? <Badge tone="success">ok</Badge> : <Badge tone="danger">{call.error_code}</Badge>}
        <span className="text-xs text-muted-foreground">call #{call.id}</span>
      </div>
      <KeyValues rows={rows} />
      <div>
        <div className="mb-2 text-xs font-medium text-muted-foreground">Steps</div>
        {call.steps.length === 0 ? (
          <p className="text-sm text-muted-foreground">This call recorded no steps.</p>
        ) : (
          <div className="space-y-2">
            {call.steps.map((s) => (
              <div key={s.step}>
                <div className="flex justify-between text-xs">
                  <span>{s.step}</span>
                  <span className="tabular-nums text-muted-foreground">
                    {duration(s.ms)} · {s.n} results
                  </span>
                </div>
                <div className="mt-1 h-2 rounded-full bg-muted">
                  <div className="h-2 rounded-full bg-primary" style={{ width: `${(100 * s.ms) / longest}%` }} />
                </div>
              </div>
            ))}
          </div>
        )}
      </div>
    </Dialog>
  );
}
