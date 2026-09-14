// What a review actually read: the knowledge chunks and memories it pulled
// back, colour-coded the way the runner's context pane codes them.
//
// Two readings of the same trace. `detail` sits next to the retrieval counts
// on the cost step and answers "what were those 2 chunks?", grouped by
// document. `citations` sits under the findings and answers "where did these
// comments come from?", one quoted rule per row.
//
// No similarity score on either. The trace's `score` is 1 - cosine distance,
// derived correctly, but the distances this embedding space returns sit at
// ~0.99 even for the chunk that plainly drove a comment -- so the percentage
// lands at 0-1% and reads as a bug. Order carries the real signal: the
// gateway sorts hits closest-first before they get here.
//
// Deliberately NOT presented as a per-finding citation in either. The
// decision trace reports what the turn retrieved, not which chunk produced
// which comment, so this says "grounded in these" rather than attaching a
// rule to a line.

import { Badge } from "@/components/ui/badge";
import type { KbHit, MemoryHit } from "@/lib/inferenceGateway";

import { LabContextSection } from "../lab-runner/LabContextSection";

/** Chunks arrive flat; the reader thinks in documents. */
function groupByDocument(hits: KbHit[]): { name: string; hits: KbHit[] }[] {
  const byDoc = new Map<string, KbHit[]>();
  for (const hit of hits) {
    const name = hit.document_name?.trim() || hit.document_id || "Unknown document";
    byDoc.set(name, [...(byDoc.get(name) ?? []), hit]);
  }
  return Array.from(byDoc.entries()).map(([name, docHits]) => ({ name, hits: docHits }));
}

function memoryScope(hit: MemoryHit): string {
  return hit.kind === "promoted" ? "collective" : hit.kind === "graph" ? "relation" : "private";
}

/** One quoted source under a set of findings: a soft-tinted pill naming the
 *  source kind, then where it came from and what it said. */
function Citation({
  kind,
  pill,
  source,
  quote,
}: {
  kind: "knowledge" | "memory";
  pill: string;
  source: string;
  quote: string;
}) {
  const tint =
    kind === "knowledge"
      ? "border-context-knowledge/25 bg-context-knowledge-soft text-context-knowledge"
      : "border-context-memory/25 bg-context-memory-soft text-context-memory";
  const name = kind === "knowledge" ? "text-context-knowledge" : "text-context-memory";

  return (
    <div className="flex items-start gap-3 rounded-md border border-border bg-card p-3">
      <Badge variant="outline" className={`shrink-0 border ${tint}`}>
        {pill}
      </Badge>
      <div className="min-w-0 flex-1">
        <p className={`min-w-0 truncate font-mono text-xs ${name}`}>{source}</p>
        <p className="mt-1 text-xs leading-relaxed text-foreground">“{quote}”</p>
      </div>
    </div>
  );
}

export function ReviewRetrievalSources({
  kbHits,
  memoryHits,
  variant = "detail",
}: {
  kbHits: KbHit[];
  memoryHits: MemoryHit[];
  variant?: "detail" | "citations";
}) {
  const documents = groupByDocument(kbHits);

  if (variant === "citations") {
    // Nothing retrieved is itself the lesson here: the comments above came
    // from the model alone, which is what the datapack is meant to change.
    if (kbHits.length === 0 && memoryHits.length === 0) {
      return (
        <p className="rounded-md border border-dashed border-border px-3 py-2.5 text-xs text-muted-foreground">
          This review matched nothing in your datapack, so every comment above is the model on its own.
        </p>
      );
    }
    return (
      <div className="flex flex-col gap-2">
        {kbHits.map((hit, i) => (
          <Citation
            key={`k-${i}`}
            kind="knowledge"
            pill="knowledge"
            source={hit.document_name?.trim() || hit.document_id || "Unknown document"}
            quote={hit.chunk_text}
          />
        ))}
        {memoryHits.map((hit, i) => (
          <Citation
            key={`m-${i}`}
            kind="memory"
            pill="memory"
            source={`${memoryScope(hit)} memory`}
            quote={hit.text}
          />
        ))}
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-5">
      <LabContextSection
        kind="knowledge"
        label="Knowledge it read"
        count={kbHits.length}
        emptyHint="This review didn't match any chunk in the knowledge base."
      >
        {documents.map((doc) => (
          <div key={doc.name} className="flex flex-col gap-1.5 rounded-md bg-card p-3">
            <div className="flex items-center justify-between gap-2">
              <p className="min-w-0 truncate font-mono text-xs text-context-knowledge">{doc.name}</p>
              <Badge variant="secondary" className="shrink-0 text-[10px]">
                {doc.hits.length} chunk{doc.hits.length === 1 ? "" : "s"}
              </Badge>
            </div>
            {doc.hits.map((hit, i) => (
              <div key={i} className="border-l-2 border-context-knowledge/40 pl-3">
                <p className="text-xs leading-relaxed text-foreground">{hit.chunk_text}</p>
              </div>
            ))}
          </div>
        ))}
      </LabContextSection>

      <LabContextSection
        kind="memory"
        label="Memories it read"
        count={memoryHits.length}
        emptyHint="This review didn't match any memory in this datapack."
      >
        {memoryHits.map((hit, i) => (
          <div key={i} className="flex flex-col gap-1 rounded-md bg-card p-3">
            <p className="text-xs leading-relaxed text-foreground">{hit.text}</p>
            <p className="text-[11px] text-muted-foreground">{memoryScope(hit)}</p>
          </div>
        ))}
      </LabContextSection>
    </div>
  );
}
