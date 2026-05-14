// Left-nav Knowledge panel for the WorkbenchLM: lists indexed sources for the
// selected datapack and exposes "Add your own" (file upload) and "Add sample"
// (seeded with europe-travel-guide.md). Both flows reuse DatapackKnowledgeUploadDialog.

import { useCallback, useEffect, useState } from "react";
import { BookOpen, ExternalLink, FileText, Sparkles, Upload } from "lucide-react";
import {
  getApiErrorMessage,
  getKnowledgeBaseDocuments,
  type KnowledgeBaseDocument,
} from "@/lib/mekoApi";
import { toast } from "@/hooks/use-toast";
import { DatapackKnowledgeUploadDialog } from "@/features/datapacks/components/datapack-detail/dialogs";
import {
  buildSampleKnowledgeFile,
  openSampleKnowledgePreview,
  SAMPLE_KNOWLEDGE_FILENAME,
} from "@/features/workbench-lm/data/sampleKnowledge";

type Props = {
  datapackName: string | null;
  /** Fired whenever the docs list changes (initial load, after upload, or
   *  after a status-change poll). WorkbenchLMPage uses this to gate the chat
   *  input until at least one doc is `COMPLETED`. */
  onDocumentsChange?: (docs: KnowledgeBaseDocument[]) => void;
  /**
   * True when WorkbenchLM is not the active route — the page tree stays
   * mounted (ProtectedRoutes preserves state via `display: none`) so
   * polling has to be paused explicitly. Both the processing-state poll
   * (5s) and the steady-state refresh (30s) skip while this is true.
   */
  paused?: boolean;
};

/** Polling interval while at least one doc is still being processed. */
const KB_POLL_INTERVAL_MS = 5_000;

/** Steady-state refresh interval — runs even when all docs are COMPLETED so
 *  files uploaded from another client / the CLI / the datapack detail page
 *  show up here without a hard refresh. Paused while the tab is hidden. */
const KB_STEADY_REFRESH_MS = 30_000;

export function WorkbenchLMKnowledgePanel({ datapackName, onDocumentsChange, paused = false }: Props) {
  const [docs, setDocs] = useState<KnowledgeBaseDocument[]>([]);
  const [docsLoading, setDocsLoading] = useState(false);
  const [docsError, setDocsError] = useState<string | null>(null);
  const [uploadOpen, setUploadOpen] = useState(false);
  const [sampleSeed, setSampleSeed] = useState<File[] | undefined>(undefined);

  // Bubble docs up to the parent on every change so the chat input can react
  // (enabled when at least one doc reaches COMPLETED).
  useEffect(() => {
    onDocumentsChange?.(docs);
  }, [docs, onDocumentsChange]);

  const fetchDocs = useCallback(
    async (signal?: AbortSignal) => {
      if (!datapackName) {
        setDocs([]);
        return;
      }
      setDocsLoading(true);
      setDocsError(null);
      try {
        const list = await getKnowledgeBaseDocuments(datapackName, { signal });
        setDocs(list ?? []);
      } catch (err) {
        if ((err as { name?: string })?.name === "CanceledError") return;
        setDocsError(getApiErrorMessage(err, "Failed to load knowledge sources."));
        setDocs([]);
      } finally {
        setDocsLoading(false);
      }
    },
    [datapackName],
  );

  useEffect(() => {
    const ac = new AbortController();
    void fetchDocs(ac.signal);
    return () => ac.abort();
  }, [fetchDocs]);

  // While any doc is still pre-COMPLETED (QUEUED / PROCESSING / FAILED),
  // refetch every 5s so the UI flips from "processing" → "ready"
  // without forcing the user to click anything. Stops polling once every doc
  // is in a terminal state (`COMPLETED`) — or when the WorkbenchLM route is
  // not active (parent passes `paused`).
  useEffect(() => {
    if (!datapackName || paused) return;
    const stillWorking = docs.length > 0 && docs.some((d) => d.status !== "COMPLETED");
    if (!stillWorking) return;
    const id = setInterval(() => {
      void fetchDocs();
    }, KB_POLL_INTERVAL_MS);
    return () => clearInterval(id);
  }, [datapackName, docs, fetchDocs, paused]);

  // Steady-state refresh — every 30s, surface uploads that landed via
  // another client (CLI, datapack detail page, another browser tab) so
  // the panel mirrors the server without a manual refresh. Paused while
  // the tab is hidden OR the WorkbenchLM route is not active (parent
  // passes `paused` — the page tree stays mounted under ProtectedRoutes).
  useEffect(() => {
    if (!datapackName || paused) return;
    let id: ReturnType<typeof setInterval> | null = null;
    const start = () => {
      if (id !== null) return;
      id = setInterval(() => {
        void fetchDocs();
      }, KB_STEADY_REFRESH_MS);
    };
    const stop = () => {
      if (id !== null) {
        clearInterval(id);
        id = null;
      }
    };
    const onVisibilityChange = () => {
      if (document.visibilityState === "visible") start();
      else stop();
    };
    if (document.visibilityState === "visible") start();
    document.addEventListener("visibilitychange", onVisibilityChange);
    return () => {
      document.removeEventListener("visibilitychange", onVisibilityChange);
      stop();
    };
  }, [datapackName, fetchDocs, paused]);

  // Refresh after the upload dialog closes so newly added files appear in the list.
  const handleUploadOpenChange = (open: boolean) => {
    setUploadOpen(open);
    if (!open) {
      setSampleSeed(undefined);
      void fetchDocs();
    }
  };

  const openOwnUpload = () => {
    setSampleSeed(undefined);
    setUploadOpen(true);
  };

  const openSampleUpload = () => {
    setSampleSeed([buildSampleKnowledgeFile()]);
    setUploadOpen(true);
  };

  const handleViewSample = () => {
    openSampleKnowledgePreview();
  };

  const sourceCount = docs.length;

  return (
    <aside className="flex min-h-0 w-full flex-col border-r border-border bg-background">
      <div className="flex items-center justify-between border-b border-border/70 px-4 py-3">
        <div className="flex items-center gap-2">
          <BookOpen className="h-4 w-4 text-emerald-400" aria-hidden />
          <h2 className="text-sm font-medium text-white">Knowledge</h2>
        </div>
        {datapackName ? (
          <span className="rounded-full border border-emerald-500/30 bg-emerald-500/10 px-2 py-0.5 text-[11px] font-medium text-emerald-300">
            {sourceCount} {sourceCount === 1 ? "source" : "sources"}
          </span>
        ) : null}
      </div>

      {/*
        Split body so "Loaded sources" + "Add your own" stay in the scrollable
        top region and "Add sample" sits in a shrink-0 footer pinned to the
        bottom of the panel. `mt-auto` inside overflow-y-auto does not consume
        leftover height — the scroll box only grows to content height.
      */}
      <div className="flex min-h-0 flex-1 flex-col">
        <div className="flex min-h-0 flex-1 flex-col gap-5 overflow-y-auto px-4 py-4">
          <section className="flex min-h-0 shrink-0 flex-col">
            <h3 className="mb-2 text-[11px] font-semibold uppercase tracking-wider text-muted-foreground">
              Loaded sources
            </h3>
            {!datapackName ? (
              <p className="text-xs text-muted-foreground">
                Select a datapack to see its knowledge sources.
              </p>
            ) : docsLoading && docs.length === 0 ? (
              <p className="text-xs text-muted-foreground">Loading sources…</p>
            ) : docsError ? (
              <p className="text-xs text-red-400">{docsError}</p>
            ) : docs.length === 0 ? (
              <p className="text-xs text-muted-foreground">
                No sources yet. Upload your own or add the sample below.
              </p>
            ) : (
              <ul className="max-h-72 space-y-1.5 overflow-y-auto pr-1">
                {docs.map((d, i) => {
                  const filename = d.document_name.split("/").pop() ?? d.document_name;
                  const display = filename.replace(/^\d+_/, "");
                  return (
                    <li
                      key={`${d.document_name}-${i}`}
                      className="flex items-center gap-2 rounded-lg border border-border bg-[#1C2333] px-3 py-2"
                      title={d.document_name}
                    >
                      <FileText className="h-3.5 w-3.5 shrink-0 text-emerald-400" aria-hidden />
                      <span className="min-w-0 flex-1 truncate text-xs text-white">{display}</span>
                      <span
                        className="shrink-0 text-[10px] uppercase tracking-wide text-muted-foreground"
                        aria-label={`status ${d.status}`}
                      >
                        {d.status}
                      </span>
                    </li>
                  );
                })}
              </ul>
            )}
          </section>

          <section className="shrink-0">
            <h3 className="mb-2 text-[11px] font-semibold uppercase tracking-wider text-muted-foreground">
              Add your own
            </h3>
            <button
              type="button"
              onClick={openOwnUpload}
              disabled={!datapackName}
              className="group flex w-full items-center justify-center gap-2 rounded-lg border border-dashed border-border/70 bg-[#1C2333] px-3 py-3 text-sm font-medium text-white transition-colors hover:border-blue-400/70 hover:bg-blue-500/5 disabled:cursor-not-allowed disabled:opacity-50"
            >
              <Upload className="h-4 w-4 text-blue-400" aria-hidden />
              File
            </button>
          </section>
        </div>

        {/*
          "Get started" affordance — hide once the user has at least one source.
          Footer is outside the scroll region so it stays pinned to the bottom
          of the panel while Loaded sources + Add your own scroll above.
        */}
        {sourceCount === 0 ? (
          <section className="shrink-0 border-t border-border/70 px-4 pb-4 pt-3">
            <h3 className="mb-2 text-[11px] font-semibold uppercase tracking-wider text-muted-foreground">
              Add sample
            </h3>
            <div className="space-y-2 rounded-lg border border-border bg-[#1C2333] p-3">
              <div className="flex items-center gap-2">
                <FileText className="h-4 w-4 shrink-0 text-emerald-400" aria-hidden />
                <span className="min-w-0 flex-1 truncate text-sm font-medium text-white">
                  {SAMPLE_KNOWLEDGE_FILENAME}
                </span>
              </div>
              <p className="text-[11px] leading-relaxed text-muted-foreground">
                A short Europe travel guide covering Spain, UK, Greece, Italy, and Portugal.
              </p>
              <div className="flex flex-wrap items-center gap-2">
                <button
                  type="button"
                  onClick={openSampleUpload}
                  disabled={!datapackName}
                  className="inline-flex items-center gap-1.5 rounded-md border border-emerald-500/30 bg-emerald-500/10 px-2.5 py-1.5 text-xs font-medium text-emerald-200 transition-colors hover:bg-emerald-500/20 disabled:cursor-not-allowed disabled:opacity-50"
                >
                  <Sparkles className="h-3.5 w-3.5" aria-hidden />
                  Add the sample
                </button>
                {/*
                  View opens the sample's raw markdown in a new tab via a
                  blob URL — lets the user preview what they'd be adding
                  before actually uploading it.
                */}
                <button
                  type="button"
                  onClick={handleViewSample}
                  className="inline-flex items-center gap-1.5 rounded-md border border-border bg-background/60 px-2.5 py-1.5 text-xs font-medium text-foreground/80 transition-colors hover:border-border/80 hover:text-white"
                  title="Open the sample file in a new tab"
                >
                  <ExternalLink className="h-3.5 w-3.5" aria-hidden />
                  View
                </button>
              </div>
            </div>
          </section>
        ) : null}
      </div>

      {datapackName ? (
        <DatapackKnowledgeUploadDialog
          datapackName={datapackName}
          open={uploadOpen}
          onOpenChange={handleUploadOpenChange}
          initialFiles={sampleSeed}
        />
      ) : null}
    </aside>
  );
}
