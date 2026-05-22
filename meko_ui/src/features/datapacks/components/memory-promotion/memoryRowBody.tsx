import { formatMemoryAuthor, type Memory } from "@/lib/mekoApi";
import { cn } from "@/lib/utils";

type Props = {
  memory: Memory;
  /** e.g. line-through for promoted rows */
  textClassName?: string;
};

/** Shared memory text + agent + author line (Learnings panel + read-only Memory summary). */
export function MemoryRowBody({ memory, textClassName }: Props) {
  const data = memory.data ?? "";
  const agent = memory.agent_id ?? "";
  // Roster view: show the actual name even on your own rows (no
  // selfLabel). Fallback chain (name → email → short UUID → "-") is
  // centralised in formatMemoryAuthor.
  const authorName = formatMemoryAuthor(memory);
  return (
    <div className="min-w-0 space-y-1">
      <p className={cn("text-sm text-foreground break-words", textClassName)}>
        {data || <span className="italic text-muted-foreground">(empty)</span>}
      </p>
      <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-[11px] text-muted-foreground">
        <span>
          by: <span className="text-purple-700 dark:text-purple-200">{authorName}</span>
        </span>
        {agent ? (
          <span>
            agent: <code className="text-blue-700 dark:text-blue-300">{agent}</code>
          </span>
        ) : null}
      </div>
    </div>
  );
}
