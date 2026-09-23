// DatapackDetailArtifactsTile — sibling of the Knowledge / Members /
// Clusters tiles on the datapack detail page.
//
// Brand-orange accent (matches Members / Connect / Clusters — the
// established tile look) so the grid reads uniformly.
//
// Read-only: unlike Knowledge / Members / Clusters, there is no
// "add" affordance today — agents write artifacts via the MCP server,
// not the UI. Footer is a plain "View all →" link, same shape as
// the Members / Knowledge tile footers.

import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { FileText, Package } from "lucide-react";
import { cn } from "@/lib/utils";
import { formatFileSize } from "@/lib/fileSize";
import { formatRelativeTime, useNowMs } from "@/lib/relativeTime";
import { datapackArtifactsPath } from "@/app/routes/paths";
import {
  getApiErrorMessage,
  listDatapackArtifacts,
  type Artifact,
} from "@/lib/api/mekoApi";
import { HoverInfoBadge } from "@/components/ui/hover-info-badge";
import { ARTIFACTS_TILE_HINT } from "@/features/datapacks/components/datapack-detail/datapackTileHints";

/** Preview cap — mirrors the KnowledgeTile preview limit so the two
 *  read the same. Anything past this is reached via the "View all"
 *  footer link. */
export const ARTIFACT_PREVIEW_LIMIT = 3;

type Props = {
  datapackId: string;
  className?: string;
};

export function DatapackDetailArtifactsTile({ datapackId, className }: Props) {
  const [items, setItems] = useState<Artifact[]>([]);
  const [total, setTotal] = useState<number | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const nowMs = useNowMs();

  useEffect(() => {
    if (!datapackId) return;
    const ac = new AbortController();
    setLoading(true);
    setError(null);
    (async () => {
      try {
        const resp = await listDatapackArtifacts(
          datapackId,
          { page: 1, page_size: ARTIFACT_PREVIEW_LIMIT },
          { signal: ac.signal },
        );
        setItems(resp.items);
        setTotal(resp.total);
      } catch (err) {
        if (ac.signal.aborted) return;
        setError(getApiErrorMessage(err, "Couldn't load artifacts."));
      } finally {
        if (!ac.signal.aborted) setLoading(false);
      }
    })();
    return () => ac.abort();
  }, [datapackId]);

  const countLabel = loading && total == null ? "…" : String(total ?? items.length);

  return (
    <aside
      className={cn(
        "flex h-full min-h-0 w-full flex-col rounded-xl border border-border bg-card/50 shadow-sm",
        className,
      )}
    >
      <div className="flex items-center gap-2 border-b border-border px-4 py-3">
        <Package className="h-4 w-4 shrink-0 text-brand" aria-hidden />
        <span className="truncate text-sm font-semibold text-foreground">Artifacts</span>
        <HoverInfoBadge variant="icon" label="What are artifacts?">
          {ARTIFACTS_TILE_HINT}
        </HoverInfoBadge>
        <span className="rounded-full bg-muted px-2 py-0.5 text-xs font-medium text-muted-foreground tabular-nums">
          {countLabel}
        </span>
      </div>
      <ul className="flex flex-col divide-y divide-border px-2 py-1">
        {error ? (
          <li className="px-2 py-4 text-center text-xs leading-relaxed text-muted-foreground">
            {error}
          </li>
        ) : loading && items.length === 0 ? (
          <li className="px-2 py-4 text-center text-xs text-muted-foreground">Loading artifacts…</li>
        ) : items.length === 0 ? (
          <li className="px-2 py-4 text-center text-xs text-muted-foreground">
            No artifacts yet.
          </li>
        ) : (
          items.map((a) => (
            <li key={a.artifact_id} className="flex items-start gap-3 px-2 py-2.5">
              <FileText className="mt-0.5 h-4 w-4 shrink-0 text-brand/90" aria-hidden />
              <div className="min-w-0 flex-1">
                <p
                  className="truncate font-mono text-[11px] leading-snug text-foreground/95"
                  title={a.filename}
                >
                  {a.filename}
                </p>
                <p className="mt-0.5 truncate text-xs text-muted-foreground">
                  {formatFileSize(a.size_bytes)} · {formatRelativeTime(a.created_at, nowMs)}
                </p>
              </div>
            </li>
          ))
        )}
      </ul>
      {/* Same footer shape as Members / Knowledge tiles. */}
      <div className="mt-auto border-t border-border">
        <Link
          to={datapackArtifactsPath(datapackId)}
          className="block w-full py-3 text-center text-sm font-normal text-foreground/90 transition-colors hover:text-foreground"
        >
          View all →
        </Link>
      </div>
    </aside>
  );
}
