import { FileText, Link2 } from "lucide-react";
import { cn } from "@/lib/utils";
import type { KnowledgeBaseDocument } from "@/lib/mekoApi";
import { documentNameLastSegment } from "@/features/datapacks/components/knowledge-documents/knowledgeDocumentName";
import { kbDocumentStatusLabel } from "@/features/datapacks/components/knowledge-documents/knowledgeDocumentStatus";

const PREVIEW_LIMIT = 3;

type Props = {
  /** From parent’s single GET …/knowledge-bases/documents fetch. */
  documents: KnowledgeBaseDocument[];
  documentsLoading: boolean;
  documentsError: string | null;
  onAddSource: () => void;
  canAddSource: boolean;
  className?: string;
};

export function DatapackDetailKnowledgeTile({
  documents,
  documentsLoading,
  documentsError,
  onAddSource,
  canAddSource,
  className,
}: Props) {
  const total = documents.length;
  const preview = documents.slice(0, PREVIEW_LIMIT);
  const countLabel = documentsLoading && total === 0 ? "…" : String(total);

  return (
    <aside
      className={cn(
        "flex h-fit min-h-0 w-full flex-col self-start rounded-xl border border-border bg-card/50 shadow-sm",
        className,
      )}
    >
      <div className="flex items-center gap-2 border-b border-border px-4 py-3">
        <FileText className="h-4 w-4 shrink-0 text-emerald-400" aria-hidden />
        <span className="truncate text-sm font-semibold text-foreground">Knowledge</span>
        <span className="rounded-full bg-muted px-2 py-0.5 text-xs font-medium text-muted-foreground tabular-nums">
          {countLabel}
        </span>
      </div>
      <ul className="flex flex-col divide-y divide-border px-2 py-1">
        {documentsError ? (
          <li className="px-2 py-4 text-center text-xs leading-relaxed text-muted-foreground">{documentsError}</li>
        ) : documentsLoading && total === 0 ? (
          <li className="px-2 py-4 text-center text-xs text-muted-foreground">Loading sources…</li>
        ) : preview.length === 0 ? (
          <li className="px-2 py-4 text-center text-xs text-muted-foreground">No knowledge documents yet.</li>
        ) : (
          preview.map((doc, i) => {
            const name = doc.document_name?.trim() || "—";
            const last = documentNameLastSegment(name);
            return (
              <li key={`${i}-${name}`} className="flex items-start gap-3 px-2 py-2.5">
                <Link2 className="mt-0.5 h-4 w-4 shrink-0 text-emerald-400/90" aria-hidden />
                <div className="min-w-0 flex-1">
                  <p className="truncate font-mono text-[11px] leading-snug text-foreground/95" title={name}>
                    {last}
                  </p>
                  <p className="mt-0.5 truncate text-xs text-muted-foreground">
                    {kbDocumentStatusLabel(doc.status)}
                  </p>
                </div>
              </li>
            );
          })
        )}
      </ul>
      <div className="border-t border-border">
        <button
          type="button"
          className={cn(
            "w-full py-3 text-center text-sm font-normal transition-colors",
            canAddSource
              ? "cursor-pointer text-foreground/90 hover:text-foreground"
              : "cursor-not-allowed text-muted-foreground/55 opacity-60 hover:text-muted-foreground/55",
          )}
          disabled={!canAddSource}
          onClick={onAddSource}
        >
          + Add Knowledge
        </button>
      </div>
    </aside>
  );
}
