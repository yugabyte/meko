// Full route: read-only paginated memories (no promote actions). Mirrors Learnings Summary layout.

import { useEffect, useState } from "react";
import { Brain } from "lucide-react";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";
import { DatapackBreadcrumbs } from "@/features/datapacks/components/datapack-detail/DatapackBreadcrumbs";
import { MemoryReadOnlySummaryPanel } from "@/features/datapacks/components/memory-promotion/MemoryReadOnlySummaryPanel";
import { WarningBanner } from "@/components/ui/warning-banner";
import { datapackDetailPath, ROUTES } from "@/app/routes/paths";
import { getApiErrorMessage, getDatapack, type Datapack } from "@/lib/mekoApi";

type Props = {
  datapackName: string;
};

export default function DatapackMemorySummaryPage({ datapackName }: Props) {
  const [datapack, setDatapack] = useState<Datapack | null>(null);
  const [statusLoading, setStatusLoading] = useState(true);
  const [statusError, setStatusError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    const run = async () => {
      setStatusLoading(true);
      setStatusError(null);
      try {
        const d = await getDatapack(datapackName);
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
  }, [datapackName]);

  return (
    <div className="flex h-screen flex-1 flex-col overflow-y-auto bg-background text-foreground">
      <AppWorkspaceHeader />

      <main className="mx-auto flex w-full max-w-7xl flex-1 flex-col space-y-8 px-1 pb-3 pr-1 pt-6">
        <DatapackBreadcrumbs
          items={[
            { label: "Datapacks", href: ROUTES.datapacks },
            { label: datapackName, href: datapackDetailPath(datapackName) },
            { label: "Memory Summary" },
          ]}
        />

        <div className="space-y-2">
          <div className="flex flex-wrap items-center gap-3">
            <Brain className="h-8 w-8 shrink-0 text-blue-400" aria-hidden />
            <h1 className="truncate text-2xl font-semibold tracking-tight text-foreground sm:text-3xl">
              Memory Summary
            </h1>
          </div>
          <p className="max-w-2xl text-sm leading-relaxed text-muted-foreground">
            All session memories for <span className="text-foreground/90">{datapackName}</span> (read-only).
          </p>
        </div>

        {statusError ? <WarningBanner>{statusError}</WarningBanner> : null}

        {datapack?.datapack_id ? (
          <MemoryReadOnlySummaryPanel
            datapackName={datapackName}
            datapackId={datapack.datapack_id}
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
