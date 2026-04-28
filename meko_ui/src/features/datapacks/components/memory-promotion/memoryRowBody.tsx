import type { Memory } from "@/lib/mekoApi";
import { cn } from "@/lib/utils";

type Props = {
  memory: Memory;
  /** e.g. line-through for promoted rows */
  textClassName?: string;
};

/** Shared memory text + agent line (Learnings panel + read-only Memory summary). */
export function MemoryRowBody({ memory, textClassName }: Props) {
  const data = memory.data ?? "";
  const agent = memory.agent_id ?? "";
  return (
    <div className="min-w-0 space-y-1">
      <p className={cn("text-sm text-white break-words", textClassName)}>
        {data || <span className="italic text-muted-foreground">(empty)</span>}
      </p>
      {agent ? (
        <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-[11px] text-muted-foreground">
          <span>
            agent: <code className="text-blue-300">{agent}</code>
          </span>
        </div>
      ) : null}
    </div>
  );
}
