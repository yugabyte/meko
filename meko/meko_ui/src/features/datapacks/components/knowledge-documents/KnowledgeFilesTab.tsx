// "Files" tab on the SharedKnowledge summary page.
//
// Three stacked sections inside one card-style container:
//   1. FILES SUMMARY · AUTO + "Upload file" CTA. Heaviest source
//      wrapped to its own line for readability.
//   2. Drag-and-drop upload area (reuses the existing `FileUpload`
//      orchestrator so the upload pipeline + KB-create flow stay
//      shared with the modal version).
//   3. Filter row (file-type + status chips) + rows, rendered via the
//      shared `FilesTable`/`FilterRow` (see KnowledgeFilesTable.tsx) so
//      this tab and the main datapack page's compact preview always
//      look and behave identically. Each row is collapsible: click the
//      chevron to reveal the auto-summary, chunking/embedding metadata,
//      and per-file actions (Remove from KB, Download).
//
// Search is owned by the parent page and passed in as `searchQuery`
// so the same input filters across both tabs.

import { useState } from "react";
import { Database, Sparkles, Upload, X } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { useAccountUsage } from "@/hooks/useAccountUsage";
import { useCurrentUser } from "@/hooks/useCurrentUser";
import { blockedMessage, getFreeTierStatus } from "@/lib/freeTier";
import { cn } from "@/lib/utils";
import FileUpload from "@/features/datapacks/components/FileUpload";
import { FilesTable, FilterRow, PaginationRow } from "./KnowledgeFilesTable";
import { useKnowledgeFilesTable } from "./useKnowledgeFilesTable";
import { useArrivalFlash } from "@/hooks/useArrivalFlash";

/** Items per page shown in the files table. */
const PAGE_SIZE = 5;

type Props = {
  datapackName: string;
  datapackId: string;
  filesSummary: string;
  heaviestSourceName: string | null;
  /** Embedding model + dimensions + chunk_size are properties of the
   *  global vector index, not the document. Threaded down from
   *  `summary` so each row's expand panel can render them without
   *  bloating every doc payload with a repeated constant. */
  embeddingModel: string | null;
  embeddingDimensions: number | null;
  chunkSize: number | null;
  /** From the page-level shared search bar — forwarded to the BE as a
   *  document_name substring filter so search works across all pages. */
  searchQuery: string;
  /** Caller identity + datapack grant — together they decide whether
   *  the Delete button is shown per-row. Pass null for either to hide
   *  Delete entirely (defensive: an unresolved caller / grant should
   *  not be treated as "can delete"). */
  callerUserId: string | null;
  datapackGrant: string | null;
  /** Incremented by the parent after a delete or KB-create to trigger
   *  a re-fetch without unmounting this component. */
  refreshKey?: number;
  /** Fired after a successful delete — page re-fetches summary and activity log. */
  onDocumentDeleted: (documentId: string) => void;
  /** Called after createKnowledgeBase succeeds so the page can refresh the activity log. */
  onKnowledgeBaseCreated?: () => void;
  /** Deep-link signal (e.g. from the upload dialog's "Knowledge Base"
   *  heading, which links here): on arrival, scroll the Knowledge Base
   *  section into view and run a one-time shimmer sweep over it so the
   *  user's eye lands on the same table they were just looking at. */
  flash?: boolean;
  /** Deep-link signal from clicking a specific row in the upload
   *  dialog's table: scrolls that same document's row into view here
   *  and shimmers just that row, once it's loaded. A no-op if the
   *  document isn't on the currently-loaded page. */
  highlightDocumentId?: string;
  /** Fired after the highlight row has genuinely been found and shimmered,
   *  so the parent can track it and prevent re-shimmers on remount. */
  onHighlightConsumed?: (id: string) => void;
  /** Fired after the Knowledge Base section shimmer finishes. */
  onFlashConsumed?: () => void;
};

export function KnowledgeFilesTab({
  datapackName,
  datapackId,
  filesSummary,
  heaviestSourceName,
  embeddingModel,
  embeddingDimensions,
  chunkSize,
  searchQuery,
  callerUserId,
  datapackGrant,
  refreshKey,
  onDocumentDeleted,
  onKnowledgeBaseCreated,
  flash = false,
  onFlashConsumed,
  highlightDocumentId,
  onHighlightConsumed,
}: Props) {
  const [uploadOpen, setUploadOpen] = useState(false);
  const {
    typeFilter,
    handleTypeFilterChange,
    statusFilter,
    handleStatusFilterChange,
    page,
    setPage,
    totalPages,
    hasMore,
    documents,
    total,
    loading,
    error,
    pendingPlaceholders,
    hasActiveFilter,
    availableTypeFilters,
    availableStatusFilters,
    pollTick,
    handleQueuedDocsChange,
    setUploadProcessingActive,
    activeHighlightDocumentId,
    consumeHighlight,
  } = useKnowledgeFilesTable({
    datapackId,
    datapackName,
    searchQuery,
    pageSize: PAGE_SIZE,
    refreshKey,
    highlightDocumentId,
    onHighlightConsumed,
  });

  // Deep-link arrival with no specific row in mind (the upload dialog's
  // "Knowledge Base" heading): scroll the section into view and flash it
  // once. The key is constant because there's only ever one section to
  // flash -- the hook's per-key guard then makes this fire once per
  // mount no matter how often `flash` is re-delivered.
  const { ref: sectionRef, flashing: shimmer } = useArrivalFlash<HTMLDivElement>({
    flashKey: "knowledge-base-section",
    active: flash,
    onDone: onFlashConsumed,
    block: "start",
    debugLabel: "KnowledgeFilesTab.section",
  });

  return (
    <div className="space-y-6">
      <SummaryCard
        filesSummary={filesSummary}
        heaviestSourceName={heaviestSourceName}
        onUploadClick={() => setUploadOpen((v) => !v)}
        uploadOpen={uploadOpen}
      />

      {/* Always mounted, hidden via CSS -- collapsing used to unmount
          FileUpload and lose staged files. `key` still remounts on
          datapack switch so state doesn't leak across datapacks. */}
      <section className={cn("rounded-xl border border-border bg-card p-6", !uploadOpen && "hidden")}>
        <FileUpload
          key={datapackId}
          datapackName={datapackName}
          datapackId={datapackId}
          onKnowledgeBaseCreated={onKnowledgeBaseCreated}
          onQueuedDocsChange={handleQueuedDocsChange}
          onProcessingActiveChange={setUploadProcessingActive}
          refreshKey={refreshKey}
          pollTick={pollTick}
        />
      </section>

      <div
        ref={sectionRef}
        className={cn("space-y-6 rounded-xl p-2", shimmer && "meko-files-flash")}
      >
        {/* One-time pulse for a deep-linked arrival from the upload
            dialog -- emerald matches the KB's accent color. */}
        <style>{`
          @keyframes meko-files-flash {
            0% {
              box-shadow: inset 0 0 0 2px rgba(16,185,129,0.55);
              background-color: rgba(16,185,129,0.16);
            }
            70% {
              box-shadow: inset 0 0 0 2px rgba(16,185,129,0.3);
              background-color: rgba(16,185,129,0.08);
            }
            100% {
              box-shadow: inset 0 0 0 2px rgba(16,185,129,0);
              background-color: rgba(16,185,129,0);
            }
          }
          .meko-files-flash {
            animation: meko-files-flash 1.6s ease-out forwards;
          }
        `}</style>

        <div className="flex items-center gap-3">
          <Database className="w-5 h-5 text-purple-400" aria-hidden />
          <h3 className="text-sm font-medium text-foreground">Knowledge Base</h3>
        </div>

        <FilterRow
          typeFilter={typeFilter}
          onTypeFilterChange={handleTypeFilterChange}
          statusFilter={statusFilter}
          onStatusFilterChange={handleStatusFilterChange}
          availableTypes={availableTypeFilters}
          availableStatuses={availableStatusFilters}
        />

        <FilesTable
          documents={documents}
          pendingPlaceholders={pendingPlaceholders}
          loading={loading}
          error={error}
          total={total}
          hasActiveFilter={hasActiveFilter}
          embeddingModel={embeddingModel}
          embeddingDimensions={embeddingDimensions}
          chunkSize={chunkSize}
          datapackName={datapackName}
          datapackId={datapackId}
          callerUserId={callerUserId}
          datapackGrant={datapackGrant}
          onDocumentDeleted={onDocumentDeleted}
          highlightDocumentId={activeHighlightDocumentId}
          onHighlightConsumed={consumeHighlight}
        />

        {totalPages > 1 || hasMore ? (
          <PaginationRow
            page={page}
            totalPages={totalPages}
            loading={loading}
            onPrev={() => setPage((p) => Math.max(1, p - 1))}
            onNext={() => setPage((p) => Math.min(totalPages, p + 1))}
          />
        ) : null}
      </div>
    </div>
  );
}

function SummaryCard({
  filesSummary,
  heaviestSourceName,
  onUploadClick,
  uploadOpen,
}: {
  filesSummary: string;
  heaviestSourceName: string | null;
  onUploadClick: () => void;
  uploadOpen: boolean;
}) {
  // Free-tier KB-storage cap. We only gate the "Upload file" affordance
  // (the open-panel button) — the "Close" variant stays enabled so a
  // user mid-flow can dismiss the panel even if a cap is hit by then.
  const { data: accountUsage } = useAccountUsage();
  const { data: currentUser } = useCurrentUser();
  const freeTier = getFreeTierStatus(accountUsage, currentUser?.user_type);
  // Split the BE-rendered summary into the head sentence + the
  // "Heaviest source" sentence so the latter renders on its own line.
  // The BE composes them as "N files totaling M chunks. Heaviest
  // source: foo." — we display them stacked.
  const [head, heaviest] = splitSummary(filesSummary, heaviestSourceName);
  return (
    <section className="rounded-xl border border-border bg-card p-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-2">
            <Sparkles className="h-3.5 w-3.5 text-emerald-700 dark:text-emerald-300" aria-hidden />
            <span className="text-[10px] font-semibold uppercase tracking-wider text-emerald-700 dark:text-emerald-200/95">
              Files summary · auto
            </span>
          </div>
          <p className="mt-2 text-sm leading-relaxed text-foreground/90">
            {head || "No files uploaded yet."}
          </p>
          {heaviest ? (
            <p className="mt-1 text-sm leading-relaxed text-foreground/80">{heaviest}</p>
          ) : null}
        </div>
        {uploadOpen ? (
          <Button
            type="button"
            variant="outline"
            onClick={onUploadClick}
            className="shrink-0 gap-1.5"
            aria-label="Close upload panel"
          >
            <X className="h-3.5 w-3.5" aria-hidden />
            Close
          </Button>
        ) : freeTier.hasHitKbStorageFileSize ? (
          <Tooltip>
            <TooltipTrigger asChild>
              <span className="inline-flex shrink-0">
                <Button
                  type="button"
                  variant="outline"
                  onClick={onUploadClick}
                  disabled
                  className="shrink-0 gap-1.5"
                >
                  <Upload className="h-3.5 w-3.5" aria-hidden />
                  Upload file
                </Button>
              </span>
            </TooltipTrigger>
            <TooltipContent side="top" className="max-w-[280px] text-center">
              {blockedMessage("knowledgeUpload", currentUser?.user_type)}
            </TooltipContent>
          </Tooltip>
        ) : (
          <Button
            type="button"
            variant="outline"
            onClick={onUploadClick}
            className="shrink-0 gap-1.5"
          >
            <Upload className="h-3.5 w-3.5" aria-hidden />
            Upload file
          </Button>
        )}
      </div>
    </section>
  );
}

/** Split the server-rendered summary into the "head" (totals) sentence
 *  and the "Heaviest source: …" sentence so we can render them on
 *  separate lines. Falls back gracefully when the heaviest part isn't
 *  present (no files, or no doc has chunks). */
function splitSummary(
  raw: string,
  heaviestSourceName: string | null,
): [string, string] {
  if (!raw) return ["", ""];
  const idx = raw.indexOf("Heaviest source:");
  if (idx < 0) return [raw, ""];
  const head = raw.slice(0, idx).trimEnd();
  const heaviestRaw = raw.slice(idx).trim();
  // Prefer the basename when the BE handed us one (it already
  // basenames in the summary endpoint, but be defensive).
  const heaviestDisplay = heaviestSourceName
    ? `Heaviest source: ${heaviestSourceName}.`
    : heaviestRaw;
  return [head, heaviestDisplay];
}
