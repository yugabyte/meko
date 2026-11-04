// A datapack at a glance: what to do first in a new one, how knowledge moves through it, what it
// holds (each count opening its tab), the questions Meko couldn't answer (opening Context Gaps), how
// it's used, and the last 30 days of searches.

import type { ReactNode } from "react";
import { Brain, ChevronRight, FileText, HelpCircle, MessagesSquare, Package, Search, Sparkles, Users } from "lucide-react";
import { api } from "../lib/api";
import { bytes, count, timeAgo } from "../lib/format";
import { href, navigate, type Tab } from "../lib/route";
import type { Datapack } from "../lib/types";
import { useLoad } from "../lib/useLoad";
import { ExtractionBanner } from "../components/ExtractionBanner";
import { GetStarted } from "../components/GetStarted";
import { ContextTiles } from "../components/metrics/ContextTiles";
import { derive } from "../lib/metrics";
import { LearningPipeline } from "../components/LearningPipeline";
import { Card, ErrorNote, Loading } from "../components/ui";
import { SHOW_COLLECTIVE } from "../lib/features";

// A count, opening tab (with itemId, e.g. the memories tab's scope) when clicked: a real link, so
// it opens in a new tab too.
function Stat({
  icon,
  label,
  value,
  hint,
  to,
}: {
  icon: ReactNode;
  label: string;
  value: string;
  hint?: string;
  to: { datapackId: string; tab: Tab; itemId?: string };
}) {
  return (
    <a
      href={href(to)}
      onClick={(e) => {
        if (e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0) return;
        e.preventDefault();
        navigate(to);
      }}
      className="group rounded-lg focus-visible:outline-2 focus-visible:outline-primary"
    >
      <Card className="h-full p-4 transition-colors group-hover:border-primary/60 group-hover:bg-accent/40">
        <div className="flex items-center gap-2 text-xs text-muted-foreground">
          {icon}
          {label}
        </div>
        <div className="mt-2 text-2xl font-semibold tabular-nums">{value}</div>
        {hint && <div className="mt-0.5 text-xs text-muted-foreground">{hint}</div>}
      </Card>
    </a>
  );
}

export function OverviewPage({ datapack }: { datapack: Datapack }) {
  const id = datapack.datapack_id;
  const { data: m, error, loading } = useLoad(() => api.metrics(id, 0), [id]);
  if (loading && !m) return <Loading />;
  if (error) return <ErrorNote error={error} />;
  if (!m) return null;

  const days = m.daily_activity ?? [];
  const at = (tab: Tab, itemId?: string) => ({ datapackId: id, tab, itemId });
  const peak = Math.max(1, ...days.map((d) => d.hybrid + d.kb + d.memory));

  return (
    <div className="space-y-6">
      <ExtractionBanner datapack={datapack} />
      <ContextTiles d={derive(m, datapack)} />
      <GetStarted datapack={datapack} metrics={m} />
      {SHOW_COLLECTIVE && <LearningPipeline datapack={datapack} />}
      <GapsLine datapackId={id} />
      <div className="grid grid-cols-2 gap-3 lg:grid-cols-3">
        <Stat
          icon={<Brain className="size-3.5" />}
          label="Memories"
          value={count(datapack.memory_count)}
          to={at("memories")}
          hint={SHOW_COLLECTIVE ? `${count(datapack.learnings_count)} learnings · ${bytes(m.memory_bytes)}` : bytes(m.memory_bytes)}
        />
        <Stat
          icon={<FileText className="size-3.5" />}
          label="Knowledge files"
          value={count(m.kb_file_count)}
          to={at("knowledge")}
          hint={`${count(m.kb_chunk_count)} chunks · ${bytes(m.kb_bytes)}`}
        />
        <Stat
          icon={<MessagesSquare className="size-3.5" />}
          label="Conversations"
          value={count(m.conversation_count)}
          to={at("conversations")}
        />
        <Stat
          icon={<Package className="size-3.5" />}
          label="Artifacts"
          value={count(m.artifact_count ?? 0)}
          to={at("artifacts")}
        />
        {SHOW_COLLECTIVE && (
          <Stat
            icon={<Sparkles className="size-3.5" />}
            label="Collective memory"
            value={count(datapack.collective_memory_count)}
            to={at("memories", "promoted")}
            hint="Promoted learnings, searchable in the knowledge base"
          />
        )}
        <Stat
          icon={<Search className="size-3.5" />}
          label="Searches (90 days)"
          value={count(m.total_requests)}
          to={at("metrics")}
          hint={`${count(m.tokens_saved_total)} tokens saved · last ${timeAgo(m.last_accessed_at)}`}
        />
        <Stat icon={<Users className="size-3.5" />} label="Agents" value={count(m.agent_count)} to={at("metrics")} />
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

// "9 questions Meko couldn't answer · 5 found nothing · 4 nothing relevant", opening Context Gaps,
// while any is open. Nothing at all when there are none, or the server has no such route.
export function GapsLine({ datapackId }: { datapackId: string }) {
  const { data } = useLoad(() => api.gaps(datapackId, "open").catch(() => undefined), [datapackId]);
  const open = data ? data.open.empty + data.open.weak : 0;
  if (!data || open === 0) return null;
  const to = { datapackId, tab: "gaps" as const };
  const split = [
    data.open.empty > 0 && `${data.open.empty.toLocaleString()} found nothing`,
    data.open.weak > 0 && `${data.open.weak.toLocaleString()} nothing relevant`,
  ].filter(Boolean);
  return (
    <a
      href={href(to)}
      onClick={(e) => {
        if (e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0) return;
        e.preventDefault();
        navigate(to);
      }}
      className="group flex flex-wrap items-center gap-x-2 gap-y-1 rounded-lg border bg-card px-4 py-3 text-sm transition-colors hover:border-primary/60 hover:bg-accent/40 focus-visible:outline-2 focus-visible:outline-primary"
    >
      <HelpCircle className="size-4 text-warning" aria-hidden />
      <span className="font-medium">
        {open.toLocaleString()} {open === 1 ? "question" : "questions"} Meko couldn't answer
      </span>
      {split.length > 0 && <span className="text-muted-foreground">· {split.join(" · ")}</span>}
      <span className="ml-auto inline-flex items-center gap-0.5 text-muted-foreground group-hover:text-foreground">
        Context Gaps <ChevronRight className="size-3.5" />
      </span>
    </a>
  );
}
