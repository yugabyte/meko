// What a project holds, as standing totals, scoped to the datapacks the
// viewer can see. Replaces the collective-learning ribbon, which drew the
// same picture for every project.

import { Boxes, FileText, Layers, MessagesSquare, Search, Sparkles } from "lucide-react";
import { Skeleton } from "@/components/ui/skeleton";
import { StatTile } from "@/components/ui/stat-tile";
import { InlineErrorBanner } from "@/components/ui/inline-error-banner";
import { formatFileSize } from "@/lib/fileSize";
import { getApiErrorMessage } from "@/lib/api/mekoApi";
import { useProject, useProjectStats } from "@/features/projects/hooks/useProjects";
import {
  STATS_TITLE,
  KB_FILES_LABEL,
  KB_FILES_INDEXED_HINT,
} from "@/features/projects/projectsCopy";

export function ProjectStatsPanel({ projectId }: { projectId: string }) {
  const { data, isLoading, error } = useProjectStats(projectId);
  // Memory figures come from the detail query the page already made, so the
  // tile is the sum of the datapack rows rendered below it. Cached: no extra
  // request, and no second count of the memory table.
  const { data: detail } = useProject(projectId);
  const nodes = detail?.datapacks ?? [];
  const memories = nodes.reduce((acc, n) => acc + n.memory_count, 0);
  const promoted = nodes.reduce((acc, n) => acc + n.promoted_count, 0);
  const awaiting = nodes.reduce((acc, n) => acc + n.awaiting_review_count, 0);

  if (error) {
    return (
      <InlineErrorBanner>
        {getApiErrorMessage(error, "Failed to load project statistics.")}
      </InlineErrorBanner>
    );
  }

  if (isLoading || !data) {
    return (
      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
        {Array.from({ length: 6 }, (_, i) => (
          <Skeleton key={i} className="h-24 w-full rounded-xl" />
        ))}
      </div>
    );
  }

  const searches = data.searches_hybrid + data.searches_kb + data.searches_memory;

  return (
    <section className="space-y-4">
      <h2 className="text-lg font-semibold text-foreground">{STATS_TITLE}</h2>

      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
        <StatTile
          icon={<MessagesSquare className="h-3.5 w-3.5 text-context-conversation" aria-hidden />}
          label="Conversations"
          value={data.conversations.toLocaleString()}
        />
        <StatTile
          icon={<Sparkles className="h-3.5 w-3.5 text-context-memory" aria-hidden />}
          label="Memories"
          value={memories.toLocaleString()}
          hint={`${awaiting.toLocaleString()} awaiting review · ${promoted.toLocaleString()} promoted`}
        />
        <StatTile
          icon={<FileText className="h-3.5 w-3.5 text-context-knowledge" aria-hidden />}
          label={KB_FILES_LABEL}
          value={data.kb_files.toLocaleString()}
          hint={indexedHint(data.kb_files_by_type)}
        />
        <StatTile
          icon={<Layers className="h-3.5 w-3.5 text-context-knowledge" aria-hidden />}
          label="Knowledge chunks"
          value={data.kb_chunks.toLocaleString()}
          hint="what search actually matches against"
        />
        <StatTile
          icon={<Search className="h-3.5 w-3.5 text-context-learning" aria-hidden />}
          label="Searches"
          value={searches.toLocaleString()}
          hint={searchHint(data)}
        />
        <StatTile
          icon={<Boxes className="h-3.5 w-3.5 text-brand" aria-hidden />}
          label="Artifacts"
          value={data.artifacts.toLocaleString()}
          hint={data.artifact_bytes > 0 ? formatFileSize(data.artifact_bytes) : undefined}
        />
      </div>
    </section>
  );
}

/** Leads with the qualifier, because the tile counts only files that
 *  finished indexing: "indexed · 12 pdf · 8 md". */
function indexedHint(byType: Record<string, number>): string {
  const types = fileTypeHint(byType);
  return types ? `${KB_FILES_INDEXED_HINT} · ${types}` : KB_FILES_INDEXED_HINT;
}

/** "12 pdf · 8 md · 3 more", biggest first. */
function fileTypeHint(byType: Record<string, number>): string | undefined {
  const entries = Object.entries(byType ?? {}).sort((a, b) => b[1] - a[1]);
  if (entries.length === 0) return undefined;
  const shown = entries.slice(0, 3).map(([type, n]) => `${n} ${shortType(type)}`);
  const rest = entries.slice(3).reduce((acc, [, n]) => acc + n, 0);
  if (rest > 0) shown.push(`${rest} more`);
  return shown.join(" · ");
}

/** `text/markdown` reads as `markdown` in a hint that has room for one word. */
function shortType(mime: string): string {
  const tail = mime.includes("/") ? mime.split("/").pop() ?? mime : mime;
  return tail.replace(/^x-/, "");
}

/** Only the kinds that ran, so a project nobody has searched by memory
 *  does not advertise a zero. */
function searchHint(stats: {
  searches_hybrid: number;
  searches_kb: number;
  searches_memory: number;
}): string | undefined {
  const parts = [
    { label: "hybrid", n: stats.searches_hybrid },
    { label: "knowledge", n: stats.searches_kb },
    { label: "memory", n: stats.searches_memory },
  ].filter((p) => p.n > 0);
  if (parts.length === 0) return undefined;
  return parts.map((p) => `${p.n.toLocaleString()} ${p.label}`).join(" · ");
}
