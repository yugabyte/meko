import { MarkdownMessage } from "@/components/markdown-message";
import { formatMemoryAuthor, type Memory } from "@/lib/api/mekoApi";
import { formatRelativeTime, formatUtcAbsolute, useNowMs } from "@/lib/relativeTime";
import { cn } from "@/lib/utils";

type Props = {
  memory: Memory;
  /** e.g. line-through for promoted rows */
  textClassName?: string;
};

/** Shared memory text + agent + author line (Learnings panel + read-only Memory summary).
 *  Memory text is rendered as markdown so extraction-written structure (bullets,
 *  line breaks, inline code, links) is honoured on the review page — a long
 *  semicolon-separated single paragraph is unreviewable (MEKO-824). Plain text
 *  with no markdown tokens round-trips unchanged. */
export function MemoryRowBody({ memory, textClassName }: Props) {
  const data = memory.data ?? "";
  const agent = memory.agent_id ?? "";
  const nowMs = useNowMs();
  const createdLabel = memory.created_at ? formatRelativeTime(memory.created_at, nowMs) : null;
  const authorName = formatMemoryAuthor(memory);
  const hasAuthor = Boolean(memory.user_id || memory.user_name || memory.user_email);
  return (
    <div className="min-w-0 space-y-1">
      {data ? (
        <div className={cn("text-sm text-foreground break-words", textClassName)}>
          <MarkdownMessage text={data} />
        </div>
      ) : (
        <p className={cn("text-sm text-foreground break-words", textClassName)}>
          <span className="italic text-muted-foreground">(empty)</span>
        </p>
      )}
      <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-[11px] text-muted-foreground">
        {/* Viewers aren't told who added a memory, so there is no author. */}
        {hasAuthor ? (
          <span>
            by: <span className="text-purple-700 dark:text-purple-200">{authorName}</span>
          </span>
        ) : null}
        {agent ? (
          <span>
            agent: <code className="text-blue-700 dark:text-blue-300">{agent}</code>
          </span>
        ) : null}
        {createdLabel ? (
          <span title={memory.created_at}>
            {createdLabel}
            <span className="text-muted-foreground/80">
              {" · "}
              {formatUtcAbsolute(memory.created_at)}
            </span>
          </span>
        ) : null}
      </div>
    </div>
  );
}
