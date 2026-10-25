// How knowledge moves through a datapack, left to right with counts: conversations, the memories
// saved from them, learnings waiting to be kept or promoted, and the collective memory every agent
// shares. Each stage opens the tab that holds it.

import { ArrowRight, Brain, MessagesSquare, Sparkles, Users } from "lucide-react";
import type { ReactNode } from "react";
import { count } from "../lib/format";
import { navigate, type Tab } from "../lib/route";
import type { Datapack } from "../lib/types";
import { SHOW_COLLECTIVE } from "../lib/features";
import { Card } from "./ui";

export function LearningPipeline({ datapack }: { datapack: Datapack }) {
  const stages: { icon: ReactNode; label: string; n?: number; tab: Tab; itemId?: string }[] = [
    {
      icon: <MessagesSquare className="size-4" />,
      label: "Conversations",
      n: datapack.conversation_count,
      tab: "conversations",
    },
    { icon: <Brain className="size-4" />, label: "Memories", n: datapack.memory_count, tab: "memories" },
    {
      icon: <Sparkles className="size-4" />,
      label: "Learnings",
      n: datapack.learnings_count,
      tab: "memories",
      itemId: "learnings",
    },
  ];
  if (SHOW_COLLECTIVE) {
    stages.push({
      icon: <Users className="size-4" />,
      label: "Collective memory",
      n: datapack.collective_memory_count,
      tab: "memories",
      itemId: "promoted",
    });
  }
  return (
    <Card className="flex flex-wrap items-center gap-2 p-3">
      {stages.map((s, i) => (
        <div key={s.label} className="flex items-center gap-2">
          {i > 0 && <ArrowRight className="size-4 text-muted-foreground" />}
          <button
            type="button"
            onClick={() => navigate({ datapackId: datapack.datapack_id, tab: s.tab, itemId: s.itemId })}
            className="flex items-center gap-2 rounded-lg px-3 py-2 hover:bg-accent"
          >
            <span className="text-primary">{s.icon}</span>
            <span className="text-lg font-semibold tabular-nums">{count(s.n)}</span>
            <span className="text-xs text-muted-foreground">{s.label}</span>
          </button>
        </div>
      ))}
    </Card>
  );
}
