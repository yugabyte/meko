import { FileText } from "lucide-react";
import { useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { cn } from "@/lib/utils";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import type { KnowledgeBaseDocument } from "@/lib/api/mekoApi";
import { datapackKnowledgePath } from "@/app/routes/paths";
import { FilesTable } from "@/features/datapacks/components/knowledge-documents/KnowledgeFilesTable";
import { HoverInfoBadge } from "@/components/ui/hover-info-badge";
import { KNOWLEDGE_TILE_HINT } from "@/features/datapacks/components/datapack-detail/datapackTileHints";
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
  /** Deep-link arrival target -- see FilesTable's `highlightDocumentId`.
   *  The matching row (if present in this tile's capped preview) scrolls
   *  into view and runs the same one-time shimmer sweep as the full
   *  Files tab. */
  highlightDocumentId?: string;
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
  highlightDocumentId,
}: Props) {
  // Free-tier KB-storage cap. Read from the shared react-query cache
  // so this is a free read after any other gated surface has fetched.
  // Disable up front rather than rely on the BE 402 firing later —
  // catches the click earlier in the flow and stays consistent with
  // the DropdownMenuItem / FilesTab / WorkbenchLM gates.
  const navigate = useNavigate();
  const { data: accountUsage } = useAccountUsage();
  const { data: currentUser } = useCurrentUser();
  const freeTier = getFreeTierStatus(accountUsage, currentUser?.user_type);
  const blockedByFreeTier = freeTier.hasHitKbStorageFileSize;
  // Final disabled state: either the share-grant says no, or the
  // free-tier cap is hit. Two distinct reasons, one final flag.
  const addDisabled = !canAddSource || blockedByFreeTier;
  const previewLen = documents.length;
  const preview = documents.slice(0, KNOWLEDGE_PREVIEW_LIMIT);

  const goToFileRow = (doc: KnowledgeBaseDocument) => {
    navigate(datapackKnowledgePath(datapackId), {
      state: { tab: "files", highlightDocId: doc.id },
    });
  };

  // "Flash once, ever" -- once the target row has actually been found in
  // this preview and shown, permanently stop passing `highlightDocumentId`
  // down. We wait for the row to signal that the shimmer has actually
  // finished before consuming, to avoid clearing the ID while the 400ms
  // delay is still running.
  const [highlightConsumed, setHighlightConsumed] = useState(false);
  const activeHighlightDocumentId = highlightConsumed ? undefined : highlightDocumentId;

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
          <HoverInfoBadge variant="icon" label="What is knowledge?">
            {KNOWLEDGE_TILE_HINT}
          </HoverInfoBadge>
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
      {/* Same `FilesTable` the full Files tab uses (see
          KnowledgeFilesTable.tsx), just `compact` -- identical status
          badges/icons/tooltips here and on the KB page, no drift. No
          row expansion at this width, so per-row metadata the expand
          panel would show (embedding model, MD5, etc.) is irrelevant
          and left as safe placeholders below. */}
      <div className="min-h-0 flex-1 overflow-y-auto px-2 py-1">
        <FilesTable
          documents={preview}
          pendingPlaceholders={[]}
          loading={documentsLoading}
          error={documentsError}
          total={totalFileCount ?? previewLen}
          hasActiveFilter={false}
          embeddingModel={null}
          embeddingDimensions={null}
          chunkSize={null}
          datapackName=""
          datapackId={datapackId}
          callerUserId={null}
          datapackGrant={null}
          onDocumentDeleted={() => {}}
          compact
          emptyMessage="No knowledge documents yet."
          onRowClick={goToFileRow}
          highlightDocumentId={activeHighlightDocumentId}
          onHighlightConsumed={() => setHighlightConsumed(true)}
        />
      </div>
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
