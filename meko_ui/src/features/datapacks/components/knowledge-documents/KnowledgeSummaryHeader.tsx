// Top of the SharedKnowledge summary page: title row with the "Ko"
// chip, the auto-generated description tile, and the 4-stat tile
// (vector entries / files / promoted memories / last activity).
//
// Counts + description come from `GET /knowledge-bases/summary`. The
// "Last activity" cell prefers the most-recent timestamp seen in the
// activity log (file upload or memory promotion); we fall back to the
// summary's `last_file_uploaded_at` when the log hasn't loaded yet.

import { BookOpen, Clock, FileText, Layers, Sparkles } from "lucide-react";
import { cn } from "@/lib/utils";
import { formatRelativeTime, useNowMs } from "@/lib/relativeTime";
import type { KnowledgeBaseSummary } from "@/lib/api/mekoApi";

type Props = {
  datapackName: string;
  summary: KnowledgeBaseSummary | null;
  loading: boolean;
  /** Most-recent activity-log timestamp (file upload OR promotion).
   *  Used in preference to summary.last_file_uploaded_at when present
   *  because it accounts for promotions too. */
  lastActivityAt?: string | null;
};

export function KnowledgeSummaryHeader({
  datapackName,
  summary,
  loading,
  lastActivityAt,
}: Props) {
  // One tick-per-minute timer shared across this subtree so the
  // "Last activity" cell stays current. Cheap.
  const now = useNowMs();

  // Prefer the activity-log-derived timestamp (covers both file
  // uploads and memory promotions). Fall back to the summary's
  // last_file_uploaded_at while the activity log is still loading.
  const lastActivityIso = lastActivityAt ?? summary?.last_file_uploaded_at ?? null;

  return (
    <div className="space-y-6">
      <TitleRow datapackName={datapackName} />
      <DescriptionTile description={summary?.description ?? ""} loading={loading} />
      <StatsTile
        summary={summary}
        loading={loading}
        lastActivityIso={lastActivityIso}
        nowMs={now}
      />
    </div>
  );
}

function TitleRow({ datapackName }: { datapackName: string }) {
  return (
    <div className="space-y-2">
      <div className="flex flex-wrap items-center gap-3">
        <BookOpen className="h-8 w-8 shrink-0 text-emerald-700 dark:text-emerald-300" aria-hidden />
        <h1 className="truncate text-2xl font-semibold tracking-tight text-foreground sm:text-3xl">
          Shared Knowledge
        </h1>
        {/* "Ko" pill: short label for the knowledge object, matches the
            design spec. Bright outline + transparent fill so it reads
            as identity, not status. */}
        <span className="inline-flex items-center rounded-full border border-sky-500/40 bg-sky-500/10 px-2 py-0.5 text-[11px] font-medium uppercase tracking-wider text-sky-700 dark:text-sky-200">
          Ko
        </span>
      </div>
      <p className="max-w-2xl text-sm leading-relaxed text-muted-foreground">
        Knowledge base for{" "}
        <span className="font-medium text-foreground/90">{datapackName}</span>. Visible to every
        agent attached to this datapack.
      </p>
    </div>
  );
}

function DescriptionTile({ description, loading }: { description: string; loading: boolean }) {
  // Emerald accent matches the rest of the SharedKnowledge color
  // language (the BookOpen icon in the title is emerald; the file
  // type pills downstream lean green for COMPLETED).
  return (
    <section className="rounded-xl border border-emerald-500/25 bg-emerald-500/[0.04] p-5">
      <header className="mb-3 flex items-center gap-2">
        <Sparkles className="h-3.5 w-3.5 text-emerald-700 dark:text-emerald-300" aria-hidden />
        <span className="text-xs font-semibold uppercase tracking-wider text-emerald-700 dark:text-emerald-200/95">
          Auto-generated description
        </span>
      </header>
      {loading && !description ? (
        <p className="text-sm leading-relaxed text-muted-foreground">Loading…</p>
      ) : description ? (
        <p className="text-sm leading-relaxed text-foreground/90">{description}</p>
      ) : (
        <p className="text-sm leading-relaxed text-muted-foreground">
          Upload files to generate a description.
        </p>
      )}
    </section>
  );
}

function StatsTile({
  summary,
  loading,
  lastActivityIso,
  nowMs,
}: {
  summary: KnowledgeBaseSummary | null;
  loading: boolean;
  lastActivityIso: string | null;
  nowMs: number;
}) {
  const fmt = (n: number | null | undefined): string =>
    n == null ? (loading ? "…" : "—") : n.toLocaleString();

  // Relative-time string for the Last Activity cell. Falls back to
  // "—" when nothing has happened in this KB yet.
  const lastActivityLabel = lastActivityIso
    ? formatRelativeTime(lastActivityIso, nowMs)
    : loading
      ? "…"
      : "—";

  return (
    <section className="rounded-xl border border-border bg-card">
      <div className="grid grid-cols-1 divide-y divide-border/60 sm:grid-cols-4 sm:divide-x sm:divide-y-0">
        <StatCell
          icon={<Layers className="h-3.5 w-3.5 text-sky-700 dark:text-sky-300" aria-hidden />}
          label="Vector entries"
          value={fmt(summary?.total_chunks)}
        />
        <StatCell
          icon={<FileText className="h-3.5 w-3.5 text-emerald-700 dark:text-emerald-300" aria-hidden />}
          label="Files"
          value={fmt(summary?.total_files)}
        />
        <StatCell
          icon={<Sparkles className="h-3.5 w-3.5 text-purple-700 dark:text-purple-300" aria-hidden />}
          label="Promoted memories"
          value={fmt(summary?.promoted_memories_count)}
        />
        <StatCell
          icon={<Clock className="h-3.5 w-3.5 text-amber-700 dark:text-amber-300" aria-hidden />}
          label="Last activity"
          value={lastActivityLabel}
          valueClassName="text-base font-medium"
        />
      </div>
    </section>
  );
}

function StatCell({
  icon,
  label,
  value,
  valueClassName,
}: {
  icon: React.ReactNode;
  label: string;
  value: string;
  valueClassName?: string;
}) {
  return (
    <div className="px-5 py-4">
      <div className="flex items-center gap-1.5">
        {icon}
        <span className="text-[10px] font-semibold uppercase tracking-wider text-muted-foreground">
          {label}
        </span>
      </div>
      <div className={cn("mt-1.5 text-2xl font-bold tabular-nums text-foreground", valueClassName)}>
        {value}
      </div>
    </div>
  );
}
