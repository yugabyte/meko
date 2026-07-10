import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { Database } from "lucide-react";
import { cn } from "@/lib/utils";
import { datapackDatabasesPath } from "@/app/routes/paths";
import { getApiErrorMessage } from "@/lib/api/mekoApi";
import { listDatabases, type AmpDatabase } from "@/lib/api/ampApi";

// Preview cap; the rest is reached via the "View all" footer. Matches the
// Members/Knowledge tiles so the tiles read consistently.
const PREVIEW_LIMIT = 3;

type Props = {
  datapackId: string;
  /** Bump to force a refetch (e.g. after "Create database" elsewhere). */
  reloadToken?: number;
  className?: string;
};

/**
 * Compact tile listing the datapack's AMP databases (names only). No search
 * here — the "View all" footer opens the full databases page. Gated by
 * SHOW_PG_AMP at the call site.
 */
export function DatapackDetailDatabasesTile({ datapackId, reloadToken = 0, className }: Props) {
  const [databases, setDatabases] = useState<AmpDatabase[]>([]);
  const [status, setStatus] = useState<"loading" | "ready" | "error">("loading");

  useEffect(() => {
    let cancelled = false;
    setStatus("loading");
    listDatabases(datapackId)
      .then((rows) => {
        if (cancelled) return;
        setDatabases(rows);
        setStatus("ready");
      })
      .catch(() => {
        if (!cancelled) setStatus("error");
      });
    return () => {
      cancelled = true;
    };
  }, [datapackId, reloadToken]);

  const preview = databases.slice(0, PREVIEW_LIMIT);
  const count = status === "loading" ? "…" : String(databases.length);
  const viewAllEnabled = status === "ready" && databases.length > 0;

  return (
    <aside
      className={cn(
        "flex h-full min-h-0 w-full flex-col rounded-xl border border-border bg-card/50 shadow-sm",
        className,
      )}
    >
      <div className="flex items-center justify-between gap-2 border-b border-border px-4 py-3">
        <div className="flex min-w-0 items-center gap-2">
          <Database className="h-4 w-4 shrink-0 text-orange-400" aria-hidden />
          <span className="truncate text-sm font-semibold text-foreground">Databases</span>
          <span className="rounded-full bg-muted px-2 py-0.5 text-xs font-medium text-muted-foreground tabular-nums">
            {count}
          </span>
        </div>
      </div>

      <ul className="flex flex-col divide-y divide-border px-2 py-1">
        {status === "loading" ? (
          <li className="px-2 py-4 text-center text-xs text-muted-foreground">Loading databases…</li>
        ) : status === "error" ? (
          <li className="px-2 py-4 text-center text-xs text-muted-foreground">
            Could not load databases.
          </li>
        ) : databases.length === 0 ? (
          <li className="px-2 py-4 text-center text-xs leading-relaxed text-muted-foreground">
            No databases yet. Use “Create database” above to spin one up.
          </li>
        ) : (
          preview.map((db) => (
            <li key={db.id} className="flex items-center gap-3 px-2 py-2.5">
              <div
                className="flex h-9 w-9 shrink-0 items-center justify-center rounded-full bg-orange-100 text-orange-700 ring-1 ring-orange-700/40 dark:bg-orange-950/70 dark:text-orange-300"
                aria-hidden
              >
                <Database className="h-4 w-4" />
              </div>
              <div className="min-w-0 flex-1">
                <div className="truncate text-sm font-medium text-foreground" title={db.name}>
                  {db.name}
                </div>
                <div className="mt-0.5 font-mono text-[11px] text-muted-foreground">
                  pg {db.default_branch.pg_version}
                </div>
              </div>
            </li>
          ))
        )}
      </ul>

      <div className="mt-auto border-t border-border">
        {viewAllEnabled ? (
          <Link
            to={datapackDatabasesPath(datapackId)}
            className="block w-full py-3 text-center text-sm font-normal text-foreground/90 transition-colors hover:text-foreground"
          >
            View all →
          </Link>
        ) : (
          <button
            type="button"
            disabled
            className="w-full cursor-not-allowed py-3 text-center text-sm font-normal text-muted-foreground/55 opacity-60"
          >
            View all →
          </button>
        )}
      </div>
    </aside>
  );
}
