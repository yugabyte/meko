// The agents' conversations, most recently active first, and one conversation's turns with the
// memories each produced.

import { useState } from "react";
import { ArrowLeft, Brain, MessagesSquare } from "lucide-react";
import { api } from "../lib/api";
import { dateTime, timeAgo } from "../lib/format";
import { href, navigate } from "../lib/route";
import type { Datapack } from "../lib/types";
import { useLoad } from "../lib/useLoad";
import { Badge, Button, Card, Empty, ErrorNote, Input, Loading } from "../components/ui";

const PAGE = 20;

export function ConversationsPage({ datapack, conversationId }: { datapack: Datapack; conversationId?: string }) {
  return conversationId ? (
    <Thread datapack={datapack} conversationId={conversationId} />
  ) : (
    <List datapack={datapack} />
  );
}

function List({ datapack }: { datapack: Datapack }) {
  const id = datapack.datapack_id;
  const [q, setQ] = useState("");
  const [offset, setOffset] = useState(0);
  const list = useLoad(() => api.conversations(id, q, offset), [id, q, offset]);

  return (
    <div className="space-y-4">
      <Input
        className="max-w-sm"
        placeholder="Filter by title or text"
        value={q}
        onChange={(e) => (setQ(e.target.value), setOffset(0))}
      />
      {list.loading && !list.data ? (
        <Loading />
      ) : list.error ? (
        <ErrorNote error={list.error} />
      ) : !list.data?.conversations.length ? (
        <Empty icon={<MessagesSquare className="size-8" />} title={q ? "No conversations match" : "No conversations yet"}>
          Agents record their turns with <b>conversation_add_message</b>.
        </Empty>
      ) : (
        <>
          <Card className="divide-y">
            {list.data.conversations.map((c) => {
              const to = { datapackId: id, tab: "conversations" as const, itemId: c.id };
              return (
                <a
                  key={c.id}
                  href={href(to)}
                  onClick={(e) => (e.preventDefault(), navigate(to))}
                  className="flex items-center justify-between gap-4 p-3 hover:bg-accent"
                >
                  <div className="min-w-0">
                    <p className="truncate text-sm font-medium">{c.title || c.first_message || "Untitled"}</p>
                    <div className="mt-1 flex items-center gap-2 text-xs text-muted-foreground">
                      <Badge>{c.agent_id}</Badge>
                      {c.messages} turns
                      {c.memories_added > 0 && <> · {c.memories_added} memories</>}
                    </div>
                  </div>
                  <span className="shrink-0 text-xs text-muted-foreground">{timeAgo(c.last_activity_at)}</span>
                </a>
              );
            })}
          </Card>
          <div className="flex justify-end gap-2">
            <Button size="sm" disabled={offset === 0} onClick={() => setOffset(Math.max(0, offset - PAGE))}>
              Previous
            </Button>
            <Button size="sm" disabled={!list.data.has_more} onClick={() => setOffset(offset + PAGE)}>
              Next
            </Button>
          </div>
        </>
      )}
    </div>
  );
}

function Thread({ datapack, conversationId }: { datapack: Datapack; conversationId: string }) {
  const id = datapack.datapack_id;
  const { data, error, loading } = useLoad(() => api.conversation(id, conversationId), [id, conversationId]);
  const back = { datapackId: id, tab: "conversations" as const };
  const c = data?.conversation;

  return (
    <div className="space-y-4">
      <a href={href(back)} onClick={(e) => (e.preventDefault(), navigate(back))} className="inline-flex items-center gap-1 text-sm text-muted-foreground hover:text-foreground">
        <ArrowLeft className="size-4" /> All conversations
      </a>
      {loading && !c ? (
        <Loading />
      ) : error ? (
        <ErrorNote error={error} />
      ) : c ? (
        <>
          <div>
            <h2 className="text-lg font-semibold">{c.title || "Untitled"}</h2>
            <p className="text-xs text-muted-foreground">
              {c.agent_id} · started {dateTime(c.started_at)} · {c.messages} turns
            </p>
          </div>
          <div className="space-y-3">
            {c.thread.map((t) => (
              <Card key={t.message_id} className="space-y-3 p-4">
                <div className="text-xs text-muted-foreground">{dateTime(t.started_at)}</div>
                {t.input && (
                  <div>
                    <div className="mb-1 text-xs font-medium text-muted-foreground">User</div>
                    <p className="whitespace-pre-wrap text-sm">{t.input}</p>
                  </div>
                )}
                {t.output && (
                  <div>
                    <div className="mb-1 text-xs font-medium text-muted-foreground">Agent</div>
                    <p className="whitespace-pre-wrap text-sm">{t.output}</p>
                  </div>
                )}
                {t.memory_texts.length > 0 && (
                  <div className="space-y-1 rounded-lg bg-muted p-2">
                    {t.memory_texts.map((m, i) => (
                      <p key={i} className="flex items-start gap-1.5 text-xs">
                        <Brain className="mt-0.5 size-3 shrink-0 text-primary" />
                        {m}
                      </p>
                    ))}
                  </div>
                )}
              </Card>
            ))}
            {c.thread_has_more && <p className="text-center text-xs text-muted-foreground">Showing the first 100 turns.</p>}
          </div>
        </>
      ) : null}
    </div>
  );
}
