// context_search from the browser: one question over memories, the knowledge base and past
// conversations, each source's hits in its own section. Graph shows the 100 things nearest the
// question as a map instead, colored by source, with what the search returned lit.

import { useState, type FormEvent, type ReactNode } from "react";
import { Brain, FileText, MessagesSquare, Search } from "lucide-react";
import { api, type SearchSource } from "../lib/api";
import { SEARCH_INPUT_ID } from "../lib/useShortcuts";
import { SHOW_COLLECTIVE } from "../lib/features";
import { timeAgo } from "../lib/format";
import type { ContextSearch, Datapack, MapSource, MemoryHit } from "../lib/types";
import { useSavedView, VIEWS } from "../lib/useSavedView";
import { ExpandableText } from "../components/ExpandableText";
import { MemoryGraph } from "../components/MemoryGraph";
import { Segmented } from "../components/Segmented";
import { round2, Similarity, WEAK_MEMORY } from "../components/Similarity";
import { Badge, Button, Card, Empty, ErrorNote, Input, Spinner } from "../components/ui";

// The cosine similarity every source is shown with: a memory's raw score, or 1 - distance.
const memorySimilarity = (m: MemoryHit) => {
  const v = m.raw_similarity_score ?? m.score;
  return v === undefined ? undefined : round2(v);
};
const isWeak = (m: MemoryHit) => (memorySimilarity(m) ?? 1) < WEAK_MEMORY;

function MemoryCard({ m }: { m: MemoryHit }) {
  return (
    <Card className="flex items-start justify-between gap-4 p-3">
      <p className="min-w-0 break-words text-sm">{m.memory}</p>
      <div className="shrink-0 text-right text-xs text-muted-foreground">
        <Similarity value={memorySimilarity(m)} />
        <div className="mt-1">{timeAgo(m.created_at)}</div>
      </div>
    </Card>
  );
}

function Memories({ hits }: { hits: MemoryHit[] }) {
  const strong = hits.filter((m) => !isWeak(m));
  if (strong.length === 0) return <p className="text-sm text-muted-foreground">Nothing close enough.</p>;
  return (
    <div className="space-y-2">
      {strong.map((m) => (
        <MemoryCard key={m.id} m={m} />
      ))}
    </div>
  );
}

// Whether a search with this source shows that section: all of them, or the one it searched.
const shows = (searched: SearchSource, section: SearchSource) => searched === "auto" || searched === section;

// n is null when the source wasn't searched; badge, if set, is the count shown instead of n.
function Section({
  icon,
  title,
  n,
  badge,
  children,
}: {
  icon: ReactNode;
  title: string;
  n: number | null;
  badge?: number;
  children: ReactNode;
}) {
  return (
    <section className="space-y-2">
      <h3 className="flex items-center gap-2 text-sm font-medium">
        {icon}
        {title}
        {n !== null && <Badge>{badge ?? n}</Badge>}
      </h3>
      {n === null ? (
        <p className="text-sm text-muted-foreground">Not searched: Meko's memory engine isn't ready yet.</p>
      ) : n === 0 ? (
        <p className="text-sm text-muted-foreground">Nothing close enough.</p>
      ) : (
        children
      )}
    </section>
  );
}

const MAP_SOURCES: MapSource[] = ["memory", "knowledge_base", "conversation"];

const SOURCES: readonly { id: SearchSource; label: string }[] = [
  { id: "auto", label: "All" },
  { id: "memory", label: "Memory" },
  { id: "knowledge_base", label: "Knowledge base" },
  { id: "conversation", label: "Conversations" },
];

export function SearchPage({ datapack }: { datapack: Datapack }) {
  const [query, setQuery] = useState("");
  const [source, setSource] = useState<SearchSource>("auto");
  // The question and source the shown results were searched with.
  const [asked, setAsked] = useState("");
  const [searched, setSearched] = useState<SearchSource>("auto");
  const [view, setView] = useSavedView("meko.search.view");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<Error>();
  const [result, setResult] = useState<ContextSearch>();

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (!query.trim()) return;
    setBusy(true);
    setError(undefined);
    try {
      setResult(await api.search(datapack.datapack_id, query.trim(), source));
      setAsked(query.trim());
      setSearched(source);
    } catch (err) {
      setError(err as Error);
    } finally {
      setBusy(false);
    }
  };

  const r = result?.results;
  return (
    <div className="space-y-6">
      <form onSubmit={submit} className="flex gap-2">
        <Input
          id={SEARCH_INPUT_ID}
          autoFocus
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          placeholder="Ask what this datapack knows, e.g. “what do I like?” (press / from anywhere)"
        />
        <Button variant="primary" disabled={busy || !query.trim()}>
          {busy ? <Spinner className="text-primary-foreground" /> : <Search className="size-4" />} Search
        </Button>
      </form>
      <div className="flex flex-wrap items-center gap-3">
        <Segmented options={SOURCES} value={source} onChange={setSource} />
        <div className="ml-auto">
          <Segmented options={VIEWS} value={view} onChange={setView} />
        </div>
      </div>
      {error && <ErrorNote error={error} />}
      {!r && !error && (
        <Empty icon={<Search className="size-8" />} title="Search everything in this datapack">
          The same search agents run with <b>context_search</b>: memories, knowledge files and past conversations.
        </Empty>
      )}
      {r && view === "graph" && (
        <MemoryGraph
          datapackId={datapack.datapack_id}
          search={{
            query: asked,
            sources: MAP_SOURCES.filter((s) => shows(searched, s)),
            hits: [
              ...(r.memory ?? []).filter((m) => !isWeak(m)).map((m) => m.id),
              ...(r.knowledge_base ?? []).map((k) => k.id),
              ...(r.conversation?.hits ?? []).map((h) => h.message_id),
            ],
          }}
        />
      )}
      {r && view === "list" && (
        <div className="space-y-6">
          {shows(searched, "memory") && (
            <Section
              icon={<Brain className="size-4 text-primary" />}
              title="Memories"
              n={r.memory?.length ?? null}
              badge={r.memory?.filter((m) => !isWeak(m)).length}
            >
              <Memories hits={r.memory ?? []} />
            </Section>
          )}
          {shows(searched, "knowledge_base") && (
            <Section
              icon={<FileText className="size-4 text-primary" />}
              title="Knowledge base"
              n={r.knowledge_base?.length ?? null}
            >
              <div className="space-y-2">
                {r.knowledge_base?.map((k) => (
                  <Card key={k.id} className="p-3">
                    <div className="mb-1.5 flex gap-1.5">
                      {SHOW_COLLECTIVE && k.metadata_filters?.source === "memory" && (
                        <Badge tone="success">promoted memory</Badge>
                      )}
                      <Badge>{k.match_type}</Badge>
                      <Similarity value={1 - k.distance} />
                      {k.rerank_score !== undefined && <Badge>rerank {k.rerank_score.toFixed(2)}</Badge>}
                    </div>
                    <ExpandableText text={k.chunk_text} lines={6} />
                  </Card>
                ))}
              </div>
            </Section>
          )}
          {shows(searched, "conversation") && (
            <Section
              icon={<MessagesSquare className="size-4 text-primary" />}
              title="Conversations"
              n={r.conversation?.hits.length ?? null}
            >
              <div className="space-y-2">
                {r.conversation?.hits.map((h) => (
                  <Card key={h.message_id + h.distance} className="flex items-start justify-between gap-4 p-3">
                    <div className="min-w-0 flex-1">
                      <ExpandableText text={h.content} lines={4} />
                    </div>
                    <Similarity value={1 - h.distance} />
                  </Card>
                ))}
              </div>
            </Section>
          )}
        </div>
      )}
    </div>
  );
}
