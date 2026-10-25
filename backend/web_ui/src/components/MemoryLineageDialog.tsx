// Where a memory came from, as a timeline: the conversation turn it was saved during (or that an
// agent added it directly), its changes as mem0 recorded them, and where it is now (a learning,
// kept, or promoted to the collective memory).

import { ArrowUpRight } from "lucide-react";
import type { ReactNode } from "react";
import { api } from "../lib/api";
import { dateTime } from "../lib/format";
import { navigate } from "../lib/route";
import type { MemoryChange, MemoryState, Turn } from "../lib/types";
import { useLoad } from "../lib/useLoad";
import { Dialog } from "./Dialog";
import { STATE_COLOR, STATE_LABEL } from "./MemoryMapView";
import { Badge, Button, ErrorNote, Loading } from "./ui";

export interface LineageOf {
  id: string;
  // Unknown on the memories list, which mixes learnings and kept ones.
  state?: MemoryState;
  agent_id: string;
  conversation_id?: string;
  created_at: string;
}

const TONE: Record<string, "success" | "primary" | "danger"> = { ADD: "success", UPDATE: "primary", DELETE: "danger" };

const NOW: Record<MemoryState, string> = {
  learning: "A learning: not reviewed yet.",
  kept: "Reviewed and kept as a memory in this datapack; no longer waiting on the Learnings list.",
  collective: "Promoted to the collective memory, so every agent finds it through the knowledge base.",
};

// A turn is stored when the agent saves it, after its memories, so the memory's turn is the first
// one stored at or after it.
function turnOf(thread: Turn[], at: string): Turn | undefined {
  const t = new Date(at).getTime();
  return thread.find((x) => new Date(x.started_at).getTime() >= t) ?? thread[thread.length - 1];
}

export function MemoryLineageDialog({
  datapackId,
  memory,
  onClose,
}: {
  datapackId: string;
  memory: LineageOf;
  onClose: () => void;
}) {
  // A collective memory's own history isn't served: it lives on with its promotion.
  const history = useLoad(
    () => (memory.state === "collective" ? Promise.resolve(undefined) : api.memoryHistory(datapackId, memory.id)),
    [datapackId, memory.id],
  );
  const conv = useLoad(
    () => (memory.conversation_id ? api.conversation(datapackId, memory.conversation_id) : Promise.resolve(undefined)),
    [datapackId, memory.conversation_id],
  );
  const c = conv.data?.conversation;
  const turn = c && turnOf(c.thread, memory.created_at);

  return (
    <Dialog title="Where this memory came from" onClose={onClose} actions={<Button onClick={onClose}>Close</Button>}>
      <ol className="space-y-4">
        <Step title="Saved by" when={memory.created_at}>
          <Badge>{memory.agent_id || "an agent"}</Badge>{" "}
          {!memory.conversation_id ? (
            <span className="text-muted-foreground">directly, with memory_add, outside a conversation.</span>
          ) : conv.loading ? (
            <Loading />
          ) : !c ? (
            <span className="text-muted-foreground">in a conversation that's no longer stored.</span>
          ) : (
            <>
              <span className="text-muted-foreground">during </span>
              <button
                type="button"
                className="inline-flex items-center gap-0.5 font-medium text-primary hover:underline"
                onClick={() => navigate({ datapackId, tab: "conversations", itemId: c.id })}
              >
                {c.title || "a conversation"} <ArrowUpRight className="size-3" />
              </button>
              {turn && (
                <blockquote className="mt-2 border-l-2 pl-2 text-xs text-muted-foreground">
                  <span className="line-clamp-3">{turn.input}</span>
                </blockquote>
              )}
            </>
          )}
        </Step>
        {history.error && <ErrorNote error={history.error} />}
        {(history.data?.history ?? []).map((h: MemoryChange, i) => (
          <Step
            key={i}
            title={<Badge tone={TONE[h.action] ?? "primary"}>{h.action.toLowerCase()}</Badge>}
            when={h.updated_at ?? h.created_at}
          >
            {h.action === "UPDATE" && h.previous_value !== null && (
              <p className="text-muted-foreground line-through">{h.previous_value}</p>
            )}
            {h.new_value !== null && h.action !== "DELETE" && <p>{h.new_value}</p>}
          </Step>
        ))}
        {memory.state && (
          <Step title={<span className={STATE_COLOR[memory.state]}>{STATE_LABEL[memory.state]}</span>}>
            {NOW[memory.state]}
          </Step>
        )}
      </ol>
    </Dialog>
  );
}

function Step({ title, when, children }: { title: ReactNode; when?: string; children: ReactNode }) {
  return (
    <li className="border-l-2 pl-3 text-sm">
      <div className="flex items-center gap-2 text-xs font-medium">
        {title}
        {when && <span className="font-normal text-muted-foreground">{dateTime(when)}</span>}
      </div>
      <div className="mt-1">{children}</div>
    </li>
  );
}
