// context_search from the browser: one question over memories, the knowledge base and past
// conversations, each source's hits in its own section.

import { useState, type FormEvent, type ReactNode } from "react";
import { Brain, ChevronDown, ChevronRight, FileText, MessagesSquare, Search } from "lucide-react";
import { api } from "../lib/api";
import { timeAgo } from "../lib/format";
import type { ContextSearch, Datapack, MemoryHit } from "../lib/types";
import { Badge, Button, Card, Empty, ErrorNote, Input, Spinner } from "../components/ui";

// Memories below this cosine are folded away as weak. context_search returns every memory it ranks,
// as Cloud's does, for an agent to weigh beside the other sources; a person reading the list needs
// the noise out of the way. Measured with Meko's embedder, right answers scored 0.40 to 0.72 and
// unrelated memories 0.11 to 0.29.
const WEAK_MEMORY = 0.4;

// The cosine similarity every source is shown with: a memory's raw score, or 1 - distance.
function Similarity({ value }: { value: number | undefined }) {
  if (value === undefined) return null;
  return <Badge tone={value >= 0.5 ? "success" : value >= WEAK_MEMORY ? "primary" : "neutral"}>{value.toFixed(2)}</Badge>;
}

// Rounded to the two places the badge shows, so a memory shown as 0.40 counts as 0.40.
const round2 = (v: number) => Math.round(v * 100) / 100;
const memorySimilarity = (m: MemoryHit) => {
  const v = m.raw_similarity_score ?? m.score;
  return v === undefined ? undefined : round2(v);
};
const isWeak = (m: MemoryHit) => (memorySimilarity(m) ?? 1) < WEAK_MEMORY;

function MemoryCard({ m }: { m: MemoryHit }) {
  return (
    <Card className="flex items-start justify-between gap-4 p-3">
      <p className="text-sm">{m.memory}</p>
      <div className="shrink-0 text-right text-xs text-muted-foreground">
        <Similarity value={memorySimilarity(m)} />
        <div className="mt-1">{timeAgo(m.created_at)}</div>
      </div>
    </Card>
  );
}

function Memories({ hits }: { hits: MemoryHit[] }) {
  const [showWeak, setShowWeak] = useState(false);
  const strong = hits.filter((m) => !isWeak(m));
  const weak = hits.filter(isWeak);
  return (
    <div className="space-y-2">
      {strong.length === 0 && <p className="text-sm text-muted-foreground">No strong matches.</p>}
      {strong.map((m) => (
        <MemoryCard key={m.id} m={m} />
      ))}
      {weak.length > 0 && (
        <button
          onClick={() => setShowWeak(!showWeak)}
          className="flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground"
        >
          {showWeak ? <ChevronDown className="size-3.5" /> : <ChevronRight className="size-3.5" />}
          {weak.length} weak {weak.length === 1 ? "match" : "matches"} (similarity below {WEAK_MEMORY})
        </button>
      )}
      {showWeak && (
        <div className="space-y-2 opacity-60">
          {weak.map((m) => (
            <MemoryCard key={m.id} m={m} />
          ))}
        </div>
      )}
    </div>
  );
}

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

export function SearchPage({ datapack }: { datapack: Datapack }) {
  const [query, setQuery] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<Error>();
  const [result, setResult] = useState<ContextSearch>();

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (!query.trim()) return;
    setBusy(true);
    setError(undefined);
    try {
      setResult(await api.search(datapack.datapack_id, query.trim()));
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
          autoFocus
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          placeholder="Ask what this datapack knows, e.g. “what do I like?”"
        />
        <Button variant="primary" disabled={busy || !query.trim()}>
          {busy ? <Spinner className="text-primary-foreground" /> : <Search className="size-4" />} Search
        </Button>
      </form>
      {error && <ErrorNote error={error} />}
      {!r && !error && (
        <Empty icon={<Search className="size-8" />} title="Search everything in this datapack">
          The same search agents run with <b>context_search</b>: memories, knowledge files and past conversations.
        </Empty>
      )}
      {r && (
        <div className="space-y-6">
          <Section icon={<Brain className="size-4 text-primary" />} title="Memories"
            n={r.memory?.length ?? null}
            badge={r.memory?.filter((m) => !isWeak(m)).length}
          >
            <Memories hits={r.memory ?? []} />
          </Section>
          <Section icon={<FileText className="size-4 text-primary" />} title="Knowledge base" n={r.knowledge_base?.length ?? null}>
            <div className="space-y-2">
              {r.knowledge_base?.map((k) => (
                <Card key={k.id} className="p-3">
                  <div className="mb-1.5 flex gap-1.5">
                    {k.metadata_filters?.source === "memory" && <Badge tone="success">promoted memory</Badge>}
                    <Badge>{k.match_type}</Badge>
                    <Similarity value={1 - k.distance} />
                    {k.rerank_score !== undefined && <Badge>rerank {k.rerank_score.toFixed(2)}</Badge>}
                  </div>
                  <p className="line-clamp-6 whitespace-pre-wrap text-sm">{k.chunk_text}</p>
                </Card>
              ))}
            </div>
          </Section>
          <Section
            icon={<MessagesSquare className="size-4 text-primary" />}
            title="Conversations"
            n={r.conversation?.hits.length ?? null}
          >
            <div className="space-y-2">
              {r.conversation?.hits.map((h) => (
                <Card key={h.message_id + h.distance} className="flex items-start justify-between gap-4 p-3">
                  <p className="line-clamp-4 whitespace-pre-wrap text-sm">{h.content}</p>
                  <Similarity value={1 - h.distance} />
                </Card>
              ))}
            </div>
          </Section>
        </div>
      )}
    </div>
  );
}
