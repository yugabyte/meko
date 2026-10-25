// The Memories tab's Graph view: the datapack's memories laid out by meaning (only the states the
// tab's scope shows), and Ask the map, a question that lands among them, lighting the memories that
// match it (0.40 or more, as Search shows) with their scores beside.

import { useMemo, useState, type FormEvent } from "react";
import { Map as MapIcon, Sparkles } from "lucide-react";
import { api } from "../lib/api";
import type { MapPoint, MemoryMap, MemoryState } from "../lib/types";
import { useLoad } from "../lib/useLoad";
import { MemoryLineageDialog } from "./MemoryLineageDialog";
import { MemoryMapView } from "./MemoryMapView";
import { round2, Similarity, WEAK_MEMORY } from "./Similarity";
import { Button, Card, Empty, ErrorNote, Input, Loading, Spinner } from "./ui";

// Only the points in states, with the links between them renumbered.
function only(m: MemoryMap, states: MemoryState[]): MemoryMap {
  const at = new Map<number, number>();
  const points = m.points.filter((p, i) => states.includes(p.state) && (at.set(i, at.size), true));
  const links = m.links.flatMap((l) =>
    at.has(l.a) && at.has(l.b) ? [{ ...l, a: at.get(l.a)!, b: at.get(l.b)! }] : [],
  );
  return { ...m, points, links };
}

export function MemoryGraph({
  datapackId: id,
  states,
  refresh,
}: {
  datapackId: string;
  states: MemoryState[];
  refresh: number;
}) {
  const [draft, setDraft] = useState("");
  const [query, setQuery] = useState("");
  const [picked, setPicked] = useState<MapPoint>();
  const [origin, setOrigin] = useState<MapPoint>();
  const loaded = useLoad(() => api.memoryMap(id, query), [id, query, refresh]);
  const key = states.join();
  const data = useMemo(() => loaded.data && only(loaded.data, states), [loaded.data, key]);
  const map = { ...loaded, data };

  const submit = (e: FormEvent) => {
    e.preventDefault();
    setQuery(draft.trim());
  };
  const matches = query
    ? (map.data?.points ?? [])
        .filter((p) => round2(p.similarity ?? 0) >= WEAK_MEMORY)
        .sort((a, b) => (b.similarity ?? 0) - (a.similarity ?? 0))
    : [];
  const lit = query ? new Set(matches.map((p) => p.id)) : undefined;

  return (
    <div className="space-y-4">
      <form onSubmit={submit} className="flex gap-2">
        <Input value={draft} onChange={(e) => setDraft(e.target.value)} placeholder="Ask the map any question" />
        <Button variant="primary" disabled={map.loading && !!query}>
          {map.loading && query ? <Spinner className="text-primary-foreground" /> : <Sparkles className="size-4" />} Ask
        </Button>
        {query && (
          <Button variant="ghost" type="button" onClick={() => (setDraft(""), setQuery(""))}>
            Clear
          </Button>
        )}
      </form>

      {map.error ? (
        <ErrorNote error={map.error} />
      ) : !map.data ? (
        <Loading />
      ) : map.data.points.length === 0 ? (
        <Empty icon={<MapIcon className="size-8" />} title="No memories to map yet">
          Memories appear here once they're embedded. Agents add them with <b>memory_add</b>, or add one above.
        </Empty>
      ) : (
        <div className="grid gap-4 lg:grid-cols-[minmax(0,1fr)_20rem]">
          <MemoryMapView map={map.data} lit={lit} selected={picked?.id} onPick={setPicked} />
          <div className="space-y-2">
            <h3 className="text-sm font-medium">
              {query ? `${matches.length} ${matches.length === 1 ? "memory matches" : "memories match"}` : "How to read it"}
            </h3>
            {!query ? (
              <p className="text-sm text-muted-foreground">
                Memories alike in meaning sit close together, and lines join each to its nearest. Ask a question to see
                where it lands and which memories answer it.
              </p>
            ) : matches.length === 0 ? (
              <p className="text-sm text-muted-foreground">Nothing scores 0.40 or more: the star shows the nearest region.</p>
            ) : (
              <Card className="divide-y">
                {matches.map((p) => (
                  <button
                    key={p.id}
                    type="button"
                    onClick={() => setPicked(p)}
                    className="flex w-full items-start justify-between gap-3 p-2.5 text-left text-sm hover:bg-accent"
                  >
                    <span className="min-w-0 break-words">{p.text}</span>
                    <Similarity value={round2(p.similarity ?? 0)} />
                  </button>
                ))}
              </Card>
            )}
            {picked && (
              <Button size="sm" variant="ghost" onClick={() => setOrigin(picked)}>
                Where the selected memory came from
              </Button>
            )}
          </div>
        </div>
      )}
      {origin && <MemoryLineageDialog datapackId={id} memory={origin} onClose={() => setOrigin(undefined)} />}
    </div>
  );
}
