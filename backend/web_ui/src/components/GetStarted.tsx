// The first things worth doing in a datapack, each ticked once it's done and each a link: connect
// an agent, add a memory, upload a file, run a search. Once all four are done it's gone for good:
// the agent and search steps come from the call log, which forgets after 90 days, so this browser
// remembers that the datapack got started.

import { useEffect } from "react";
import { CheckCircle2, Circle } from "lucide-react";
import { navigate, type Tab } from "../lib/route";
import type { Datapack, Metrics } from "../lib/types";
import { Card, cx } from "./ui";

const startedKey = (id: string) => `meko.started.${id}`;

function wasStarted(id: string): boolean {
  try {
    return localStorage.getItem(startedKey(id)) === "1";
  } catch {
    return false;
  }
}

function markStarted(id: string) {
  try {
    localStorage.setItem(startedKey(id), "1");
  } catch {
    // no storage: the card's own counts still hide it while they last
  }
}

export function GetStarted({ datapack, metrics }: { datapack: Datapack; metrics: Metrics }) {
  const steps: { label: string; done: boolean; tab: Tab }[] = [
    { label: "Connect an agent", done: metrics.agent_count > 0, tab: "connect" },
    {
      label: "Add a memory",
      done: (datapack.memory_count ?? 0) + (datapack.collective_memory_count ?? 0) > 0,
      tab: "memories",
    },
    { label: "Upload a file", done: metrics.kb_file_count > 0, tab: "knowledge" },
    { label: "Run a search", done: metrics.total_requests > 0, tab: "search" },
  ];
  const left = steps.filter((s) => !s.done).length;
  useEffect(() => {
    if (left === 0) markStarted(datapack.datapack_id);
  }, [left, datapack.datapack_id]);
  if (left === 0 || wasStarted(datapack.datapack_id)) return null;
  return (
    <Card className="p-4">
      <div className="mb-3 flex items-baseline justify-between">
        <h3 className="font-semibold">Get started</h3>
        <span className="text-xs text-muted-foreground">
          {steps.length - left} of {steps.length} done
        </span>
      </div>
      <div className="grid grid-cols-1 gap-2 sm:grid-cols-2 lg:grid-cols-4">
        {steps.map((s) => (
          <button
            key={s.label}
            type="button"
            onClick={() => navigate({ datapackId: datapack.datapack_id, tab: s.tab })}
            className={cx(
              "flex items-center gap-2 rounded-lg border px-3 py-2 text-left text-sm hover:bg-accent",
              s.done && "text-muted-foreground",
            )}
          >
            {s.done ? (
              <CheckCircle2 className="size-4 shrink-0 text-success" />
            ) : (
              <Circle className="size-4 shrink-0 text-primary" />
            )}
            <span className={cx(s.done && "line-through")}>{s.label}</span>
          </button>
        ))}
      </div>
    </Card>
  );
}
