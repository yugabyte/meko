// A datapack's memories laid out by meaning (only the given states): the Memories tab's Graph view,
// and Search's, where the question lands as a star among them and the memories it returned are lit.

import { useMemo, useState } from "react";
import { Map as MapIcon } from "lucide-react";
import { api } from "../lib/api";
import type { MapPoint, MemoryState } from "../lib/types";
import { useLoad } from "../lib/useLoad";
import { MemoryLineageDialog } from "./MemoryLineageDialog";
import { MemoryMapView, only, SHOWN_STATES } from "./MemoryMapView";
import { round2, Similarity } from "./Similarity";
import { Button, Card, Empty, ErrorNote, Loading } from "./ui";

export function MemoryGraph({
  datapackId: id,
  states,
  refresh = 0,
  query = "",
  hits,
}: {
  datapackId: string;
  states: MemoryState[];
  refresh?: number;
  // A search's question, and the ids of the memories it returned, which the map lights.
  query?: string;
  hits?: string[];
}) {
  const [picked, setPicked] = useState<MapPoint>();
  const [origin, setOrigin] = useState<MapPoint>();
  const hitKey = hits?.join() ?? "";
  const loaded = useLoad(() => api.memoryMap(id, query, hits), [id, query, hitKey, refresh]);
  const key = states.join();
  const data = useMemo(
    () => loaded.data && only(loaded.data, states.filter((s) => SHOWN_STATES.includes(s))),
    [loaded.data, key],
  );
  const lit = hits && new Set(hits);
  const matches = (data?.points ?? [])
    .filter((p) => lit?.has(p.id))
    .sort((a, b) => (b.similarity ?? 0) - (a.similarity ?? 0));

  if (loaded.error) return <ErrorNote error={loaded.error} />;
  if (!data) return <Loading />;
  if (data.points.length === 0)
    return (
      <Empty icon={<MapIcon className="size-8" />} title="No memories to map yet">
        Memories appear here once they're embedded. Agents add them with <b>memory_add</b>.
      </Empty>
    );
  return (
    <div className="grid gap-4 lg:grid-cols-[minmax(0,1fr)_20rem]">
      <MemoryMapView map={data} lit={lit} selected={picked?.id} onPick={setPicked} />
      <div className="space-y-2">
        <h3 className="text-sm font-medium">
          {query ? `${matches.length} ${matches.length === 1 ? "memory matches" : "memories match"}` : "How to read it"}
        </h3>
        {!query ? (
          <p className="text-sm text-muted-foreground">
            Memories alike in meaning sit close together, and lines join each to its nearest. Search shows where a
            question lands among them.
          </p>
        ) : matches.length === 0 ? (
          <p className="text-sm text-muted-foreground">No memory was close enough: the star shows the nearest region.</p>
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
      {origin && <MemoryLineageDialog datapackId={id} memory={origin} onClose={() => setOrigin(undefined)} />}
    </div>
  );
}
