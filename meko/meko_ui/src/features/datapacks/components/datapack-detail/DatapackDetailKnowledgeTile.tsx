import { FileText, Link2 } from "lucide-react";
import { Link } from "react-router-dom";
import { cn } from "@/lib/utils";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import type { KnowledgeBaseDocument } from "@/lib/api/mekoApi";
import { datapackKnowledgePath } from "@/app/routes/paths";
import { documentNameLastSegment } from "@/features/datapacks/components/knowledge-documents/knowledgeDocumentName";
import { kbDocumentStatusLabel } from "@/features/datapacks/components/knowledge-documents/knowledgeDocumentStatus";
import { useAccountUsage } from "@/hooks/useAccountUsage";
import { useCurrentUser } from "@/hooks/useCurrentUser";
import { blockedMessage, getFreeTierStatus } from "@/lib/freeTier";

export const KNOWLEDGE_PREVIEW_LIMIT = 3;

type Props = {
  datapackId: string;
  /** From parent’s single GET …/knowledge-bases/documents fetch. Capped
   *  at `KNOWLEDGE_PREVIEW_LIMIT` rows on the page side to keep the
   *  request light — we only need the preview rows here. */
  documents: KnowledgeBaseDocument[];
  documentsLoading: boolean;
  documentsError: string | null;
  totalFileCount?: number | null;
  onAddSource: () => void;
  canAddSource: boolean;
  className?: string;
};

export function DatapackDetailKnowledgeTile({
  datapackId,
  documents,
  documentsLoading,
  documentsError,
  totalFileCount,
  onAddSource,
  canAddSource,
  className,
}: Props) {
  // Free-tier KB-storage cap. Read from the shared react-query cache
  // so this is a free read after any other gated surface has fetched.
  // Disable up front rather than rely on the BE 402 firing later —
  // catches the click earlier in the flow and stays consistent with
  // the DropdownMenuItem / FilesTab / WorkbenchLM gates.
  const { data: accountUsage } = useAccountUsage();
  const { data: currentUser } = useCurrentUser();
  const freeTier = getFreeTierStatus(accountUsage, currentUser?.user_type);
  const blockedByFreeTier = freeTier.hasHitKbStorageFileSize;
  // Final disabled state: either the share-grant says no, or the
  // free-tier cap is hit. Two distinct reasons, one final flag.
  const addDisabled = !canAddSource || blockedByFreeTier;
  const previewLen = documents.length;
  const preview = documents.slice(0, KNOWLEDGE_PREVIEW_LIMIT);

  const chipTotal = totalFileCount ?? previewLen;
  const countLabel =
    documentsLoading && totalFileCount == null && previewLen === 0
      ? "…"
      : String(chipTotal);

  return (
    <aside
      className={cn(
        "flex h-full min-h-0 w-full flex-col rounded-xl border border-border bg-card/50 shadow-sm",
        className,
      )}
    >
      {/* Header layout mirrors DatapackDetailMembersTile: identity on
          the left, top-right action for the write path — here that's
          "+ Add Knowledge" (Members has "Share"). "View all →" moves
          to the tile footer so all detail tiles share the same shape. */}
      <div className="flex items-center justify-between gap-2 border-b border-border px-4 py-3">
        <div className="flex min-w-0 items-center gap-2">
          <FileText className="h-4 w-4 shrink-0 text-emerald-400" aria-hidden />
          <span className="truncate text-sm font-semibold text-foreground">Knowledge</span>
          <span className="rounded-full bg-muted px-2 py-0.5 text-xs font-medium text-muted-foreground tabular-nums">
            {countLabel}
          </span>
        </div>
        {blockedByFreeTier ? (
          <Tooltip>
            <TooltipTrigger asChild>
              <span className="inline-flex">
                <button
                  type="button"
                  disabled
                  className="shrink-0 cursor-not-allowed text-xs font-medium text-muted-foreground/55 opacity-60"
                >
                  + Add Knowledge
                </button>
              </span>
            </TooltipTrigger>
            <TooltipContent side="top" className="max-w-[280px] text-center">
              {blockedMessage("knowledgeUpload", currentUser?.user_type)}
            </TooltipContent>
          </Tooltip>
        ) : (
          <button
            type="button"
            disabled={addDisabled}
            onClick={onAddSource}
            className={cn(
              "shrink-0 text-xs font-medium transition-colors",
              addDisabled
                ? "cursor-not-allowed text-muted-foreground/55 opacity-60"
                : "cursor-pointer text-brand hover:underline",
            )}
          >
            + Add Knowledge
          </button>
        )}
      </div>
      <ul className="flex flex-col divide-y divide-border px-2 py-1">
        {documentsError ? (
          <li className="px-2 py-4 text-center text-xs leading-relaxed text-muted-foreground">{documentsError}</li>
        ) : documentsLoading && previewLen === 0 ? (
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
      {/* Footer link → full Shared Knowledge page. Same shape as the
          Members / Artifacts tile footers so the tile grid reads
          uniformly. */}
      <div className="mt-auto border-t border-border">
        <Link
          to={datapackKnowledgePath(datapackId)}
          className="block w-full py-3 text-center text-sm font-normal text-foreground/90 transition-colors hover:text-foreground"
        >
          View all →
        </Link>
      </div>
    </aside>
  );
}
