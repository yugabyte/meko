// Full route: read-only paginated promoted memories. Mirrors DatapackMemorySummaryPage layout.

import { useEffect, useState } from "react";
import { Sparkles } from "lucide-react";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";
import { DatapackBreadcrumbs } from "@/features/datapacks/components/datapack-detail/DatapackBreadcrumbs";
import { CollectiveMemoryPanel } from "@/features/datapacks/components/collective-memory/CollectiveMemoryPanel";
import { WarningBanner } from "@/components/ui/warning-banner";
import { datapackDetailPath, ROUTES } from "@/app/routes/paths";
import {
  getApiErrorMessage,
  getDatapack,
  getKnowledgeBaseSummary,
  type Datapack,
  type KnowledgeBaseSummary,
} from "@/lib/api/mekoApi";

type Props = {
  datapackName: string;
  /** Optional URL-pinned id from `?datapack_id=`. See sibling pages
   *  for the rationale (name-collision disambiguation across share
   *  boundaries). Falling back to name-only resolution when omitted. */
  datapackId?: string;
};

export default function DatapackCollectiveMemorySummaryPage({ datapackName, datapackId }: Props) {
  const [datapack, setDatapack] = useState<Datapack | null>(null);
  const [statusLoading, setStatusLoading] = useState(true);
  const [statusError, setStatusError] = useState<string | null>(null);
  // /summary feeds the per-card index-level chips (embedding model /
  // dimensions / ai provider) in the panel footer. We don't show its
  // description / files-summary on this standalone page, but the
  // chips need the data so we fetch it lazily alongside the datapack.
  const [summary, setSummary] = useState<KnowledgeBaseSummary | null>(null);

  useEffect(() => {
    let cancelled = false;
    const run = async () => {
      setStatusLoading(true);
      setStatusError(null);
      try {
        const d = await getDatapack(datapackName, { datapackId });
        if (!cancelled) setDatapack(d);
      } catch (e) {
        if (!cancelled) {
          setDatapack(null);
          setStatusError(getApiErrorMessage(e, "Failed to load datapack."));
        }
      } finally {
        if (!cancelled) setStatusLoading(false);
      }
    };
    void run();
    return () => {
      cancelled = true;
    };
  }, [datapackName, datapackId]);

  useEffect(() => {
    const ac = new AbortController();
    void getKnowledgeBaseSummary(datapackName, { signal: ac.signal })
      .then((s) => {
        if (!ac.signal.aborted) setSummary(s);
      })
      .catch(() => {
        // Silent — the panel degrades to no chips if /summary 404s.
      });
    return () => ac.abort();
  }, [datapackName]);

  return (
    <div className="flex h-screen flex-1 flex-col overflow-y-auto bg-background text-foreground">
      <AppWorkspaceHeader />

      <main className="mx-auto flex w-full max-w-7xl flex-1 flex-col space-y-8 px-1 pb-3 pr-1 pt-6">
        <DatapackBreadcrumbs
          items={[
            { label: "Datapacks", href: ROUTES.datapacks },
            { label: datapackName, href: datapackDetailPath(datapackName, datapackId) },
            { label: "Collective Memory Summary" },
          ]}
        />

        <div className="space-y-2">
          <div className="flex flex-wrap items-center gap-3">
            <Sparkles className="h-8 w-8 shrink-0 text-sky-700 dark:text-sky-300" aria-hidden />
            <h1 className="truncate text-2xl font-semibold tracking-tight text-foreground sm:text-3xl">
              Collective Memory Summary
            </h1>
          </div>
          <p className="max-w-2xl text-sm leading-relaxed text-muted-foreground">
            All memories promoted into the tenant knowledge base for{" "}
            <span className="text-foreground/90">{datapackName}</span> (read-only).
          </p>
        </div>

        {statusError ? <WarningBanner>{statusError}</WarningBanner> : null}

        {datapack?.datapack_id ? (
          <CollectiveMemoryPanel
            datapackName={datapackName}
            datapackId={datapack.datapack_id}
            pendingCount={datapack.learnings_count ?? 0}
            embeddingModel={summary?.embedding_model ?? null}
            embeddingDimensions={summary?.embedding_dimensions ?? null}
            aiProvider={summary?.ai_provider ?? null}
          />
        ) : (
          <div className="rounded-xl border border-dashed border-border bg-muted/5 px-6 py-16 text-center text-sm text-muted-foreground">
            {statusLoading ? "Loading datapack…" : "Datapack not available."}
          </div>
        )}
      </main>
    </div>
  );
}
