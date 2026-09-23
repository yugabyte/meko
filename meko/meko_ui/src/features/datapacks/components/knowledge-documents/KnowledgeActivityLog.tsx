// "Activity log" panel: the SharedKnowledge page's, and the collection
// Activity tab's.
//
// Takes a fetcher rather than a datapack id, because the two surfaces differ
// only in which endpoint they page through: one datapack's feed, or every
// datapack in a collection. Everything else here (pagination, row rendering,
// the 429 tolerance) is identical and worth having once.
//
// Merged file-upload + memory-promotion feed, server-side paginated.
// The "Show more" button asks the BE for the next page (offset += 10)
// and appends; it stops appearing when the BE reports `has_more: false`.
//
// Row layout matches the design spec — leading icon (file icon for
// uploads, sparkles for promotions), body in the middle, timestamp
// as a relative-time string on the right ("1d ago"). The relative
// time auto-ticks via the shared useNowMs hook so a row that says
// "5m ago" updates to "6m ago" without a manual refresh.

import { useCallback, useEffect, useState } from "react";
import axios from "axios";
import { History, Loader2, Sparkles, Upload } from "lucide-react";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { formatRelativeTime, useNowMs } from "@/lib/relativeTime";
import {
  getApiErrorMessage,
  type KBActivityLogItem,
  type KBActivityLogResponse,
} from "@/lib/api/mekoApi";

const DEFAULT_PAGE_SIZE = 10;

type Props = {
  /** Fetches one page. Bound to a datapack or to a collection by the caller.
   *  Must be stable across renders (useCallback) or the feed refetches. */
  fetchPage: (args: { limit: number; cursor: string }) => Promise<KBActivityLogResponse>;
  /** Increment to reset and re-fetch from the first page (e.g. after a new KB is created). */
  refreshKey?: number;
  /**
   * Items per "Show more" page. Defaults to 10.
   * Pass a smaller value (e.g. 1) during local testing to exercise
   * the pagination flow without needing many log entries.
   */
  pageSize?: number;
  /** Says what the feed covers, when it covers more than one datapack. */
  description?: string;
};

export function KnowledgeActivityLog({
  fetchPage,
  refreshKey,
  pageSize = DEFAULT_PAGE_SIZE,
  description,
}: Props) {
  const [items, setItems] = useState<KBActivityLogItem[]>([]);
  const [hasMore, setHasMore] = useState(false);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // Server-issued opaque cursor for the NEXT page. Empty == feed
  // exhausted (or first page not yet loaded — see `initialized`).
  const [nextCursor, setNextCursor] = useState<string>("");
  // Distinguishes "no page loaded yet" (full-card placeholder) from
  // "page loaded but empty" (empty-state message).
  const [initialized, setInitialized] = useState(false);

  const loadPage = useCallback(
    async (cursor: string, append: boolean) => {
      setLoading(true);
      setError(null);
      try {
        const resp = await fetchPage({ limit: pageSize, cursor });
        setItems((prev) => (append ? [...prev, ...resp.items] : resp.items));
        setHasMore(resp.has_more);
        setNextCursor(resp.next_cursor);
        setInitialized(true);
      } catch (err) {
        // Self-inflicted rate limiting: keep showing the last good page
        // instead of surfacing a scary red banner for a transient 429.
        if (axios.isAxiosError(err) && err.response?.status === 429) return;
        setError(getApiErrorMessage(err, "Failed to load activity log."));
      } finally {
        setLoading(false);
      }
    },
    [fetchPage, pageSize],
  );

  useEffect(() => {
    setItems([]);
    setHasMore(false);
    setNextCursor("");
    setInitialized(false);
    void loadPage("", false);
  }, [loadPage, refreshKey]);

  return (
    <section className="rounded-xl border border-border bg-card p-6">
      <header className="mb-4 flex items-center gap-2">
        <History className="h-4 w-4 text-muted-foreground" aria-hidden />
        <h2 className="text-base font-semibold text-foreground">Activity log</h2>
      </header>

      {description ? (
        <p className="-mt-2 mb-4 text-sm text-muted-foreground">{description}</p>
      ) : null}

      {error ? (
        <div
          className="mb-3 rounded-md border border-red-500/30 bg-red-500/10 px-3 py-2 text-xs text-red-700 dark:text-red-200"
          role="alert"
        >
          {error}
        </div>
      ) : null}

      {!initialized && loading ? (
        <p className="text-sm text-muted-foreground">Loading activity…</p>
      ) : items.length === 0 ? (
        <p className="rounded-lg border border-dashed border-border bg-muted/5 px-4 py-8 text-center text-sm text-muted-foreground">
          No activity yet. Upload a file or promote a memory to see it here.
        </p>
      ) : (
        <ul className="space-y-2">
          {items.map((it, idx) => (
            // Items don't carry a globally-unique id (memory_id is only
            // present for promotions), so we key by (type, timestamp,
            // idx) — stable for any given render.
            <ActivityRow key={`${it.type}-${it.timestamp}-${idx}`} item={it} />
          ))}
        </ul>
      )}

      {hasMore ? (
        <div className="mt-4 flex justify-center">
          <Button
            type="button"
            variant="outline"
            size="sm"
            disabled={loading}
            onClick={() => void loadPage(nextCursor, true)}
            className="gap-1.5"
          >
            {loading ? (
              <Loader2 className="h-3.5 w-3.5 animate-spin" aria-hidden />
            ) : null}
            Show more
          </Button>
        </div>
      ) : null}
    </section>
  );
}

function ActivityRow({ item }: { item: KBActivityLogItem }) {
  const now = useNowMs();

  if (item.type === "file_upload") {
    // Take only the last path segment and strip leading UUID/timestamp prefixes.
    const rawName = item.document_name ?? "";
    const basename = rawName.split("/").pop() ?? rawName;
    const displayName = basename.replace(/^\d+_/, "") || "—";
    const relTime = formatRelativeTime(item.timestamp, now);

    // Datapack first: on the collection feed it is the thing that tells two
    // otherwise identical rows apart. Absent on a single-datapack feed.
    const meta: string[] = [];
    if (item.datapack_name) meta.push(item.datapack_name);
    if (item.chunks_processed != null) meta.push(`${item.chunks_processed} chunks generated`);

    return (
      <li className="flex items-start gap-3 rounded-lg border border-border bg-card px-4 py-3">
        <RowIcon tone="emerald">
          <Upload className="h-3.5 w-3.5" aria-hidden />
        </RowIcon>
        <div className="min-w-0 flex-1">
          <p className="truncate text-sm text-foreground" title={rawName || undefined}>
            Uploaded <span className="font-medium">{displayName}</span>
            {item.document_type ? (
              <span className="ml-1 text-xs text-muted-foreground">({item.document_type})</span>
            ) : null}
          </p>
          {meta.length > 0 ? (
            <p className="mt-0.5 text-[11px] text-muted-foreground">{meta.join(" · ")}</p>
          ) : null}
        </div>
        <div className="shrink-0 text-right">
          <RelTime label={relTime} title={item.timestamp} />
          {item.uploaded_by_email ? (
            <p className="mt-0.5 text-[11px] text-muted-foreground">{item.uploaded_by_email}</p>
          ) : null}
        </div>
      </li>
    );
  }

  // memory_promotion
  const relTime = formatRelativeTime(item.timestamp, now);
  const content = (item.content ?? "").trim();
  const snippet = content.length > 160 ? `${content.slice(0, 157)}…` : content || "(empty)";
  return (
    <li className="flex items-center gap-3 rounded-lg border border-border bg-card px-4 py-3">
      <RowIcon tone="sky">
        <Sparkles className="h-3.5 w-3.5" aria-hidden />
      </RowIcon>
      <div className="min-w-0 flex-1">
        <p className="line-clamp-2 text-sm text-foreground" title={content || undefined}>
          Promoted memory <span className="font-medium">{snippet}</span>
        </p>
        {item.agent_id || item.datapack_name ? (
          <p className="mt-0.5 text-[11px] text-muted-foreground">
            {item.datapack_name}
            {item.datapack_name && item.agent_id ? " · " : null}
            {item.agent_id ? (
              <>
                agent <code className="text-foreground/70">{item.agent_id}</code>
              </>
            ) : null}
          </p>
        ) : null}
      </div>
      <RelTime label={relTime} title={item.timestamp} />
    </li>
  );
}

/** Leading 32×32 icon square. Tone determines the background and glyph color —
 *  emerald for file uploads (knowledge), sky for promotions (collective
 *  memory) — matching the semantic color system. */
function RowIcon({
  tone,
  children,
}: {
  tone: "emerald" | "sky";
  children: React.ReactNode;
}) {
  const toneClass =
    tone === "emerald"
      ? "border-emerald-500/30 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300"
      : "border-sky-500/30 bg-sky-500/10 text-sky-700 dark:text-sky-300";
  return (
    <span
      className={cn(
        "flex h-8 w-8 shrink-0 items-center justify-center rounded-md border",
        toneClass,
      )}
      aria-hidden
    >
      {children}
    </span>
  );
}

/** Right-aligned relative-time label. Title attribute exposes the
 *  full ISO timestamp for users who hover. */
function RelTime({ label, title }: { label: string; title: string }) {
  return (
    <span
      className="shrink-0 whitespace-nowrap text-[11px] text-muted-foreground"
      title={title}
    >
      {label}
    </span>
  );
}
