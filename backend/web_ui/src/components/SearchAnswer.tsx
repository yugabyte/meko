// Search's answer: the user's own Claude Code, run by Meko with no tools, writes an answer to the
// question from the results the search returned, citing them. Off until meko answers use claude.

import { Fragment } from "react";
import { Sparkles } from "lucide-react";
import { api } from "../lib/api";
import type { ContextSearch, Passage } from "../lib/types";
import { useLoad } from "../lib/useLoad";
import { SOURCE_COLOR, SOURCE_LABEL } from "./MemoryMapView";
import { Card, cx, ErrorNote, Spinner } from "./ui";

// The most results an answer is written from (the server takes no more).
const MAX_PASSAGES = 10;

// The results to answer from: taken in turn from each source, best first, so no one source crowds
// out the others.
export function passagesOf(r: ContextSearch["results"], strongMemory: (id: string) => boolean): Passage[] {
  const lists: Passage[][] = [
    (r.memory ?? []).filter((m) => strongMemory(m.id)).map((m) => ({ source: "memory", id: m.id, text: m.memory })),
    (r.knowledge_base ?? []).map((k) => ({ source: "knowledge_base", id: k.id, text: k.chunk_text })),
    (r.conversation?.hits ?? []).map((h) => ({ source: "conversation", id: h.message_id, text: h.content })),
  ];
  const out: Passage[] = [];
  for (let i = 0; out.length < MAX_PASSAGES && lists.some((l) => i < l.length); i++)
    for (const l of lists) if (i < l.length && out.length < MAX_PASSAGES) out.push(l[i]);
  return out;
}

export function SearchAnswer({
  datapackId,
  query,
  passages,
}: {
  datapackId: string;
  query: string;
  passages: Passage[];
}) {
  const status = useLoad(() => api.answers(), []);
  if (!status.data) return null;
  if (!status.data.enabled)
    return (
      <p className="text-sm text-muted-foreground">
        <Sparkles className="mr-1 inline size-4 text-primary" />
        Get a written answer from these results with your own Claude Code: run <b>meko answers use claude</b>.
      </p>
    );
  if (passages.length === 0) return null;
  // Keyed by the search, so a new one starts from loading rather than the last answer's citations.
  const key = query + "|" + passages.map((p) => p.id).join();
  return <Answer key={key} datapackId={datapackId} query={query} passages={passages} model={status.data.model} />;
}

function Answer({
  datapackId,
  query,
  passages,
  model,
}: {
  datapackId: string;
  query: string;
  passages: Passage[];
  model: string;
}) {
  const answer = useLoad(() => api.answer(datapackId, query, passages), [datapackId]);
  return (
    <Card className="space-y-3 p-4">
      <h3 className="flex items-center gap-2 text-sm font-medium">
        <Sparkles className="size-4 text-primary" /> Answer
        <span className="font-normal text-muted-foreground">· Claude ({model}), from these results only</span>
      </h3>
      {answer.loading ? (
        <p className="flex items-center gap-2 text-sm text-muted-foreground">
          <Spinner /> Reading {passages.length} results…
        </p>
      ) : answer.error ? (
        <ErrorNote error={answer.error} />
      ) : (
        answer.data && (
          <>
            <p className="whitespace-pre-wrap text-sm leading-relaxed">
              <Cited text={answer.data.answer} passages={passages} />
            </p>
            {answer.data.cited.length > 0 && (
              <ol className="space-y-1 text-xs text-muted-foreground">
                {answer.data.cited.map((n) => {
                  const p = passages[n - 1];
                  if (!p) return null;
                  return (
                    <li key={n} className="flex items-start gap-2">
                      <span className="font-medium text-foreground">[{n}]</span>
                      <span className={cx("mt-1 size-2 shrink-0 rounded-full bg-current", SOURCE_COLOR[p.source])} />
                      <span className="min-w-0 truncate">
                        {SOURCE_LABEL[p.source]}: {p.text}
                      </span>
                    </li>
                  );
                })}
              </ol>
            )}
          </>
        )
      )}
    </Card>
  );
}

// The answer with its [n] citations as small marks that show their passage on hover.
function Cited({ text, passages }: { text: string; passages: Passage[] }) {
  const parts = text.split(/\[(\d+)\]/g);
  return (
    <>
      {parts.map((part, i) => {
        if (i % 2 === 0) return <Fragment key={i}>{part}</Fragment>;
        const p = passages[Number(part) - 1];
        return (
          <sup key={i} title={p?.text} className="ml-0.5 cursor-help font-medium text-primary">
            [{part}]
          </sup>
        );
      })}
    </>
  );
}
