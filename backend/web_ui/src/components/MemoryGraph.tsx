// A map laid out by meaning: the Memories tab's Graph view (a datapack's memories, only the given
// states), or Search's (the 100 memories, knowledge-base chunks and conversation turns nearest the
// question, with what the search returned lit and listed beside it).

import { useMemo, useState } from "react";
import { Map as MapIcon } from "lucide-react";
import { api } from "../lib/api";
import type { MapPoint, MapSource, MemoryState } from "../lib/types";
import { useLoad } from "../lib/useLoad";
import { MemoryLineageDialog, type LineageOf } from "./MemoryLineageDialog";
import { MemoryMapView, only, SHOWN_STATES, SOURCE_COLOR } from "./MemoryMapView";
import { round2, Similarity } from "./Similarity";
import { Button, Card, cx, Empty, ErrorNote, Loading } from "./ui";

// A search to draw: its question, the sources it searched, and the ids of what it returned (memory
// and chunk ids, turns' message ids).
export interface MapSearch {
  query: string;
  sources: MapSource[];
  hits: string[];
}

export function MemoryGraph({
  datapackId: id,
  states = SHOWN_STATES,
  refresh = 0,
  search,
}: {
  datapackId: string;
  states?: MemoryState[];
  refresh?: number;
  search?: MapSearch;
}) {
  const [picked, setPicked] = useState<MapPoint>();
  const [origin, setOrigin] = useState<LineageOf>();
  const searchKey = search ? [search.query, search.sources.join(), search.hits.join()].join("|") : "";
  const loaded = useLoad(
    () => (search ? api.searchMap(id, search.query, search.sources, search.hits) : api.memoryMap(id)),
    [id, searchKey, refresh],
  );
  const key = states.join();
  const data = useMemo(
    () => loaded.data && (search ? loaded.data : only(loaded.data, states.filter((s) => SHOWN_STATES.includes(s)))),
    [loaded.data, key, searchKey],
  );
  const lit = search && new Set(search.hits);
  const matches = (data?.points ?? [])
    .filter((p) => lit?.has(p.id))
    .sort((a, b) => (b.similarity ?? 0) - (a.similarity ?? 0));

  if (loaded.error) return <ErrorNote error={loaded.error} />;
  if (!data) return <Loading />;
  if (data.points.length === 0)
    return (
      <Empty icon={<MapIcon className="size-8" />} title={search ? "Nothing to map" : "No memories to map yet"}>
        {search
          ? "Nothing searched is embedded yet."
          : "Memories appear here once they're embedded. Agents add them with memory_add."}
      </Empty>
    );
  return (
    <div className="grid gap-4 lg:grid-cols-[minmax(0,1fr)_20rem]">
      <MemoryMapView map={data} lit={lit} selected={picked?.id} onPick={setPicked} />
      <div className="space-y-2">
        <h3 className="text-sm font-medium">
          {search ? `${matches.length} returned, among the ${data.points.length} nearest` : "How to read it"}
        </h3>
        {!search ? (
          <p className="text-sm text-muted-foreground">
            Memories alike in meaning sit close together, and lines join each to its nearest. Search shows where a
            question lands among them.
          </p>
        ) : matches.length === 0 ? (
          <p className="text-sm text-muted-foreground">Nothing was close enough: the star shows the nearest region.</p>
        ) : (
          <Card className="divide-y">
            {matches.map((p) => (
              <button
                key={p.id}
                type="button"
                onClick={() => setPicked(p)}
                className="flex w-full items-start justify-between gap-3 p-2.5 text-left text-sm hover:bg-accent"
              >
                <span className="flex min-w-0 items-start gap-2">
                  {p.source && (
                    <span className={cx("mt-1.5 size-2 shrink-0 rounded-full bg-current", SOURCE_COLOR[p.source])} />
                  )}
                  <span className="line-clamp-3 min-w-0 break-words">{p.text}</span>
                </span>
                <Similarity value={round2(p.similarity ?? 0)} />
              </button>
            ))}
          </Card>
        )}
        {/* Only a memory map's points carry what the lineage needs. */}
        {picked?.agent_id && picked.created_at && (
          <Button
            size="sm"
            variant="ghost"
            onClick={() => setOrigin({ ...picked, agent_id: picked.agent_id!, created_at: picked.created_at! })}
          >
            Where the selected memory came from
          </Button>
        )}
      </div>
      {origin && <MemoryLineageDialog datapackId={id} memory={origin} onClose={() => setOrigin(undefined)} />}
    </div>
  );
}
