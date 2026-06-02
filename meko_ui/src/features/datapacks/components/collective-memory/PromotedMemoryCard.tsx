// Rich card view for a single PromotedMemory.
//
// Layout follows the SharedKnowledge "Promoted memories" tab design:
//   row 1 (chips):  "Me → Ko"  ·  agent pill
//   row 1 (right):  "promoted <relative> by <email>"  ·  overflow menu
//   body:           SHARED KNOWLEDGE ENTRY section with m.data
//   collapsible:    Source memory · <id-prefix>  → conversation link + conv_id
//   footer:         vector entries · embed dimensions · embed model · ai provider
//
// All envelope-level metadata (embedding model / dimensions / ai provider)
// comes from GET /knowledge-bases/summary as props, NOT from the
// /promoted-memories row — single source of truth for index-level
// constants. The row carries only per-row data (data, agent_id,
// conversation_id, promoted_to_kb_at, promoted_by_name, promoted_by_email).
//
// Vector entries: always exactly 1. `Promote()` writes one row to
// `datapacks.knowledge_bases` per promoted memory (single chunk, single
// embedding copied from mem0), so the count is a hard-coded constant
// rather than a per-row correlated subquery against the chunk store.

import { useState } from "react";
import { Link } from "react-router-dom";
import {
  BookOpen,
  ChevronDown,
  ChevronUp,
  Cpu,
  Hash,
  Layers,
  MessageSquare,
  Sparkles,
  SquareArrowOutUpRight,
} from "lucide-react";
import {
  CONVERSATION_SOURCE,
  datapackConversationsPathWithSource,
} from "@/app/routes/paths";
import { formatRelativeTime, formatUtcAbsolute, useNowMs } from "@/lib/relativeTime";
import type { PromotedMemory } from "@/lib/mekoApi";

type Props = {
  memory: PromotedMemory;
  datapackName: string;
  /** Envelope-level constants from /knowledge-bases/summary. All
   *  nullable; when null the footer chip is omitted instead of
   *  showing an empty value. */
  embeddingModel: string | null;
  embeddingDimensions: number | null;
  aiProvider: string | null;
};

export function PromotedMemoryCard({
  memory,
  datapackName,
  embeddingModel,
  embeddingDimensions,
  aiProvider,
}: Props) {
  const nowMs = useNowMs();
  const [sourceOpen, setSourceOpen] = useState(false);

  const data = memory.data ?? "";
  const agent = memory.agent_id ?? "";
  const conversationId = memory.conversation_id ?? "";
  const promotedAt = memory.promoted_to_kb_at ?? "";
  // Prefer the full name when available, otherwise fall back to email.
  // Mirrors the name → email → … fallback chain used everywhere else
  // memory authors are rendered.
  const promotedBy = (memory.promoted_by_name ?? "").trim() || (memory.promoted_by_email ?? "").trim();
  // First 8 chars of memory id — enough to disambiguate in the UI
  // without consuming a whole row. Matches the "<id-prefix>" pattern
  // used by the conversations viewer.
  const memoryIdPrefix = memory.id.split("-")[0] ?? memory.id.slice(0, 8);

  return (
    <article className="rounded-xl border border-border bg-input p-4 space-y-4">
      {/* Top row: identity chips on the left, promotion meta on the right. */}
      <header className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex flex-wrap items-center gap-2">
          {/* "Me → Ko" identity pill. Me = mem0 (raw memory), Ko = the
              KnowledgeObject for this datapack. Purple-tinted to match
              the Sparkles accent the rest of the tab uses. */}
          <span className="inline-flex items-center gap-1 rounded-full border border-purple-500/30 bg-purple-500/10 px-2 py-0.5 text-[11px] font-medium text-purple-700 dark:text-purple-200">
            Me <span aria-hidden>→</span> Ko
          </span>
          {agent ? (
            <span
              title={`Agent: ${agent}`}
              className="inline-flex items-center gap-1 rounded-full border border-border bg-background/40 px-2 py-0.5 text-[11px] font-medium text-foreground/85"
            >
              <Cpu className="h-3 w-3 text-sky-700 dark:text-sky-300" aria-hidden />
              {agent}
            </span>
          ) : null}
        </div>
        <div className="flex items-center gap-2 text-[11px] text-muted-foreground">
          {promotedAt ? (
            <span title={promotedAt}>
              promoted{" "}
              <span className="text-foreground/85">
                {formatRelativeTime(promotedAt, nowMs)}
              </span>
              <span className="text-muted-foreground/80">
                {" · "}{formatUtcAbsolute(promotedAt)}
              </span>
              {promotedBy ? (
                <>
                  {" "}
                  by <span className="text-foreground/85">{promotedBy}</span>
                </>
              ) : null}
            </span>
          ) : promotedBy ? (
            <span>
              promoted by <span className="text-foreground/85">{promotedBy}</span>
            </span>
          ) : null}
        </div>
      </header>

      {/* The promoted memory itself. Emerald accent ring + label matches
          the SharedKnowledge color language (BookOpen, file-complete pills). */}
      <section className="rounded-lg border border-emerald-500/20 bg-emerald-500/[0.03] p-3">
        <div className="mb-2 flex items-center gap-1.5">
          <BookOpen className="h-3.5 w-3.5 text-emerald-700 dark:text-emerald-300" aria-hidden />
          <span className="text-[10px] font-semibold uppercase tracking-wider text-emerald-700 dark:text-emerald-200/95">
            Shared knowledge entry
          </span>
        </div>
        <p className="text-sm leading-relaxed text-foreground break-words">
          {data || <span className="italic text-muted-foreground">(empty)</span>}
        </p>
      </section>

      {/* Source memory drawer. Collapsed by default so the card stays
          compact; expand to inspect provenance. Only rendered when we
          have something useful to show (conv id or memory id). */}
      {(conversationId || memoryIdPrefix) ? (
        <section className="rounded-lg border border-border bg-background/30">
          <button
            type="button"
            onClick={() => setSourceOpen((v) => !v)}
            className="flex w-full items-center justify-between gap-2 px-3 py-2 text-left text-[11px] text-muted-foreground hover:text-foreground"
            aria-expanded={sourceOpen}
          >
            <span className="flex items-center gap-1.5">
              <Sparkles className="h-3.5 w-3.5 text-sky-700 dark:text-sky-300" aria-hidden />
              <span className="font-semibold uppercase tracking-wider text-foreground/80">
                Source memory
              </span>
              <code className="font-mono text-foreground/70">· {memoryIdPrefix}</code>
            </span>
            {sourceOpen ? (
              <ChevronUp className="h-3.5 w-3.5" aria-hidden />
            ) : (
              <ChevronDown className="h-3.5 w-3.5" aria-hidden />
            )}
          </button>
          {sourceOpen ? (
            // Expanded body: mirrors the SharedKnowledge "Original
            // Memory (Me)" design — small uppercase label, the raw
            // memory text in italic quotes, then a "From conversation"
            // row that links back to the chat with the external-link
            // icon, plus a truncated conv_id underneath for copy/paste.
            //
            // Note: our system stores only one copy of the data, so
            // the "Original Memory" body and the "Shared knowledge
            // entry" body are textually identical today — promotions
            // currently don't transform the text. The repeated render
            // is intentional: it surfaces the source-of-record so
            // users can spot future transforms (auto-summary, PII
            // scrubbing, etc.) when they land.
            <div className="space-y-3 border-t border-border/60 px-3 py-3 text-[11px] text-muted-foreground">
              <div>
                <div className="mb-1.5 text-[10px] font-semibold uppercase tracking-wider text-purple-700 dark:text-purple-200/95">
                  Original memory (Me)
                </div>
                <p className="text-sm italic leading-relaxed text-foreground/90 break-words">
                  {data ? (
                    <>&ldquo;{data}&rdquo;</>
                  ) : (
                    <span className="not-italic text-muted-foreground">(empty)</span>
                  )}
                </p>
              </div>

              {conversationId ? (
                <div className="space-y-1">
                  <Link
                    to={datapackConversationsPathWithSource(
                      datapackName,
                      conversationId,
                      CONVERSATION_SOURCE.collectiveMemorySummary,
                    )}
                    title={`Open conversation ${conversationId}`}
                    className="inline-flex items-center gap-1.5 text-sky-700 dark:text-sky-300 hover:underline"
                  >
                    <MessageSquare className="h-3.5 w-3.5" aria-hidden />
                    <span>
                      From conversation:{" "}
                      <span className="text-sky-700 dark:text-sky-200">
                        {conversationId.split("-")[0] ?? conversationId.slice(0, 8)}
                      </span>
                    </span>
                    <SquareArrowOutUpRight className="h-3 w-3" aria-hidden />
                  </Link>
                  <div className="text-foreground/55">
                    conv_id <code className="font-mono">{truncateMiddle(conversationId, 18)}</code>
                  </div>
                </div>
              ) : null}

              <div className="text-foreground/55">
                memory_id <code className="font-mono">{truncateMiddle(memory.id, 18)}</code>
              </div>
            </div>
          ) : null}
        </section>
      ) : null}

      {/* Footer chips: per-row constant (vector entries) + index-level
          provenance from /summary (so the SAME embed-* values render on
          every card). */}
      <footer className="flex flex-wrap items-center gap-x-4 gap-y-1.5 border-t border-border/50 pt-3 text-[11px] text-muted-foreground">
        {/* Always 1 — see the file-header comment. Hardcoded rather than
            queried because `Promote()` writes exactly one KB row per
            promoted memory; an N+1 correlated COUNT on every list page
            isn't worth a value that's a constant. */}
        <span className="inline-flex items-center gap-1.5">
          <Layers className="h-3.5 w-3.5 text-sky-700 dark:text-sky-300" aria-hidden />
          <span className="text-foreground/85">1</span> vector entry
        </span>
        {embeddingDimensions !== null && embeddingDimensions !== undefined ? (
          <span className="inline-flex items-center gap-1.5">
            <Hash className="h-3.5 w-3.5 text-purple-700 dark:text-purple-300" aria-hidden />
            <span className="text-foreground/85">{embeddingDimensions}-dim</span>
          </span>
        ) : null}
        {embeddingModel ? (
          <span
            className="inline-flex items-center gap-1.5"
            title={aiProvider ? `Provider: ${aiProvider}` : undefined}
          >
            <Cpu className="h-3.5 w-3.5 text-emerald-700 dark:text-emerald-300" aria-hidden />
            <span className="text-foreground/85">{embeddingModel}</span>
          </span>
        ) : null}
      </footer>
    </article>
  );
}

// truncateMiddle keeps the first `head` chars and trails an ellipsis.
// We don't render full 36-char UUIDs in the card body — they wrap to
// two lines and steal visual weight from the actual memory text.
// `head=18` shows the first segment of a UUID (8-4-4-…) which is more
// than enough to disambiguate while staying on one line.
function truncateMiddle(value: string, head: number): string {
  if (!value) return value;
  if (value.length <= head + 1) return value;
  return `${value.slice(0, head)}…`;
}
