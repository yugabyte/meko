// The datapack's memories: add one, filter, page, and select several to delete, or (learnings) to
// keep or promote into the collective memory.

import { useState, type FormEvent } from "react";
import { Brain, Check, Plus, Sparkles, Trash2 } from "lucide-react";
import { api } from "../lib/api";
import { timeAgo } from "../lib/format";
import type { Datapack } from "../lib/types";
import { useLoad } from "../lib/useLoad";
import { Pager } from "../components/Pager";
import { Segmented } from "../components/Segmented";
import { Badge, Button, Card, Empty, ErrorNote, Input, Loading } from "../components/ui";

type Scope = "memories" | "learnings" | "promoted";

const SCOPES: readonly { id: Scope; label: string }[] = [
  { id: "memories", label: "All memories" },
  { id: "learnings", label: "Learnings" },
  { id: "promoted", label: "Collective" },
];

export function MemoriesPage({ datapack, onChanged }: { datapack: Datapack; onChanged: () => void }) {
  const id = datapack.datapack_id;
  const [scope, setScope] = useState<Scope>("memories");
  const [q, setQ] = useState("");
  const [page, setPage] = useState(1);
  const [actionError, setActionError] = useState<Error>();
  const [selected, setSelected] = useState<Set<string>>(new Set());

  const list = useLoad(
    () => (scope === "promoted" ? Promise.resolve(undefined) : api.memories(id, { scope, q, page })),
    [id, scope, q, page],
  );
  const promoted = useLoad(() => (scope === "promoted" ? api.promoted(id) : Promise.resolve(undefined)), [id, scope]);

  // Reports whether it worked; a failure is shown above the list.
  const act = async (fn: () => Promise<unknown>): Promise<boolean> => {
    setActionError(undefined);
    try {
      await fn();
      setSelected(new Set());
      list.reload();
      promoted.reload();
      onChanged();
      return true;
    } catch (e) {
      setActionError(e as Error);
      return false;
    }
  };

  return (
    <div className="space-y-4">
      <AddMemory onAdd={(text) => act(() => api.addMemory(id, text))} />

      <div className="flex flex-wrap items-center gap-3">
        <Segmented options={SCOPES} value={scope} onChange={(v) => (setScope(v), setPage(1), setSelected(new Set()))} />
        {scope !== "promoted" && (
          <Input
            className="max-w-xs"
            placeholder="Filter by text"
            value={q}
            onChange={(e) => (setQ(e.target.value), setPage(1), setSelected(new Set()))}
          />
        )}
      </div>

      {actionError && <ErrorNote error={actionError} />}

      {scope === "promoted" ? (
        promoted.loading && !promoted.data ? (
          <Loading />
        ) : promoted.error ? (
          <ErrorNote error={promoted.error} />
        ) : !promoted.data?.promoted_memories.length ? (
          <Empty icon={<Sparkles className="size-8" />} title="No collective memory yet">
            Promote learnings to share them with every agent through the knowledge base.
          </Empty>
        ) : (
          <Card className="divide-y">
            {promoted.data.promoted_memories.map((m) => (
              <div key={m.id} className="flex items-start justify-between gap-4 p-3">
                <p className="text-sm">{m.data}</p>
                <span className="shrink-0 text-xs text-muted-foreground">{timeAgo(m.created_at)}</span>
              </div>
            ))}
          </Card>
        )
      ) : list.loading && !list.data ? (
        <Loading />
      ) : list.error ? (
        <ErrorNote error={list.error} />
      ) : !list.data?.memories.length ? (
        <Empty icon={<Brain className="size-8" />} title={q ? "No memories match" : "No memories yet"}>
          Agents add memories with <b>memory_add</b>, or add one above.
        </Empty>
      ) : (
        <>
          <SelectionBar
            count={selected.size}
            learnings={scope === "learnings"}
            onKeep={() => act(() => api.keep(id, [...selected]))}
            onPromote={() => act(() => api.promote(id, [...selected]))}
            onDelete={() => act(() => Promise.all([...selected].map((m) => api.deleteMemory(id, m))))}
            onClear={() => setSelected(new Set())}
          />
          <Card className="divide-y">
            {list.data.memories.map((m) => (
              <label key={m.id} className="flex cursor-pointer items-start gap-3 p-3 hover:bg-accent/50">
                <input
                  type="checkbox"
                  className="mt-1 accent-primary"
                  checked={selected.has(m.id)}
                  onChange={() => setSelected(toggle(selected, m.id))}
                />
                <div className="min-w-0">
                  <p className="text-sm">{m.data}</p>
                  <div className="mt-1 flex items-center gap-2 text-xs text-muted-foreground">
                    <Badge>{m.agent_id}</Badge>
                    {timeAgo(m.created_at)}
                  </div>
                </div>
              </label>
            ))}
          </Card>
          <Pager
            label={`Page ${list.data.page} of ${list.data.total_pages}`}
            canPrev={page > 1}
            canNext={page < list.data.total_pages}
            onPrev={() => (setPage(page - 1), setSelected(new Set()))}
            onNext={() => (setPage(page + 1), setSelected(new Set()))}
          />
        </>
      )}
    </div>
  );
}

function toggle(set: Set<string>, id: string): Set<string> {
  const next = new Set(set);
  if (!next.delete(id)) next.add(id);
  return next;
}

// What can be done to the selected memories: keep and promote apply only to learnings.
function SelectionBar({
  count,
  learnings,
  onKeep,
  onPromote,
  onDelete,
  onClear,
}: {
  count: number;
  learnings: boolean;
  onKeep: () => void;
  onPromote: () => void;
  onDelete: () => void;
  onClear: () => void;
}) {
  if (count === 0) {
    return (
      <p className="text-xs text-muted-foreground">
        Select memories to {learnings ? "keep, promote or " : ""}delete them.
      </p>
    );
  }
  return (
    <div className="flex flex-wrap items-center gap-2 rounded-lg border bg-card px-3 py-2 text-sm">
      <span className="font-medium">{count} selected</span>
      {learnings && (
        <>
          <Button size="sm" variant="ghost" onClick={onKeep} title="Keep as private memories">
            <Check className="size-3.5" /> Keep
          </Button>
          <Button size="sm" variant="ghost" onClick={onPromote} title="Promote to the collective memory">
            <Sparkles className="size-3.5" /> Promote
          </Button>
        </>
      )}
      <Button
        size="sm"
        variant="danger"
        onClick={() => confirm(`Delete ${count} ${count === 1 ? "memory" : "memories"}?`) && onDelete()}
      >
        <Trash2 className="size-3.5" /> Delete
      </Button>
      <Button size="sm" variant="ghost" className="ml-auto" onClick={onClear}>
        Clear
      </Button>
    </div>
  );
}

function AddMemory({ onAdd }: { onAdd: (text: string) => Promise<boolean> }) {
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    if (await onAdd(text.trim())) setText("");
    setBusy(false);
  };
  return (
    <form onSubmit={submit} className="flex gap-2">
      <Input value={text} onChange={(e) => setText(e.target.value)} placeholder="Add a memory, word for word" />
      <Button variant="primary" disabled={busy || !text.trim()}>
        <Plus className="size-4" /> Add
      </Button>
    </form>
  );
}
