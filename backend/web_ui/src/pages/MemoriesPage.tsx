// The datapack's memories: add one, filter, page, and for learnings keep or promote them into the
// collective memory. Delete is per memory.

import { useState, type FormEvent } from "react";
import { Brain, Check, Plus, Sparkles, Trash2 } from "lucide-react";
import { api } from "../lib/api";
import { timeAgo } from "../lib/format";
import type { Datapack } from "../lib/types";
import { useLoad } from "../lib/useLoad";
import { Badge, Button, Card, cx, Empty, ErrorNote, Input, Loading } from "../components/ui";

type Scope = "memories" | "learnings" | "promoted";

const SCOPES: { id: Scope; label: string }[] = [
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

  const list = useLoad(
    () => (scope === "promoted" ? Promise.resolve(undefined) : api.memories(id, { scope, q, page })),
    [id, scope, q, page],
  );
  const promoted = useLoad(() => (scope === "promoted" ? api.promoted(id) : Promise.resolve(undefined)), [id, scope]);

  const act = async (fn: () => Promise<unknown>) => {
    setActionError(undefined);
    try {
      await fn();
      list.reload();
      promoted.reload();
      onChanged();
    } catch (e) {
      setActionError(e as Error);
    }
  };

  return (
    <div className="space-y-4">
      <AddMemory onAdd={(text) => act(() => api.addMemory(id, text))} />

      <div className="flex flex-wrap items-center gap-3">
        <div className="flex rounded-lg border bg-card p-0.5 text-sm">
          {SCOPES.map((s) => (
            <button
              key={s.id}
              onClick={() => (setScope(s.id), setPage(1))}
              className={cx("rounded-md px-3 py-1", scope === s.id ? "bg-accent font-medium" : "text-muted-foreground")}
            >
              {s.label}
            </button>
          ))}
        </div>
        {scope !== "promoted" && (
          <Input
            className="max-w-xs"
            placeholder="Filter by text"
            value={q}
            onChange={(e) => (setQ(e.target.value), setPage(1))}
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
          <Card className="divide-y">
            {list.data.memories.map((m) => (
              <div key={m.id} className="group flex items-start justify-between gap-4 p-3">
                <div className="min-w-0">
                  <p className="text-sm">{m.data}</p>
                  <div className="mt-1 flex items-center gap-2 text-xs text-muted-foreground">
                    <Badge>{m.agent_id}</Badge>
                    {timeAgo(m.created_at)}
                  </div>
                </div>
                <div className="flex shrink-0 gap-1 opacity-60 group-hover:opacity-100">
                  {scope === "learnings" && (
                    <>
                      <Button size="sm" variant="ghost" title="Keep as memory" onClick={() => act(() => api.keep(id, [m.id]))}>
                        <Check className="size-3.5" /> Keep
                      </Button>
                      <Button
                        size="sm"
                        variant="ghost"
                        title="Promote to collective memory"
                        onClick={() => act(() => api.promote(id, [m.id]))}
                      >
                        <Sparkles className="size-3.5" /> Promote
                      </Button>
                    </>
                  )}
                  <Button size="sm" variant="danger" title="Delete" onClick={() => act(() => api.deleteMemory(id, m.id))}>
                    <Trash2 className="size-3.5" />
                  </Button>
                </div>
              </div>
            ))}
          </Card>
          {list.data.total_pages > 1 && (
            <div className="flex items-center justify-end gap-2 text-sm text-muted-foreground">
              Page {list.data.page} of {list.data.total_pages}
              <Button size="sm" disabled={page <= 1} onClick={() => setPage(page - 1)}>
                Previous
              </Button>
              <Button size="sm" disabled={page >= list.data.total_pages} onClick={() => setPage(page + 1)}>
                Next
              </Button>
            </div>
          )}
        </>
      )}
    </div>
  );
}

function AddMemory({ onAdd }: { onAdd: (text: string) => Promise<void> }) {
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    await onAdd(text.trim());
    setText("");
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
