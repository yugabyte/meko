// What a review actually read: the knowledge chunks and memories it pulled
// back, colour-coded the way the runner's context pane codes them.
//
// Sits under the findings and answers "where did these comments come from?",
// one quoted rule per row. The cost step reports how MANY chunks and memories
// reached the model; this is the only place their content appears.
//
// No similarity score. The trace's `score` is 1 - cosine distance, derived
// correctly, but the distances this embedding space returns sit at ~0.99 even
// for the chunk that plainly drove a comment -- so the percentage lands at
// 0-1% and reads as a bug. Order carries the real signal: the gateway sorts
// hits closest-first before they get here.
//
// Deliberately NOT presented as a per-finding citation. The decision trace
// reports what the turn retrieved, not which chunk produced which comment, so
// this says "grounded in these" rather than attaching a rule to a line.

import type { KbHit, MemoryHit } from "@/lib/inferenceGateway";

import { ContextCitation } from "./ContextCitation";

function memoryScope(hit: MemoryHit): string {
  return hit.kind === "promoted" ? "collective" : hit.kind === "graph" ? "relation" : "private";
}

export function ReviewRetrievalSources({
  kbHits,
  memoryHits,
}: {
  kbHits: KbHit[];
  memoryHits: MemoryHit[];
}) {
  // Nothing retrieved is itself the lesson: the comments above came from the
  // model alone, which is what the datapack is meant to change.
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
        <ContextCitation
          key={`k-${i}`}
          kind="knowledge"
          source={hit.document_name?.trim() || hit.document_id || "Unknown document"}
          quote={hit.chunk_text}
        />
      ))}
      {memoryHits.map((hit, i) => (
        <ContextCitation
          key={`m-${i}`}
          kind="memory"
          source={`${memoryScope(hit)} memory`}
          quote={hit.text}
        />
      ))}
    </div>
  );
}
