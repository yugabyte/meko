// Full route: paginated knowledge documents for a datapack. Mirrors the
// other detail summary pages (Memory / Collective Memory / Learnings).

import { useCallback, useEffect, useState } from "react";
import { BookOpen } from "lucide-react";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";
import { DatapackBreadcrumbs } from "@/features/datapacks/components/datapack-detail/DatapackBreadcrumbs";
import { KnowledgeDocumentsPanel } from "@/features/datapacks/components/knowledge-documents/KnowledgeDocumentsPanel";
import { WarningBanner } from "@/components/ui/warning-banner";
import { datapackDetailPath, ROUTES } from "@/app/routes/paths";
import {
  getApiErrorMessage,
  getDatapack,
  getKnowledgeBaseDocuments,
  type Datapack,
  type KnowledgeBaseDocument,
} from "@/lib/mekoApi";

type Props = {
  datapackName: string;
  datapackId?: string;
};

export default function DatapackKnowledgeSummaryPage({ datapackName, datapackId }: Props) {
  const [datapack, setDatapack] = useState<Datapack | null>(null);
  const [statusLoading, setStatusLoading] = useState(true);
  const [statusError, setStatusError] = useState<string | null>(null);

  const [documents, setDocuments] = useState<KnowledgeBaseDocument[]>([]);
  const [documentsLoading, setDocumentsLoading] = useState(true);
  const [documentsError, setDocumentsError] = useState<string | null>(null);
  const [documentsNonce, setDocumentsNonce] = useState(0);

  const reloadDocuments = useCallback(() => setDocumentsNonce((n) => n + 1), []);

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
    setDocumentsLoading(true);
    setDocumentsError(null);
    void getKnowledgeBaseDocuments(datapackName, { signal: ac.signal, datapackId })
      .then((list) => {
        if (ac.signal.aborted) return;
        setDocuments(list ?? []);
      })
      .catch((e) => {
        if (ac.signal.aborted) return;
        setDocuments([]);
        setDocumentsError(getApiErrorMessage(e, "Could not load knowledge documents."));
      })
      .finally(() => {
        if (!ac.signal.aborted) setDocumentsLoading(false);
      });
    return () => ac.abort();
  }, [datapackName, datapackId, documentsNonce]);

  return (
    <div className="flex h-screen flex-1 flex-col overflow-y-auto bg-background text-foreground">
      <AppWorkspaceHeader />

      <main className="mx-auto flex w-full max-w-7xl flex-1 flex-col space-y-8 px-1 pb-3 pr-1 pt-6">
        <DatapackBreadcrumbs
          items={[
            { label: "Datapacks", href: ROUTES.datapacks },
            { label: datapackName, href: datapackDetailPath(datapackName, datapackId) },
            { label: "Knowledge" },
          ]}
        />

        <div className="space-y-2">
          <div className="flex flex-wrap items-center gap-3">
            <BookOpen className="h-8 w-8 shrink-0 text-emerald-700 dark:text-emerald-300" aria-hidden />
            <h1 className="truncate text-2xl font-semibold tracking-tight text-foreground sm:text-3xl">
              Knowledge
            </h1>
          </div>
          <p className="max-w-2xl text-sm leading-relaxed text-muted-foreground">
            All knowledge documents ingested for{" "}
            <span className="text-foreground/90">{datapackName}</span>.
          </p>
        </div>

        {statusError ? <WarningBanner>{statusError}</WarningBanner> : null}

        {datapack?.datapack_id ? (
          <KnowledgeDocumentsPanel
            knowledgeBaseCount={documents.length}
            datapackSummaryReady={!statusLoading && datapack != null}
            documents={documents}
            documentsLoading={documentsLoading}
            documentsError={documentsError}
            onReloadDocuments={reloadDocuments}
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
