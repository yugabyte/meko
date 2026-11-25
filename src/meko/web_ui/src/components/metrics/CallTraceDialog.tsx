// One call from the call log: what it was, how it went, the time each of its steps took (a search's
// sources run side by side, so steps can add up to more than the whole), and, while Settings keeps
// call contents, what it sent and got back.

import { api } from "../../lib/api";
import { dateTime, duration } from "../../lib/format";
import type { MekoCall } from "../../lib/types";
import { useLoad } from "../../lib/useLoad";
import { CopyBlock } from "../CopyBlock";
import { Dialog } from "../Dialog";
import { KeyValues } from "../KeyValues";
import { Badge, Button, ErrorNote, Spinner } from "../ui";

export function CallTraceDialog({ call, onClose }: { call: MekoCall; onClose: () => void }) {
  // An older server's calls carry no steps.
  const steps = call.steps ?? [];
  const longest = Math.max(1, call.ms, ...steps.map((s) => s.ms));
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
        {call.id !== undefined && <span className="text-xs text-muted-foreground">call #{call.id}</span>}
      </div>
      <KeyValues rows={rows} />
      <div>
        <div className="mb-2 text-xs font-medium text-muted-foreground">Steps</div>
        {steps.length === 0 ? (
          <p className="text-sm text-muted-foreground">This call recorded no steps.</p>
        ) : (
          <div className="space-y-2">
            {steps.map((s) => (
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
      {call.id !== undefined && <Contents id={call.id} />}
    </Dialog>
  );
}

// What the call sent and got back, when they're kept (Settings' "Keep call contents").
function Contents({ id }: { id: number }) {
  const detail = useLoad(() => api.call(id), [id]);
  const d = detail.data;
  if (detail.error) return <ErrorNote error={detail.error} />;
  if (!d) return <Spinner />;
  if (d.args === undefined) {
    return (
      <p className="text-sm text-muted-foreground">
        What this call sent and got back isn't kept: Keep call contents in Settings was off at the time, or they were
        cleared, or the call is older than 7 days.
      </p>
    );
  }
  return (
    <div className="space-y-3">
      <div>
        <div className="mb-2 text-xs font-medium text-muted-foreground">Sent</div>
        <CopyBlock text={pretty(d.args)} />
      </div>
      <div>
        <div className="mb-2 text-xs font-medium text-muted-foreground">Got back</div>
        {d.reply ? <CopyBlock text={pretty(d.reply ?? "")} /> : <p className="text-sm text-muted-foreground">Nothing.</p>}
      </div>
      {d.cut && <p className="text-xs text-muted-foreground">Cut to 1 MB to keep.</p>}
    </div>
  );
}

// JSON indented to read, anything else as it is.
function pretty(text: string): string {
  try {
    return JSON.stringify(JSON.parse(text), null, 2);
  } catch {
    return text;
  }
}
