// Full route: read-only paginated promoted memories. Mirrors DatapackMemorySummaryPage layout.

import { useEffect, useState } from "react";
import { Sparkles } from "lucide-react";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";
import { DatapackBreadcrumbs } from "@/features/datapacks/components/datapack-detail/DatapackBreadcrumbs";
import { CollectiveMemoryPanel } from "@/features/datapacks/components/collective-memory/CollectiveMemoryPanel";
import { datapackDetailPath, ROUTES } from "@/app/routes/paths";
import { getApiErrorMessage, getDatapack, type Datapack } from "@/lib/mekoApi";

type Props = {
  datapackName: string;
};

export default function DatapackCollectiveMemorySummaryPage({ datapackName }: Props) {
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
            { label: "Collective Memory Summary" },
          ]}
        />

        <div className="space-y-2">
          <div className="flex flex-wrap items-center gap-3">
            <Sparkles className="h-8 w-8 shrink-0 text-sky-300" aria-hidden />
            <h1 className="truncate text-2xl font-semibold tracking-tight text-foreground sm:text-3xl">
              Collective Memory Summary
            </h1>
          </div>
          <p className="max-w-2xl text-sm leading-relaxed text-muted-foreground">
            All memories promoted into the tenant knowledge base for{" "}
            <span className="text-foreground/90">{datapackName}</span> (read-only).
          </p>
        </div>

        {statusError ? (
          <div className="rounded-md border border-amber-500/40 bg-amber-500/10 px-4 py-3 text-sm text-amber-100">
            {statusError}
          </div>
        ) : null}

        {datapack?.datapack_id ? (
          <CollectiveMemoryPanel
            datapackName={datapackName}
            datapackId={datapack.datapack_id}
            variant="full"
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
