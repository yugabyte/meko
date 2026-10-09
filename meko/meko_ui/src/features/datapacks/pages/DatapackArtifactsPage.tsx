// DatapackArtifactsPage — the datapack's Artifacts detail surface.
// Motivated by the Knowledge Base summary page: breadcrumb → tinted
// header hero → 4 count tiles → files-style table. Brand-orange
// accents throughout so the page matches the DatapackDetailArtifactsTile.
// Any datapack grant can read this (owner / maintainer / contributor / viewer).
//
// The API is thin — a paginated list endpoint + a presigned-S3
// download endpoint (plus a base64 fallback for db-inline artifacts).
// This page is essentially a read-only report; artifact upload lives
// with the tool that produced it (usually the MCP server), not here.

import { useEffect, useMemo, useState } from "react";
import { Database, FileText, HardDrive, Loader2, Package, Search } from "lucide-react";

import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";
import { DatapackBreadcrumbs } from "@/features/datapacks/components/datapack-detail/DatapackBreadcrumbs";
import { ArtifactRow } from "@/features/datapacks/components/artifacts/ArtifactRow";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { StatTile } from "@/components/ui/stat-tile";
import { formatRelativeTime, useNowMs } from "@/lib/relativeTime";
import { datapackDetailPath, ROUTES } from "@/app/routes/paths";
import {
  getApiErrorMessage,
  getDatapack,
  listDatapackArtifacts,
  type Artifact,
  type Datapack,
} from "@/lib/api/mekoApi";

const PAGE_SIZE = 25;

type Props = {
  datapackId: string;
};

// `datapackId` is passed as a prop by `DatapacksPage` (which parses
// the URL itself and dispatches to sub-pages without a nested
// react-router <Route> tree). Keeps this component consistent with
// DatapackClustersPage / DatapackKnowledgeSummaryPage etc. — a
// useParams() read here returns {} because no parent route names
// the segment.
export default function DatapackArtifactsPage({ datapackId }: Props) {
  // Datapack metadata (name) — same lightweight fetch other detail
  // pages use so the breadcrumb has a display name.
  const [datapack, setDatapack] = useState<Datapack | null>(null);
  const [datapackError, setDatapackError] = useState<string | null>(null);
  useEffect(() => {
    if (!datapackId) return;
    const ac = new AbortController();
    (async () => {
      try {
        setDatapack(await getDatapack(datapackId, { signal: ac.signal }));
      } catch (err) {
        if (ac.signal.aborted) return;
        setDatapackError(getApiErrorMessage(err, "Couldn't load datapack."));
      }
    })();
    return () => ac.abort();
  }, [datapackId]);

  // Search input is debounced into `q` before hitting the API — a
  // filename ILIKE on every keystroke is wasteful and jitters the
  // table.
  const [searchInput, setSearchInput] = useState("");
  const [q, setQ] = useState("");
  useEffect(() => {
    const t = setTimeout(() => setQ(searchInput.trim()), 250);
    return () => clearTimeout(t);
  }, [searchInput]);

  const [page, setPage] = useState(1);
  useEffect(() => setPage(1), [q]); // reset paging on new filter

  const [items, setItems] = useState<Artifact[]>([]);
  const [total, setTotal] = useState(0);
  const [hasMore, setHasMore] = useState(false);
  const [loading, setLoading] = useState(false);
  const [listError, setListError] = useState<string | null>(null);

  useEffect(() => {
    if (!datapackId) return;
    const ac = new AbortController();
    setLoading(true);
    setListError(null);
    (async () => {
      try {
        const resp = await listDatapackArtifacts(
          datapackId,
          { page, page_size: PAGE_SIZE, q: q || undefined },
          { signal: ac.signal },
        );
        setItems(resp.items);
        setTotal(resp.total);
        setHasMore(resp.has_more);
      } catch (err) {
        if (ac.signal.aborted) return;
        setListError(getApiErrorMessage(err, "Couldn't load artifacts."));
      } finally {
        if (!ac.signal.aborted) setLoading(false);
      }
    })();
    return () => ac.abort();
  }, [datapackId, page, q]);

  const nowMs = useNowMs();
  const displayName = datapack?.datapack_name ?? "";

  // Header stats. `total` is server-authoritative; the storage split /
  // last-upload aggregates rely on the current page (the API returns
  // the newest rows first, so page 1 usually carries the "latest").
  // If a summary endpoint gets added later, wire it here.
  const stats = useMemo(() => {
    const dbCount = items.filter((a) => a.stored_in === "db").length;
    const s3Count = items.filter((a) => a.stored_in === "s3").length;
    const pageSize = items.reduce((sum, a) => sum + (a.size_bytes ?? 0), 0);
    const latest = items[0]?.created_at ?? null;
    return { dbCount, s3Count, pageSize, latest };
  }, [items]);

  return (
    <div className="flex h-screen flex-1 flex-col overflow-y-auto bg-background text-foreground">
      <AppWorkspaceHeader />
      <main className="mx-auto flex w-full max-w-7xl flex-1 flex-col space-y-8 px-1 pb-12 pr-1 pt-6">
        {/* Skip the datapack-name crumb until the datapack fetch
            resolves — an empty label would render as a stray arrow
            between "Datapacks" and "Artifacts" for the ~1 tick before
            the name lands. */}
        <DatapackBreadcrumbs
          items={[
            { label: "Datapacks", href: ROUTES.datapacks },
            ...(displayName ? [{ label: displayName, href: datapackDetailPath(datapackId) }] : []),
            { label: "Artifacts" },
          ]}
        />

        {datapackError ? (
          <div className="rounded-md border border-amber-500/40 bg-amber-500/10 px-4 py-3 text-sm text-amber-700 dark:text-amber-100">
            {datapackError}
          </div>
        ) : null}

        {/* Header mirrors DatapackCollectiveMemorySummaryPage: bare
            icon + big title on one row, description on the next row.
            No tinted card so the two "detail library" pages read as
            part of the same family instead of dueling coloured boxes. */}
        <div className="space-y-2">
          <div className="flex flex-wrap items-center gap-3">
            <Package className="h-8 w-8 shrink-0 text-brand" aria-hidden />
            <h1 className="truncate text-2xl font-semibold tracking-tight text-foreground sm:text-3xl">
              Artifacts
            </h1>
          </div>
          <p className="max-w-2xl text-sm leading-relaxed text-muted-foreground">
            Files agents produced or attached in this datapack. Available to every member of{" "}
            <span className="text-foreground/90">{displayName || "this datapack"}</span>.
          </p>
        </div>

        {/* Count tiles — same 4-tile deck as KB, adapted to what the
            list endpoint returns. Total is authoritative; the others
            are page-local aggregates. */}
        <div className="grid gap-3 md:grid-cols-4">
          <StatTile
            icon={<FileText className="h-4 w-4 text-brand" />}
            label="TOTAL"
            value={total}
          />
          <StatTile
            icon={<Database className="h-4 w-4 text-brand" />}
            label="INLINE (DB)"
            value={stats.dbCount}
          />
          <StatTile
            icon={<HardDrive className="h-4 w-4 text-brand" />}
            label="LARGE (S3)"
            value={stats.s3Count}
          />
          <StatTile
            icon={<Loader2 className="h-4 w-4 text-brand" />}
            label="LATEST UPLOAD"
            value={stats.latest ? formatRelativeTime(stats.latest, nowMs) : "—"}
          />
        </div>

        {/* Search + files table */}
        <div className="rounded-xl border border-border bg-card">
          <div className="border-b border-border px-4 py-3">
            <div className="relative">
              <Search
                className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground"
                aria-hidden
              />
              <Input
                value={searchInput}
                onChange={(e) => setSearchInput(e.target.value)}
                placeholder="Search artifacts by filename…"
                className="border border-border bg-input pl-9"
                aria-label="Search artifacts"
              />
            </div>
          </div>

          {listError ? (
            <p className="px-4 py-6 text-sm text-red-600 dark:text-red-400">{listError}</p>
          ) : loading && items.length === 0 ? (
            <p className="px-4 py-6 text-sm text-muted-foreground">Loading artifacts…</p>
          ) : items.length === 0 ? (
            <p className="px-4 py-6 text-sm text-muted-foreground">
              {q
                ? `No artifacts match "${q}".`
                : "No artifacts yet — agents haven't stored anything in this datapack."}
            </p>
          ) : (
            <>
              {/* Column header row — same widths + label style as the
                  KB documents table so both surfaces read consistently.
                  Hidden below md so the row layout collapses cleanly
                  on mobile. Left-aligned to match KB; the row's value
                  cells mirror this alignment (see ArtifactRow). */}
              <div className="hidden items-center gap-4 border-b border-border px-4 py-2 text-[10px] font-semibold uppercase tracking-wider text-muted-foreground md:flex">
                {/* +28px indent matches the file-icon column (h-4 icon
                    + gap-3) in the row so FILE sits over the filename. */}
                <span className="flex-1 pl-7">File</span>
                <span className="w-20">Size</span>
                <span className="w-28">Uploaded</span>
                {/* Spacer matching the fixed Download-button width so
                    the header stops where the button starts. */}
                <span aria-hidden className="w-[7.5rem]" />
              </div>
              <ul className="divide-y divide-border">
                {items.map((a) => (
                  <ArtifactRow
                    key={a.artifact_id}
                    artifact={a}
                    datapackId={datapackId}
                    nowMs={nowMs}
                  />
                ))}
              </ul>
            </>
          )}

          {/* Pagination — mirrors KB documents (page/back buttons). */}
          {(page > 1 || hasMore) && !listError ? (
            <div className="flex items-center justify-between gap-2 border-t border-border px-4 py-3 text-xs text-muted-foreground">
              <span>
                Page {page} · {total} total
              </span>
              <div className="flex items-center gap-2">
                <Button
                  variant="outline"
                  size="sm"
                  disabled={page <= 1 || loading}
                  onClick={() => setPage((p) => Math.max(1, p - 1))}
                >
                  Previous
                </Button>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={!hasMore || loading}
                  onClick={() => setPage((p) => p + 1)}
                >
                  Next
                </Button>
              </div>
            </div>
          ) : null}
        </div>
      </main>
    </div>
  );
}
