// Left-nav Knowledge panel for the Playground: lists indexed sources for the
// selected datapack and exposes "Add your own" (file upload) and "Add sample"
// (seeded with europe-travel-guide.md). Both flows reuse DatapackKnowledgeUploadDialog.

import { useCallback, useEffect, useState } from "react";
import { BookOpen, FileText, Sparkles, Upload } from "lucide-react";
import {
  getApiErrorMessage,
  getKnowledgeBaseDocuments,
  type KnowledgeBaseDocument,
} from "@/lib/mekoApi";
import { DatapackKnowledgeUploadDialog } from "@/features/datapacks/components/datapack-detail/dialogs";
import {
  buildSampleKnowledgeFile,
  SAMPLE_KNOWLEDGE_FILENAME,
} from "@/features/playground/data/sampleKnowledge";

type Props = {
  datapackName: string | null;
  /** Fired whenever the docs list changes (initial load, after upload, or
   *  after a status-change poll). PlaygroundPage uses this to gate the chat
   *  input until at least one doc is `COMPLETED`. */
  onDocumentsChange?: (docs: KnowledgeBaseDocument[]) => void;
};

/** Polling interval while at least one doc is still being processed. */
const KB_POLL_INTERVAL_MS = 5_000;

export function PlaygroundKnowledgePanel({ datapackName, onDocumentsChange }: Props) {
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

  // While any doc is still pre-COMPLETED (PROCESSING / QUEUED / DISCOVERING /
  // FAILED), refetch every 5s so the UI flips from "processing" → "ready"
  // without forcing the user to click anything. Stops polling once every doc
  // is in a terminal state (`COMPLETED`).
  useEffect(() => {
    if (!datapackName) return;
    const stillWorking = docs.length > 0 && docs.some((d) => d.status !== "COMPLETED");
    if (!stillWorking) return;
    const id = setInterval(() => {
      void fetchDocs();
    }, KB_POLL_INTERVAL_MS);
    return () => clearInterval(id);
  }, [datapackName, docs, fetchDocs]);

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

      <div className="flex min-h-0 flex-1 flex-col gap-5 overflow-y-auto px-4 py-4">
        <section className="flex min-h-0 flex-col">
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

        <section>
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

        <section>
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
            <button
              type="button"
              onClick={openSampleUpload}
              disabled={!datapackName}
              className="inline-flex items-center gap-1.5 rounded-md border border-emerald-500/30 bg-emerald-500/10 px-2.5 py-1.5 text-xs font-medium text-emerald-200 transition-colors hover:bg-emerald-500/20 disabled:cursor-not-allowed disabled:opacity-50"
            >
              <Sparkles className="h-3.5 w-3.5" aria-hidden />
              Add the sample
            </button>
          </div>
        </section>
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
