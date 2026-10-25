// A datapack at a glance: what to do first in a new one, how knowledge moves through it, what it
// holds, how it's used, and the last 30 days of searches.

import type { ReactNode } from "react";
import { Brain, FileText, MessagesSquare, Search, Sparkles, Users } from "lucide-react";
import { api } from "../lib/api";
import { bytes, count, timeAgo } from "../lib/format";
import type { Datapack } from "../lib/types";
import { useLoad } from "../lib/useLoad";
import { GetStarted } from "../components/GetStarted";
import { LearningPipeline } from "../components/LearningPipeline";
import { Card, ErrorNote, Loading } from "../components/ui";
import { SHOW_COLLECTIVE } from "../lib/features";

function Stat({ icon, label, value, hint }: { icon: ReactNode; label: string; value: string; hint?: string }) {
  return (
    <Card className="p-4">
      <div className="flex items-center gap-2 text-xs text-muted-foreground">
        {icon}
        {label}
      </div>
      <div className="mt-2 text-2xl font-semibold tabular-nums">{value}</div>
      {hint && <div className="mt-0.5 text-xs text-muted-foreground">{hint}</div>}
    </Card>
  );
}

export function OverviewPage({ datapack }: { datapack: Datapack }) {
  const id = datapack.datapack_id;
  const { data: m, error, loading } = useLoad(() => api.metrics(id, 0), [id]);
  if (loading && !m) return <Loading />;
  if (error) return <ErrorNote error={error} />;
  if (!m) return null;

  const days = m.daily_activity ?? [];
  const peak = Math.max(1, ...days.map((d) => d.hybrid + d.kb + d.memory));

  return (
    <div className="space-y-6">
      <GetStarted datapack={datapack} metrics={m} />
      {SHOW_COLLECTIVE && <LearningPipeline datapack={datapack} />}
      <div className="grid grid-cols-2 gap-3 lg:grid-cols-3">
        <Stat
          icon={<Brain className="size-3.5" />}
          label="Memories"
          value={count(datapack.memory_count)}
          hint={SHOW_COLLECTIVE ? `${count(datapack.learnings_count)} learnings · ${bytes(m.memory_bytes)}` : bytes(m.memory_bytes)}
        />
        <Stat
          icon={<FileText className="size-3.5" />}
          label="Knowledge files"
          value={count(m.kb_file_count)}
          hint={`${count(m.kb_chunk_count)} chunks · ${bytes(m.kb_bytes)}`}
        />
        <Stat
          icon={<MessagesSquare className="size-3.5" />}
          label="Conversations"
          value={count(m.conversation_count)}
        />
        {SHOW_COLLECTIVE && (
          <Stat
            icon={<Sparkles className="size-3.5" />}
            label="Collective memory"
            value={count(datapack.collective_memory_count)}
            hint="Promoted learnings, searchable in the knowledge base"
          />
        )}
        <Stat
          icon={<Search className="size-3.5" />}
          label="Searches (90 days)"
          value={count(m.total_requests)}
          hint={`${count(m.tokens_saved_total)} tokens saved · last ${timeAgo(m.last_accessed_at)}`}
        />
        <Stat icon={<Users className="size-3.5" />} label="Agents" value={count(m.agent_count)} />
      </div>

      <Card className="p-4">
        <div className="mb-3 text-sm font-medium">Searches, last 30 days</div>
        {days.length === 0 ? (
          <p className="py-6 text-center text-sm text-muted-foreground">No searches yet.</p>
        ) : (
          <div className="flex h-28 items-end gap-1">
            {days.map((d) => {
              const n = d.hybrid + d.kb + d.memory;
              return (
                <div
                  key={d.date}
                  title={`${d.date}: ${n} searches`}
                  className="flex-1 rounded-t bg-primary/70 hover:bg-primary"
                  style={{ height: `${Math.max(4, (n / peak) * 100)}%` }}
                />
              );
            })}
          </div>
        )}
      </Card>
    </div>
  );
}
