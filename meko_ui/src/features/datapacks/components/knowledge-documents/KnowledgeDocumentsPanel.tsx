/**
 * Knowledge tab: lists ingested documents from GET …/knowledge-bases/documents.
 * Data is loaded by the parent so the same payload can feed the sidebar tile without a second request.
 */
import { BookOpen, Loader2, AlertCircle, RefreshCw } from "lucide-react";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import type { KnowledgeBaseDocument } from "@/lib/mekoApi";
import { documentNameLastSegment } from "./knowledgeDocumentName";
import { kbDocumentStatusLabel, kbDocumentStatusTone } from "./knowledgeDocumentStatus";

type Props = {
  /** From datapack describe (`knowledge_count`); shown in header when summary is ready. */
  knowledgeBaseCount: number;
  /** When false, header shows a placeholder (same pattern as Learnings summary). */
  datapackSummaryReady: boolean;
  documents: KnowledgeBaseDocument[];
  documentsLoading: boolean;
  documentsError: string | null;
  onReloadDocuments: () => void;
};

const toneClasses = {
  muted: "border-border/60 bg-muted/30 text-muted-foreground",
  blue: "border-blue-500/35 bg-blue-500/10 text-blue-700 dark:text-blue-200",
  green: "border-emerald-500/35 bg-emerald-500/10 text-emerald-700 dark:text-emerald-200",
  red: "border-red-500/35 bg-red-500/10 text-red-700 dark:text-red-200",
} as const;

export function KnowledgeDocumentsPanel({
  knowledgeBaseCount,
  datapackSummaryReady,
  documents,
  documentsLoading,
  documentsError,
  onReloadDocuments,
}: Props) {
  const loading = documentsLoading;

  return (
    <div className="rounded-xl border border-border bg-card p-6 space-y-4">
      <div className="flex items-center justify-between gap-4">
        <div className="flex min-w-0 items-center gap-4">
          <div className="flex h-10 w-10 shrink-0 items-center justify-center rounded-lg bg-emerald-500/15 ring-1 ring-emerald-500/10">
            <BookOpen className="h-5 w-5 text-emerald-700 dark:text-emerald-300" strokeWidth={1.75} aria-hidden />
          </div>
          <div className="min-w-0">
            <h3 className="text-base font-semibold tracking-tight text-foreground">
              <span>Knowledge documents</span>
              {datapackSummaryReady ? (
                <span>
                  {" "}
                  · {knowledgeBaseCount}{" "}
                  {knowledgeBaseCount === 1 ? "knowledge base" : "knowledge bases"}
                </span>
              ) : (
                <span className="font-normal text-muted-foreground"> · …</span>
              )}
            </h3>
          </div>
        </div>
        <Button
          variant="outline"
          className="gap-2 border-border bg-background text-muted-foreground hover:bg-muted hover:text-foreground"
          onClick={() => void onReloadDocuments()}
          disabled={loading}
        >
          {loading ? (
            <Loader2 className="h-4 w-4 animate-spin" aria-hidden />
          ) : (
            <RefreshCw className="h-4 w-4" aria-hidden />
          )}
          {loading ? "Loading..." : "Refresh"}
        </Button>
      </div>

      {documentsError ? (
        <div
          className="flex items-center gap-2 rounded-lg border border-red-500/30 bg-red-500/10 px-4 py-2 text-xs text-red-700 dark:text-red-300"
          role="alert"
        >
          <AlertCircle className="h-4 w-4 shrink-0" aria-hidden />
          {documentsError}
        </div>
      ) : null}

      {loading && documents.length === 0 && !documentsError ? (
        <div className="flex flex-col items-center justify-center gap-3 py-14 text-muted-foreground">
          <Loader2 className="h-8 w-8 animate-spin text-emerald-400/80" aria-hidden />
          <p className="text-sm">Loading documents…</p>
        </div>
      ) : documents.length === 0 && !documentsError ? (
        <div className="rounded-lg border border-dashed border-border/60 bg-muted/5 px-4 py-12 text-center text-sm text-muted-foreground">
          No knowledge documents yet. Upload files from this datapack&apos;s actions menu to see them
          here.
        </div>
      ) : documents.length > 0 ? (
        <ul className="max-h-[min(28rem,55vh)] space-y-2 overflow-y-auto pr-1">
          {documents.map((doc, idx) => {
            const tone = kbDocumentStatusTone(doc.status);
            const fullName = doc.document_name?.trim() || "—";
            const displayName = documentNameLastSegment(fullName);
            return (
              <li
                key={`${idx}-${doc.document_name}`}
                className="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-border/50 bg-input/80 px-3 py-2.5"
              >
                <span
                  className="min-w-0 flex-1 truncate font-mono text-xs text-foreground/90"
                  title={fullName}
                >
                  {displayName}
                </span>
                <span
                  className={cn(
                    "shrink-0 rounded-full border px-2.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide",
                    toneClasses[tone],
                  )}
                >
                  {kbDocumentStatusLabel(doc.status)}
                </span>
              </li>
            );
          })}
        </ul>
      ) : null}
    </div>
  );
}
