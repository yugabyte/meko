// context_search from the browser: one question over memories, the knowledge base and past
// conversations, each source's hits in its own section.

import { useState, type FormEvent, type ReactNode } from "react";
import { Brain, FileText, MessagesSquare, Search } from "lucide-react";
import { api } from "../lib/api";
import { timeAgo } from "../lib/format";
import type { ContextSearch, Datapack } from "../lib/types";
import { Badge, Button, Card, Empty, ErrorNote, Input, Spinner } from "../components/ui";

function Section({ icon, title, n, children }: { icon: ReactNode; title: string; n: number; children: ReactNode }) {
  return (
    <section className="space-y-2">
      <h3 className="flex items-center gap-2 text-sm font-medium">
        {icon}
        {title}
        <Badge>{n}</Badge>
      </h3>
      {n === 0 ? <p className="text-sm text-muted-foreground">Nothing close enough.</p> : children}
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
          <Section icon={<Brain className="size-4 text-primary" />} title="Memories" n={r.memory.length}>
            <div className="space-y-2">
              {r.memory.map((m) => (
                <Card key={m.id} className="flex items-start justify-between gap-4 p-3">
                  <p className="text-sm">{m.memory}</p>
                  <div className="shrink-0 text-right text-xs text-muted-foreground">
                    <Badge tone="primary">{m.score.toFixed(2)}</Badge>
                    <div className="mt-1">{timeAgo(m.created_at)}</div>
                  </div>
                </Card>
              ))}
            </div>
          </Section>
          <Section icon={<FileText className="size-4 text-primary" />} title="Knowledge base" n={r.knowledge_base.length}>
            <div className="space-y-2">
              {r.knowledge_base.map((k) => (
                <Card key={k.id} className="p-3">
                  <div className="mb-1.5 flex gap-1.5">
                    {k.metadata_filters?.source === "memory" && <Badge tone="success">promoted memory</Badge>}
                    <Badge>{k.match_type}</Badge>
                    {k.rerank_score !== undefined && <Badge tone="primary">{k.rerank_score.toFixed(2)}</Badge>}
                  </div>
                  <p className="line-clamp-6 whitespace-pre-wrap text-sm">{k.chunk_text}</p>
                </Card>
              ))}
            </div>
          </Section>
          <Section
            icon={<MessagesSquare className="size-4 text-primary" />}
            title="Conversations"
            n={r.conversation.hits.length}
          >
            <div className="space-y-2">
              {r.conversation.hits.map((h) => (
                <Card key={h.message_id + h.distance} className="p-3">
                  <p className="line-clamp-4 whitespace-pre-wrap text-sm">{h.content}</p>
                </Card>
              ))}
            </div>
          </Section>
        </div>
      )}
    </div>
  );
}
