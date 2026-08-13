// Shared files-table building blocks for the knowledge base document list.
//
// Used by both the full "Files" tab on the SharedKnowledge summary page
// (`KnowledgeFilesTab`) and the compact preview on the main datapack
// detail page (`DatapackDetailKnowledgeTile`) so both surfaces render
// exactly the same status badges, icons, and row behavior instead of
// drifting apart. `compact` trims the grid down to File + Status only,
// for a narrower container that can't fit the full 7-column layout.

import {
  Children,
  memo,
  type ComponentType,
  type ReactNode,
  type SVGProps,
  useCallback,
  useEffect,
  useRef,
  useState,
} from "react";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  Check,
  ChevronDown,
  ChevronLeft,
  ChevronRight,
  Download,
  File as FileIcon,
  Loader2,
  Sparkles,
  Trash2,
} from "lucide-react";
import {
  FileTypeCsvIcon,
  FileTypeHtmlIcon,
  FileTypeJsonIcon,
  FileTypeMarkdownIcon,
  FileTypePdfIcon,
  FileTypeTxtIcon,
} from "./fileTypeIcons";
import { Button } from "@/components/ui/button";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { cn } from "@/lib/utils";
import { formatFileSize } from "@/lib/fileSize";
import { formatRelativeTime, useNowMs } from "@/lib/relativeTime";
import { KNOWLEDGE_UPLOAD_ACCEPTED_EXTENSION_LIST } from "@/lib/knowledgeFileUploadRules";
import {
  deleteKnowledgeBaseDocument,
  getApiErrorMessage,
  requestKnowledgeBaseDownloadURL,
  type KnowledgeBaseDocument,
} from "@/lib/api/mekoApi";
import { toast } from "@/hooks/use-toast";
import { useArrivalFlash } from "@/hooks/useArrivalFlash";
import {
  documentFileExtension,
  documentNameForMessage,
  documentNameLastSegment,
} from "./knowledgeDocumentName";
import {
  KB_DOCUMENT_STATUS,
  isKbDocumentInFlight,
  kbDocumentStatusLabel,
  kbDocumentStatusTone,
  kbDocumentStatusTooltip,
} from "./knowledgeDocumentStatus";

/** Permission policy for the per-row Delete button. Mirrors the BE
 *  rule in `canDeleteKBDocument` (handlers/knowledge_base_handler.go):
 *  owner / maintainer of the datapack OR uploader of THIS file can
 *  delete. Viewers/contributors who didn't upload the file are denied.
 *  The BE re-enforces; this just hides the button when we already
 *  know the call would 403. */
function canDeleteKBDocumentClient(
  datapackGrant: string | null,
  callerUserId: string | null,
  fileUploaderUserId: string | null | undefined,
): boolean {
  const g = (datapackGrant ?? "").toLowerCase().trim();
  if (g === "owner" || g === "maintainer") return true;
  if (!callerUserId || !fileUploaderUserId) return false;
  return callerUserId === fileUploaderUserId;
}

/** Per-extension glyph for the file row. PDF/JSON/HTML use the
 *  vscode-icons "badge" artwork (see fileTypeIcons.tsx) -- full-color
 *  pictograms with every fill baked in, so no tone class is needed (or
 *  wanted; `text-*` wouldn't touch a hardcoded `fill`). TXT/CSV/Markdown
 *  instead use the outline-style, `currentColor` Tabler glyphs (the
 *  vscode-icons badges for those three read as too flat/plain at this
 *  size), which do need a tone class for their color. Unknown / unset
 *  types fall through to a plain document glyph in the section's
 *  emerald accent.
 *  Takes a file *extension* (".pdf", ".md", …), not `document_type` --
 *  that field is the S3 object's MIME content-type (e.g. "text/csv"),
 *  which never matches these cases; callers should extract the
 *  extension from the document name via `documentFileExtension` first. */
function fileTypeVisual(
  extension?: string | null,
): { Icon: ComponentType<SVGProps<SVGSVGElement>>; toneClass: string } {
  switch ((extension || "").toLowerCase()) {
    case ".pdf":
      return { Icon: FileTypePdfIcon, toneClass: "text-red-700 dark:text-red-400" };
    case ".md":
      return { Icon: FileTypeMarkdownIcon, toneClass: "text-blue-700 dark:text-blue-300" };
    case ".txt":
      return { Icon: FileTypeTxtIcon, toneClass: "text-sky-700 dark:text-sky-300" };
    case ".json":
      return { Icon: FileTypeJsonIcon, toneClass: "" };
    case ".csv":
      return { Icon: FileTypeCsvIcon, toneClass: "text-emerald-700 dark:text-emerald-300" };
    case ".html":
    case ".htm":
      return { Icon: FileTypeHtmlIcon, toneClass: "" };
    default:
      return { Icon: FileIcon, toneClass: "text-emerald-700 dark:text-emerald-300" };
  }
}

/** File-type chip values. "all" is the default (no filter); the rest
 *  mirror the upload allow-list so a file type can't be filterable here
 *  without also being uploadable (and vice versa). */
export const FILE_TYPE_FILTERS = [
  { id: "all" as const, label: "All types" },
  ...KNOWLEDGE_UPLOAD_ACCEPTED_EXTENSION_LIST.map((ext) => ({ id: ext, label: ext })),
];
export type FileTypeFilter = (typeof FILE_TYPE_FILTERS)[number]["id"];

/** Status chip values for the filter row, alongside the file-type chips.
 *  "all" means no status restriction; the rest are exact matches against
 *  {@link KB_DOCUMENT_STATUS}. FAILED is deliberately omitted from the UI
 *  today -- there's no dedicated "Failed" affordance elsewhere yet, and
 *  adding the filter alone wouldn't be actionable. */
export const STATUS_FILTERS = [
  { id: "all" as const, label: "All statuses" },
  { id: KB_DOCUMENT_STATUS.COMPLETED, label: "Indexed" },
  { id: KB_DOCUMENT_STATUS.QUEUED, label: "Queued" },
  { id: KB_DOCUMENT_STATUS.PROCESSING, label: "Processing" },
] as const;
export type StatusFilter = (typeof STATUS_FILTERS)[number]["id"];

const STATUS_TONE: Record<string, string> = {
  muted: "border-border/60 bg-muted/30 text-muted-foreground",
  blue: "border-blue-500/35 bg-blue-500/10 text-blue-700 dark:text-blue-200",
  yellow: "border-yellow-500/35 bg-yellow-500/10 text-yellow-700 dark:text-yellow-300",
  green: "border-emerald-500/35 bg-emerald-500/10 text-emerald-700 dark:text-emerald-200",
  red: "border-red-500/35 bg-red-500/10 text-red-700 dark:text-red-200",
};

/** Solid dot color per status tone, used as a small leading indicator in
 *  the status filter dropdown (mirrors the badge's tone at a glance
 *  without repeating the full pill treatment inside the menu). */
const STATUS_DOT: Record<string, string> = {
  muted: "bg-muted-foreground/50",
  blue: "bg-blue-500",
  yellow: "bg-yellow-500",
  green: "bg-emerald-500",
  red: "bg-red-500",
};

/** UI status label. We surface "indexed" instead of "Completed" when
 *  the file is fully ingested — that's the user-facing meaning. Other
 *  states keep their natural names. */
function statusLabel(s: string): string {
  switch (s) {
    case KB_DOCUMENT_STATUS.COMPLETED:
      return "indexed";
    case KB_DOCUMENT_STATUS.QUEUED:
      return "Queued";
    case KB_DOCUMENT_STATUS.PROCESSING:
      return "Processing";
    case KB_DOCUMENT_STATUS.FAILED:
      return "Failed";
    default:
      return s;
  }
}

/** Status badge with hover tooltip explaining the lifecycle step. */
function StatusBadge({ status }: { status: string }) {
  const tooltip = kbDocumentStatusTooltip(status);
  const tone = kbDocumentStatusTone(status);
  const label = statusLabel(status);

  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span
          className={cn(
            "inline-flex items-center rounded-full border px-2 py-0.5 text-[10px] font-semibold uppercase tracking-wide cursor-default transition-colors",
            STATUS_TONE[tone],
          )}
        >
          {label}
        </span>
      </TooltipTrigger>
      {tooltip ? (
        <TooltipContent side="top" className="max-w-[280px] text-center">
          {tooltip}
        </TooltipContent>
      ) : null}
    </Tooltip>
  );
}

/** Filter row: a Type dropdown + a Status dropdown, each a single select
 *  ("all" clears the respective filter, matching how each maps to a
 *  single backend query param -- type_filter / status_filter).
 *  Previously this rendered every possible chip as its own button (10+
 *  across two wrapping rows); collapsed to two compact dropdowns so the
 *  row stays a single line regardless of how many file types/statuses
 *  exist.
 *
 *  `availableTypes`/`availableStatuses` (raw values sampled from this
 *  datapack's actual documents -- see useKnowledgeFilesTable's
 *  `availableTypeFilters`/`availableStatusFilters`) restrict each
 *  dropdown's options to ones that would actually match something, e.g.
 *  no point offering ".json" when nothing of that type has been
 *  uploaded. Omit either list (or leave it empty, e.g. before the first
 *  fetch resolves) to fall back to showing every possible option. */
export function FilterRow({
  typeFilter,
  onTypeFilterChange,
  statusFilter,
  onStatusFilterChange,
  availableTypes,
  availableStatuses,
}: {
  typeFilter: FileTypeFilter;
  onTypeFilterChange: (next: FileTypeFilter) => void;
  statusFilter: StatusFilter;
  onStatusFilterChange: (next: StatusFilter) => void;
  availableTypes?: string[];
  availableStatuses?: string[];
}) {
  const typeOptions = FILE_TYPE_FILTERS.filter(
    (f) => f.id === "all" || !availableTypes?.length || availableTypes.includes(f.id),
  );
  const statusOptions = STATUS_FILTERS.filter(
    (f) => f.id === "all" || !availableStatuses?.length || availableStatuses.includes(f.id),
  );
  // Nothing beyond "All types"/"All statuses" to pick from -- an empty
  // datapack, or one where every document shares the same type/status.
  // A lone dropdown that can't actually filter anything is just noise.
  const showType = typeOptions.length > 1;
  const showStatus = statusOptions.length > 1;
  if (!showType && !showStatus) return null;

  const triggerClass =
    "h-9 w-auto gap-2 rounded-lg border-border/80 bg-background px-3 text-xs font-medium text-foreground shadow-sm transition-colors hover:bg-accent/50 hover:border-border focus:ring-2 focus:ring-ring/40 data-[state=open]:border-ring/60 data-[state=open]:ring-2 data-[state=open]:ring-ring/40 [&>span]:line-clamp-1 [&>span]:flex [&>span]:items-center [&>span]:gap-2 [&_svg]:h-4 [&_svg]:w-4 [&_svg]:shrink-0";
  const contentClass = "rounded-lg border-border/80 shadow-lg";
  const itemClass = "gap-2 rounded-md py-1.5 text-xs [&:not(:last-child)]:mb-0.5";

  return (
    <div className="flex flex-wrap items-center gap-2.5">
      <span className="text-xs font-medium text-muted-foreground">Filter</span>
      {showType ? (
        <Select value={typeFilter} onValueChange={(v) => onTypeFilterChange(v as FileTypeFilter)}>
          <SelectTrigger className={triggerClass}>
            <SelectValue />
          </SelectTrigger>
          <SelectContent className={contentClass}>
            {typeOptions.map((f) => {
              if (f.id === "all") {
                return (
                  <SelectItem key={f.id} value={f.id} className={itemClass}>
                    {f.label}
                  </SelectItem>
                );
              }
              const { Icon, toneClass } = fileTypeVisual(f.id);
              return (
                <SelectItem key={f.id} value={f.id} className={itemClass}>
                  <span className="flex items-center gap-2">
                    <Icon className={cn("h-4 w-4 shrink-0", toneClass)} aria-hidden />
                    <span>{f.label}</span>
                  </span>
                </SelectItem>
              );
            })}
          </SelectContent>
        </Select>
      ) : null}
      {showStatus ? (
        <Select value={statusFilter} onValueChange={(v) => onStatusFilterChange(v as StatusFilter)}>
          <SelectTrigger className={triggerClass}>
            <SelectValue />
          </SelectTrigger>
          <SelectContent className={contentClass}>
            {statusOptions.map((f) => {
              if (f.id === "all") {
                return (
                  <SelectItem key={f.id} value={f.id} className={itemClass}>
                    {f.label}
                  </SelectItem>
                );
              }
              const dot = STATUS_DOT[kbDocumentStatusTone(f.id)] ?? STATUS_DOT.muted;
              return (
                <SelectItem key={f.id} value={f.id} className={itemClass}>
                  <span className="flex items-center gap-2">
                    <span className={cn("h-2 w-2 shrink-0 rounded-full", dot)} aria-hidden />
                    <span>{f.label}</span>
                  </span>
                </SelectItem>
              );
            })}
          </SelectContent>
        </Select>
      ) : null}
    </div>
  );
}

/** Prev / Next pagination footer below the files table. Shared so every
 *  surface with a paginated `FilesTable` (the full Files tab and the
 *  upload dialog) gets the identical control instead of a bespoke one
 *  per call site. */
export function PaginationRow({
  page,
  totalPages,
  loading,
  onPrev,
  onNext,
}: {
  page: number;
  totalPages: number;
  loading: boolean;
  onPrev: () => void;
  onNext: () => void;
}) {
  return (
    <div className="flex items-center justify-between gap-4 px-1">
      <Button
        type="button"
        variant="outline"
        size="sm"
        disabled={page <= 1 || loading}
        onClick={onPrev}
        className="gap-1.5"
      >
        <ChevronLeft className="h-3.5 w-3.5" aria-hidden />
        Prev
      </Button>

      {/* Stays put regardless of `loading` -- the rows area (see
          RowsList) is where the loading state shows now, so this
          doesn't change shape mid-fetch. */}
      <span className="text-xs text-muted-foreground tabular-nums">
        Page <span className="font-medium text-foreground">{page}</span> of{" "}
        <span className="font-medium text-foreground">{totalPages}</span>
      </span>

      <Button
        type="button"
        variant="outline"
        size="sm"
        disabled={page >= totalPages || loading}
        onClick={onNext}
        className="gap-1.5"
      >
        Next
        <ChevronRight className="h-3.5 w-3.5" aria-hidden />
      </Button>
    </div>
  );
}

/** Grid template shared by the header and every row so columns always
 *  line up. `compact` drops Size/Uploaded/MD5, leaving just the
 *  chevron, File, (optionally Chunks), and Status -- enough to fit a
 *  narrow tile or a dialog without the full 7-column layout wrapping
 *  awkwardly. `showChunks` opts a compact table back into the Chunks
 *  column alone (e.g. the upload dialog, which cares whether a file is
 *  still being chunked but has no room for Size/Uploaded/MD5 too). */
function rowGridClass(compact: boolean, showChunks: boolean): string {
  if (!compact) {
    return "md:grid-cols-[24px_minmax(0,2fr)_minmax(0,0.7fr)_minmax(0,0.6fr)_minmax(0,0.9fr)_minmax(0,1fr)_minmax(0,0.7fr)]";
  }
  return showChunks
    ? "md:grid-cols-[24px_minmax(0,1fr)_minmax(0,0.5fr)_minmax(0,0.6fr)]"
    : "md:grid-cols-[24px_minmax(0,1fr)_minmax(0,0.6fr)]";
}

export function FilesTable({
  documents,
  pendingPlaceholders,
  loading,
  error,
  total,
  hasActiveFilter,
  embeddingModel,
  embeddingDimensions,
  chunkSize,
  datapackName,
  datapackId,
  callerUserId,
  datapackGrant,
  onDocumentDeleted,
  compact = false,
  showChunks = false,
  emptyMessage,
  onRowClick,
  highlightDocumentId,
  onHighlightConsumed,
}: {
  documents: KnowledgeBaseDocument[];
  /** Just-uploaded files with no dist_rag row yet -- see
   *  queuedPlaceholders in KnowledgeFilesTab. Rendered as inert rows
   *  (status only, no size/chunks/MD5/actions) ahead of the real ones. */
  pendingPlaceholders: { name: string; status: string }[];
  loading: boolean;
  error: string | null;
  /** Total matching documents across all pages (from server). */
  total: number;
  hasActiveFilter: boolean;
  embeddingModel: string | null;
  embeddingDimensions: number | null;
  chunkSize: number | null;
  datapackName: string;
  datapackId: string;
  callerUserId: string | null;
  datapackGrant: string | null;
  onDocumentDeleted: (documentId: string) => void;
  /** Hides Size/Chunks/Uploaded/MD5 and disables row expansion -- for a
   *  narrow preview surface rather than the full Files tab. */
  compact?: boolean;
  /** Only meaningful alongside `compact` -- opts back into a single
   *  Chunks column (e.g. the upload dialog, which wants File + Chunks +
   *  Status but not the other compact-hidden columns). Ignored when
   *  `compact` is false, since the full table always shows Chunks. */
  showChunks?: boolean;
  emptyMessage?: string;
  /** When set, rows navigate instead of expanding in place -- see the
   *  matching doc on FileRow's `onRowClick` prop. */
  onRowClick?: (doc: KnowledgeBaseDocument) => void;
  /** Deep-link arrival target: the row whose `id` matches gets scrolled
   *  into view and given a one-time shimmer sweep (see FileRow). */
  highlightDocumentId?: string;
  /** Fired after the highlight row has genuinely been found and shimmered. */
  onHighlightConsumed?: (id: string) => void;
}) {
  if (error) {
    return (
      <div
        className="rounded-md border border-red-500/30 bg-red-500/10 px-4 py-3 text-sm text-red-700 dark:text-red-200"
        role="alert"
      >
        {error}
      </div>
    );
  }
  if (loading && documents.length === 0 && pendingPlaceholders.length === 0) {
    return (
      <div className="rounded-xl border border-dashed border-border bg-muted/5 px-6 py-12 text-center text-sm text-muted-foreground">
        Loading documents…
      </div>
    );
  }
  if (documents.length === 0 && pendingPlaceholders.length === 0) {
    return (
      <div className="rounded-xl border border-dashed border-border bg-muted/5 px-6 py-12 text-center text-sm text-muted-foreground">
        {emptyMessage ??
          (total === 0 && !hasActiveFilter
            ? "No knowledge documents yet. Upload files to see them here."
            : "No files match the current search or filter.")}
      </div>
    );
  }

  return (
    <section className="rounded-xl border border-border bg-card">
      {/* Below md: the header is unreadable on a narrow viewport, and the
          row cells below stack vertically anyway -- so hide the header
          to remove visual clutter and rely on per-cell labels instead. */}
      <header
        className={cn(
          "hidden grid gap-3 border-b border-border/60 px-5 py-3 text-[10px] font-semibold uppercase tracking-wider text-muted-foreground md:grid",
          rowGridClass(compact, showChunks),
        )}
      >
        <span aria-hidden />
        <span>File</span>
        {compact ? (
          showChunks ? <span>Chunks</span> : null
        ) : (
          <>
            <span>Size</span>
            <span>Chunks</span>
            <span>Uploaded</span>
            <span className="font-mono">MD5</span>
          </>
        )}
        <span>Status</span>
      </header>
      <RowsList
        loading={loading}
        hasPendingPlaceholders={pendingPlaceholders.length > 0}
        // Below the fold change: a page with fewer rows than a prior one
        // would otherwise shrink the list (and shift the pagination
        // footer up with it) -- RowsList reserves room for a full page to
        // stop that jump. `total` caps the reservation so a result set
        // that has shrunk below a page (files deleted) stops reserving
        // room for rows that no longer exist.
        total={total}
      >
        {pendingPlaceholders.map(({ name, status }) => (
          <PendingFileRow
            key={`pending-${name}`}
            name={name}
            status={status}
            compact={compact}
            showChunks={showChunks}
          />
        ))}
        {documents.map((d) => (
          <FileRow
            key={d.id}
            doc={d}
            embeddingModel={embeddingModel}
            embeddingDimensions={embeddingDimensions}
            chunkSize={chunkSize}
            datapackName={datapackName}
            datapackId={datapackId}
            callerUserId={callerUserId}
            datapackGrant={datapackGrant}
            onDocumentDeleted={onDocumentDeleted}
            compact={compact}
            showChunks={showChunks}
            onRowClick={onRowClick}
            highlight={highlightDocumentId != null && d.id === highlightDocumentId}
            onHighlightConsumed={onHighlightConsumed}
          />
        ))}
      </RowsList>
      {/* One-time pulse for a deep-linked row (see highlightDocumentId).
          Emerald shimmer sweep matches the KB's accent color. */}
      <style>{`
        @keyframes meko-file-row-sweep {
          0%   { transform: translateX(-100%); }
          100% { transform: translateX(100%); }
        }
        @keyframes meko-file-row-glow {
          0%   { background-color: rgba(16,185,129,0.22); }
          70%  { background-color: rgba(16,185,129,0.12); }
          100% { background-color: rgba(16,185,129,0); }
        }
        .meko-file-row-sweep {
          animation: meko-file-row-sweep 1.2s ease-in-out forwards;
          background: linear-gradient(
            100deg,
            transparent 20%,
            rgba(16,185,129,0.45) 50%,
            transparent 80%
          );
        }
        .dark .meko-file-row-sweep {
          background: linear-gradient(
            100deg,
            transparent 20%,
            rgba(52,211,153,0.5) 50%,
            transparent 80%
          );
        }
        .meko-file-row-glow {
          animation: meko-file-row-glow 1.6s ease-out forwards;
        }
      `}</style>
    </section>
  );
}

/** Wraps the row `<ul>` with things a plain list doesn't get for free:
 *   1. A min-height pinned to the tallest page rendered so far. Paging
 *      to a shorter page (fewer rows, or rows without an expanded
 *      panel) would otherwise shrink this element and visibly shift
 *      the Prev/Next footer below it up -- growing-only avoids that
 *      without hardcoding a row-height guess that would drift out of
 *      sync with real content (wrapped filenames, compact vs. full
 *      grid, etc.). This matches the general pagination guidance of
 *      keeping the footer's position fixed rather than letting it jump
 *      (e.g. https://ux.stackexchange.com/questions/78911) -- but that
 *      guidance also warns a bare blank void below a short last page
 *      reads as broken/unfinished, so a trailing "That's all N files"
 *      divider marks the real end of content right where the last row
 *      left off, before the padding starts.
 *   2. A quiet loading overlay over the *rows* while a background
 *      re-fetch (e.g. Next/Prev, or a filter change) is in flight --
 *      rather than the old approach of swapping the pagination
 *      footer's "Page X of Y" text for a spinner, which made the one
 *      piece of UI that's supposed to stay put change shape instead. */
function RowsList({
  loading,
  hasPendingPlaceholders = false,
  total = 0,
  children,
}: {
  loading: boolean;
  /** True while transient "still uploading" placeholder rows (see
   *  PendingFileRow) are stacked on top of a real page. These come and
   *  go independently of pagination, so a page that happens to be
   *  showing several at once must NOT get baked into the standing
   *  min-height below -- otherwise the space they took up stays
   *  reserved forever after they clear, even though no real page is
   *  ever that tall. */
  hasPendingPlaceholders?: boolean;
  /** Documents matching the current filter, across all pages. Caps the
   *  reservation: once fewer files exist than a page holds, reserving a
   *  full page's worth leaves a void no page can ever fill. */
  total?: number;
  children: ReactNode;
}) {
  const ref = useRef<HTMLUListElement>(null);
  const [rowHeight, setRowHeight] = useState(0);
  const [maxRowCount, setMaxRowCount] = useState(0);
  const rowCount = Children.count(children);

  useEffect(() => {
    if (loading || hasPendingPlaceholders) return; // reflects the outgoing page, or an inflated placeholder count; skip
    const el = ref.current;
    if (!el) return;
    const rows = Array.from(el.children).filter(
      (c): c is HTMLElement => c instanceof HTMLElement && c.dataset.endDivider !== "1",
    );
    if (rows.length === 0) return;
    // One *collapsed* row, taken as the shortest on the page. Measuring
    // the <ul> as a whole (its scrollHeight) instead folds in whichever
    // rows the user happens to have expanded, and since the expanded
    // panel is several times a row's height and the reservation only
    // ever grew, one expand pinned a void under the list for the rest of
    // the session. Deleting made it obvious because "Remove from KB"
    // lives inside that panel -- you cannot delete without expanding
    // first -- but collapsing the row again was enough to show it.
    const collapsed = Math.round(Math.min(...rows.map((r) => r.offsetHeight)));
    if (collapsed > 0 && Math.abs(collapsed - rowHeight) > 1) setRowHeight(collapsed);
    if (rows.length > maxRowCount) setMaxRowCount(rows.length);
  }, [children, loading, hasPendingPlaceholders, rowHeight, maxRowCount]);

  // A full page's worth of collapsed rows, never more than the result
  // set actually holds.
  const reservedRows = total > 0 ? Math.min(maxRowCount, total) : maxRowCount;
  const minHeight = rowHeight * reservedRows;

  // Only worth a divider once we've seen a taller page (so a datapack
  // that's only ever had one small page never shows it) and this page is
  // genuinely shorter -- not while loading, when rowCount still
  // describes the outgoing page.
  const showEndDivider = !loading && rowCount > 0 && rowCount < reservedRows;

  return (
    <div className="relative">
      <ul
        ref={ref}
        className="divide-y divide-border/40"
        style={minHeight ? { minHeight } : undefined}
      >
        {children}
        {showEndDivider ? (
          <li
            aria-hidden
            data-end-divider="1"
            className="px-4 py-3 text-center text-[11px] text-muted-foreground/60 md:px-5"
          >
            — end of page —
          </li>
        ) : null}
      </ul>
      {loading ? (
        <div
          className="absolute inset-0 flex items-start justify-center bg-card/60 pt-10 backdrop-blur-[1px]"
          aria-live="polite"
          aria-busy="true"
        >
          <Loader2 className="h-5 w-5 animate-spin text-muted-foreground" aria-hidden />
          <span className="sr-only">Loading documents…</span>
        </div>
      ) : null}
    </div>
  );
}

/** Placeholder row for a file uploaded but not yet discovered by dist_rag --
 *  no size/chunks/MD5/actions yet, since none of that exists server-side
 *  for it. Status/tone reuse the same helpers as a real row (`statusLabel`,
 *  `kbDocumentStatusTone`) so "Queued" vs "Processing" here matches
 *  exactly what a real row would show once discovered. Not expandable;
 *  disappears once the real row arrives. */
function PendingFileRow({
  name,
  status,
  compact,
  showChunks,
}: {
  name: string;
  status: string;
  compact: boolean;
  showChunks: boolean;
}) {
  return (
    <li>
      <div
        className={cn(
          "grid grid-cols-1 gap-1 px-4 py-3 md:gap-3 md:px-5",
          rowGridClass(compact, showChunks),
        )}
      >
        <span aria-hidden className="hidden md:block" />
        <span className="flex min-w-0 items-center gap-2">
          {(() => {
            const { Icon, toneClass } = fileTypeVisual(documentFileExtension(name));
            return <Icon className={cn("h-5 w-5 shrink-0", toneClass)} aria-hidden />;
          })()}
          <span className="truncate text-sm text-foreground" title={name}>
            {name}
          </span>
        </span>
        {compact ? (
          showChunks ? <span className="self-center text-sm text-muted-foreground">—</span> : null
        ) : (
          <>
            <span className="self-center text-sm text-muted-foreground">—</span>
            <span className="self-center text-sm text-muted-foreground">—</span>
            <span className="self-center text-sm text-muted-foreground">—</span>
            <span className="self-center font-mono text-xs text-muted-foreground/90">—</span>
          </>
        )}
        <span className="self-center">
          <StatusBadge status={status} />
        </span>
      </div>
    </li>
  );
}

/** Chunk counts for a still-processing document climb in real time as the
 *  pipeline works through the file -- rendering that exactly like a
 *  finished count (plain tabular-nums, no context) reads as "done" when
 *  it's anything but, especially for a large file where it visibly ticks
 *  up every poll. A spinner + "so far" + a tooltip make clear the number
 *  is a live, in-progress tally, not the final chunk total; `aria-live`
 *  lets a screen reader pick up the same "still going" signal.
 *  `status` decides live vs. final, so callers never have to duplicate
 *  the COMPLETED/FAILED check themselves. */
function ChunksValue({
  chunks,
  status,
}: {
  chunks: number | null | undefined;
  status: string;
}) {
  const isLive = status !== KB_DOCUMENT_STATUS.COMPLETED && status !== KB_DOCUMENT_STATUS.FAILED;

  // Hooks must run on every render regardless of `status` -- this component
  // stays mounted (FileRow is memo'd, keyed by the stable doc id) across the
  // QUEUED -> PROCESSING -> COMPLETED transition, so an early return above
  // these would call a different number of hooks between renders and crash
  // React ("Rendered more hooks than during the previous render").
  //
  // Flipping straight from the amber "still counting" spinner to plain
  // gray text on the very next poll reads as if the number just
  // silently stopped -- nothing marks *this* as the count that stuck.
  // Track the live->final transition and hold a brief green "locked in"
  // flash so the final value visibly announces itself before settling.
  const wasLiveRef = useRef(isLive);
  const [justSettled, setJustSettled] = useState(false);
  useEffect(() => {
    if (wasLiveRef.current && !isLive) {
      setJustSettled(true);
      const timer = setTimeout(() => setJustSettled(false), 1800);
      wasLiveRef.current = isLive;
      return () => clearTimeout(timer);
    }
    wasLiveRef.current = isLive;
  }, [isLive]);

  // If the document is still in the queue, it hasn't even started chunking yet.
  // Showing "Counting..." is premature; a simple placeholder reads cleaner
  // until the worker actually picks it up.
  if (status === KB_DOCUMENT_STATUS.QUEUED) {
    return (
      <Tooltip>
        <TooltipTrigger asChild>
          <span className="text-muted-foreground/60 cursor-default">—</span>
        </TooltipTrigger>
        <TooltipContent side="top" className="max-w-[280px] text-center">
          Waiting for an indexing worker to start chunking this file.
        </TooltipContent>
      </Tooltip>
    );
  }

  if (!isLive) {
    return (
      <Tooltip>
        <TooltipTrigger asChild>
          <span
            className={cn(
              "inline-flex items-center gap-1 tabular-nums text-foreground/90 transition-colors duration-700 cursor-default",
              justSettled && "text-emerald-600 dark:text-emerald-400",
            )}
          >
            {justSettled && <Check className="h-3 w-3 shrink-0" aria-hidden />}
            {chunks == null ? "—" : chunks.toLocaleString()}
          </span>
        </TooltipTrigger>
        <TooltipContent side="top" className="max-w-[280px] text-center">
          Total chunks processed for this document across all segments.
        </TooltipContent>
      </Tooltip>
    );
  }
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span
          className="inline-flex items-center gap-1.5 cursor-default text-yellow-700 dark:text-yellow-300"
          aria-live="polite"
        >
          <Loader2 className="h-3 w-3 shrink-0 animate-spin" aria-hidden />
          <span className="tabular-nums">
            {chunks == null ? "Counting…" : chunks.toLocaleString()}
          </span>
          {chunks != null ? (
            <span className="text-[10px] font-normal text-yellow-700/70 dark:text-yellow-300/70">
              so far
            </span>
          ) : null}
        </span>
      </TooltipTrigger>
      <TooltipContent side="top" className="max-w-[280px] text-center">
        Still counting: this climbs live while the file is being chunked and isn't final until processing completes.
      </TooltipContent>
    </Tooltip>
  );
}

type FileRowProps = {
  doc: KnowledgeBaseDocument;
  embeddingModel: string | null;
  embeddingDimensions: number | null;
  chunkSize: number | null;
  datapackName: string;
  datapackId: string;
  callerUserId: string | null;
  datapackGrant: string | null;
  onDocumentDeleted: (documentId: string) => void;
  compact: boolean;
  showChunks: boolean;
  /** When set, the whole row becomes a plain navigation trigger instead
   *  of the inline expand/detail panel -- see the upload dialog, which
   *  wants clicking a row to jump to (and shimmer) that same file on the
   *  full Knowledge Base page rather than open a second copy of the
   *  detail panel inside an already-small dialog. */
  onRowClick?: (doc: KnowledgeBaseDocument) => void;
  /** Deep-link arrival target -- see FilesTable's `highlightDocumentId`.
   *  Scrolls into view and runs a one-time shimmer sweep. */
  highlight?: boolean;
  /** Fired after the shimmer finishes. */
  onHighlightConsumed?: (id: string) => void;
};

const FileRow = memo(function FileRow({
  doc,
  embeddingModel,
  embeddingDimensions,
  chunkSize,
  datapackName,
  datapackId,
  callerUserId,
  datapackGrant,
  onDocumentDeleted,
  compact,
  showChunks,
  onRowClick,
  highlight = false,
  onHighlightConsumed,
}: FileRowProps) {
  const [expanded, setExpanded] = useState(false);
  const toggle = useCallback(() => setExpanded((v) => !v), []);
  const now = useNowMs();

  const fullName = doc.document_name?.trim() || "—";
  const displayName = documentNameLastSegment(fullName).replace(/^\d+_/, "");
  const { Icon: FileTypeIcon, toneClass: iconTone } = fileTypeVisual(documentFileExtension(fullName));
  const chunks = doc.chunks_processed;
  // Prefer S3 upload time; fall back to pipeline completion for legacy docs.
  const uploadTimestamp = doc.uploaded_at ?? doc.completed_at;
  const uploadedRel = uploadTimestamp ? formatRelativeTime(uploadTimestamp, now) : "—";
  const rawChecksum = doc.document_checksum?.replace(/^"+|"+$/g, "") ?? "";
  const md5 = rawChecksum
    ? `${rawChecksum.slice(0, 8)}…${rawChecksum.slice(-4)}`
    : "—";

  // Compact rows without `showChunks` (the main datapack page's preview
  // tile) aren't expandable -- there's no room for the detail panel, and
  // the chevron/actions would just be dead weight pointing at a "View
  // all" link that already exists above. A row with `onRowClick` (the
  // upload dialog) never expands in place either, regardless of
  // `compact` -- it navigates instead (see the type comment above).
  const clickToNavigate = Boolean(onRowClick);
  const canExpand = !compact && !clickToNavigate;

  const { ref: rowRef, flashing: shimmer } = useArrivalFlash<HTMLLIElement>({
    flashKey: doc.id,
    active: highlight,
    onDone: onHighlightConsumed,
    debugLabel: `FileRow(${doc.id})`,
  });
  const [clicked, setClicked] = useState(false);

  const handleClick = useCallback(() => {
    if (!onRowClick) return;
    setClicked(true);
    // Tiny delay to let the click flash breathe before the page transition
    // starts. Standard navigation is instant; this adds a "physical" feel
    // to the click that matches the user's "flash and open" request.
    setTimeout(() => {
      onRowClick(doc);
      setClicked(false);
    }, 120);
  }, [onRowClick, doc]);

  return (
    <li
      ref={rowRef}
      className={cn(
        "relative overflow-hidden transition-shadow duration-300",
        shimmer && "z-10 rounded-lg ring-2 ring-emerald-500/60 dark:ring-emerald-400/60",
      )}
    >
      {/* One-time flash for a deep-linked arrival from the preview tile.
          A held emerald glow makes the row unmissable even if the sweep
          is missed mid-scroll; the sweep on top gives it the motion that
          reads as "this one". */}
      {shimmer ? (
        <>
          <span
            aria-hidden
            className="meko-file-row-glow pointer-events-none absolute inset-0 z-10"
          />
          <span
            aria-hidden
            className="meko-file-row-sweep pointer-events-none absolute inset-0 z-10"
          />
        </>
      ) : null}
      {/* Click feedback flash: brief highlight when the row is clicked. */}
      {clicked ? (
        <span
          aria-hidden
          className="absolute inset-0 z-10 animate-in fade-in bg-emerald-500/15 duration-100"
        />
      ) : null}
      {/* Header row — clicking the chevron expands the panel below (or,
          with `onRowClick`, clicking anywhere on the row navigates
          instead). Below md: each row stacks vertically (1 column) so
          each cell gets the full width — readable as a card. Desktop
          unchanged. */}
      <div
        className={cn(
          "grid grid-cols-1 gap-1 px-4 py-3 md:gap-3 md:px-5",
          rowGridClass(compact, showChunks),
          clickToNavigate && "cursor-pointer transition-colors hover:bg-accent/40",
          clicked && "bg-emerald-500/10",
        )}
        {...(clickToNavigate ? { onClick: handleClick } : {})}
      >
        {canExpand ? (
          <button
            type="button"
            onClick={toggle}
            aria-expanded={expanded}
            aria-label={expanded ? "Collapse file details" : "Expand file details"}
            className="flex h-5 w-5 items-center justify-center text-muted-foreground hover:text-foreground"
          >
            {expanded ? (
              <ChevronDown className="h-3.5 w-3.5" aria-hidden />
            ) : (
              <ChevronRight className="h-3.5 w-3.5" aria-hidden />
            )}
          </button>
        ) : (
          <span aria-hidden className="hidden md:block" />
        )}
        {canExpand ? (
          <button
            type="button"
            onClick={toggle}
            className="flex min-w-0 items-center gap-2 text-left"
            aria-label={expanded ? "Collapse file details" : "Expand file details"}
          >
            <FileTypeIcon className={cn("h-5 w-5 shrink-0", iconTone)} aria-hidden />
            <span className="truncate text-sm text-foreground" title={fullName}>
              {displayName}
            </span>
          </button>
        ) : (
          <span className="flex min-w-0 items-center gap-2">
            <FileTypeIcon className={cn("h-5 w-5 shrink-0", iconTone)} aria-hidden />
            <span className="truncate text-sm text-foreground" title={fullName}>
              {displayName}
            </span>
          </span>
        )}
        {compact ? (
          showChunks ? (
            <span className="self-center text-sm">
              <ChunksValue chunks={chunks} status={doc.status} />
            </span>
          ) : null
        ) : (
          <>
            <span className="self-center text-sm tabular-nums text-muted-foreground">
              {formatFileSize(doc.size_bytes)}
            </span>
            <span className="self-center text-sm">
              <ChunksValue chunks={chunks} status={doc.status} />
            </span>
            <span
              className="self-center truncate text-sm text-muted-foreground"
              title={uploadTimestamp ?? undefined}
            >
              {uploadedRel}
            </span>
            <code
              className="self-center font-mono text-xs text-muted-foreground/90"
              title={doc.document_checksum ?? undefined}
            >
              {md5}
            </code>
          </>
        )}
        <span className="self-center">
          <StatusBadge status={doc.status} />
        </span>
      </div>

      {canExpand && expanded ? (
        <FileExpandPanel
          doc={doc}
          embeddingModel={embeddingModel}
          embeddingDimensions={embeddingDimensions}
          chunkSize={chunkSize}
          datapackName={datapackName}
          datapackId={datapackId}
          callerUserId={callerUserId}
          datapackGrant={datapackGrant}
          onDocumentDeleted={onDocumentDeleted}
        />
      ) : null}
    </li>
  );
});

function FileExpandPanel({
  doc,
  embeddingModel,
  embeddingDimensions,
  chunkSize,
  datapackName,
  datapackId,
  callerUserId,
  datapackGrant,
  onDocumentDeleted,
}: {
  doc: KnowledgeBaseDocument;
  embeddingModel: string | null;
  embeddingDimensions: number | null;
  chunkSize: number | null;
  datapackName: string;
  datapackId: string;
  callerUserId: string | null;
  datapackGrant: string | null;
  onDocumentDeleted: (documentId: string) => void;
}) {
  // Two columns: auto-summary (left) + chunking/embedding metadata
  // (right). We skip any field we don't have data for — there's no
  // BE source for an auto file-summary today, so we show a small
  // placeholder hint instead of a stale "—". The metadata column
  // pulls everything from the document row directly.
  return (
    <div className="space-y-4 border-t border-border/40 bg-input/40 px-5 py-4">
      {/* ml-9 skips the chevron+gap area so the two sections align
          with the File and Size columns respectively. */}
      <div className="ml-9 grid grid-cols-1 gap-3 lg:grid-cols-[2fr_4fr]">
        <section>
          <header className="mb-2 flex items-center gap-1.5">
            <Sparkles className="h-3 w-3 text-emerald-700 dark:text-emerald-300" aria-hidden />
            <span className="text-[10px] font-semibold uppercase tracking-wider text-emerald-700 dark:text-emerald-200/95">
              Auto-summary
            </span>
          </header>
          {/* No per-file auto-summary endpoint today — placeholder
              keeps the panel visually balanced. Swap when the BE
              starts returning one. */}
          <p className="text-sm leading-relaxed text-muted-foreground">
            Per-file summary not available yet.
          </p>
        </section>

        <section>
          <header className="mb-2">
            <span className="text-[10px] font-semibold uppercase tracking-wider text-muted-foreground">
              Chunking &amp; embedding
            </span>
          </header>
          {/* Two-column metadata grid. 7 fields shown in row order:
                row 1: chunks       | strategy
                row 2: embed model  | vector dim
                row 3: md5          | size
                row 4: uploaded by  | (empty)
              `uploaded by` + `size` come from
              dist_rag.documents.document_details->meko at CreateKB
              time; legacy rows fall back to "—". */}
          <dl className="grid grid-cols-2 gap-x-4 gap-y-1.5 text-sm">
            <KvRow
              label="chunks"
              value={<ChunksValue chunks={doc.chunks_processed} status={doc.status} />}
            />
            <KvRow
              label="strategy"
              value={chunkSize != null ? `semantic·${chunkSize.toLocaleString()}` : "semantic"}
            />
            <KvRow label="embed model" value={embeddingModel ?? "—"} mono />
            <KvRow
              label="vector dim"
              value={embeddingDimensions != null ? embeddingDimensions.toLocaleString() : "—"}
            />
            <KvRow label="md5" value={doc.document_checksum ?? "—"} mono />
            <KvRow label="size" value={formatFileSize(doc.size_bytes)} />
            <KvRow label="by" value={doc.uploaded_by_email ?? "—"} />
          </dl>
        </section>
      </div>

      <FileRowActions
        doc={doc}
        datapackName={datapackName}
        datapackId={datapackId}
        callerUserId={callerUserId}
        datapackGrant={datapackGrant}
        onDocumentDeleted={onDocumentDeleted}
      />
    </div>
  );
}

/** Per-row action footer. Download fetches a short-lived presigned S3
 *  URL and opens it in a new tab. Remove-from-KB calls the DELETE
 *  endpoint; the button is only rendered when the caller is allowed
 *  to delete this file (datapack owner / maintainer, or the file's
 *  recorded uploader). The BE re-enforces the same rule. */
function FileRowActions({
  doc,
  datapackName,
  datapackId,
  callerUserId,
  datapackGrant,
  onDocumentDeleted,
}: {
  doc: KnowledgeBaseDocument;
  datapackName: string;
  datapackId: string;
  callerUserId: string | null;
  datapackGrant: string | null;
  onDocumentDeleted: (documentId: string) => void;
}) {
  const canDelete = canDeleteKBDocumentClient(
    datapackGrant,
    callerUserId,
    doc.uploaded_by_user_id,
  );
  // Shown-but-disabled rather than hidden while indexing: hiding it looks
  // identical to "you don't have permission to delete this", and the row
  // is polling, so the button comes alive on its own once the file lands
  // in a terminal status.
  const inFlight = isKbDocumentInFlight(doc.status);
  return (
    <div className="flex items-center gap-3">
      <DownloadAction doc={doc} datapackName={datapackName} datapackId={datapackId} />
      {canDelete ? (
        <RemoveFromKBAction
          doc={doc}
          datapackName={datapackName}
          datapackId={datapackId}
          onDocumentDeleted={onDocumentDeleted}
          disabledReason={
            inFlight
              ? `Still ${kbDocumentStatusLabel(doc.status).toLowerCase()} — a file can only be removed once indexing finishes.`
              : null
          }
        />
      ) : null}
    </div>
  );
}

function RemoveFromKBAction({
  doc,
  datapackName,
  datapackId,
  onDocumentDeleted,
  disabledReason,
}: {
  doc: KnowledgeBaseDocument;
  datapackName: string;
  datapackId: string;
  onDocumentDeleted: (documentId: string) => void;
  /** Non-null blocks the action and is shown as the tooltip explaining why. */
  disabledReason: string | null;
}) {
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const fileLabel = documentNameLastSegment(doc.document_name) || doc.document_name;

  const handleConfirm = useCallback(async () => {
    setBusy(true);
    setError(null);
    try {
      await deleteKnowledgeBaseDocument(datapackId, doc.id);
      setOpen(false);
      onDocumentDeleted(doc.id);
      toast({
        title: "File removed",
        description: `"${documentNameForMessage(fileLabel)}" was deleted from the knowledge base.`,
      });
    } catch (err) {
      setError(getApiErrorMessage(err, "Could not delete this file."));
    } finally {
      setBusy(false);
    }
  }, [doc.id, doc.document_name, datapackName, datapackId, onDocumentDeleted, fileLabel]);

  const trigger = (
    <button
      type="button"
      disabled={Boolean(disabledReason)}
      onClick={() => { setError(null); setOpen(true); }}
      className={cn(
        "inline-flex items-center gap-1.5 text-xs font-medium text-red-700 dark:text-red-300",
        disabledReason
          ? "cursor-not-allowed opacity-50"
          : "hover:text-red-700 dark:hover:text-red-200",
      )}
    >
      <Trash2 className="h-3.5 w-3.5" aria-hidden />
      Remove from KB
    </button>
  );

  return (
    <>
      {disabledReason ? (
        <Tooltip>
          {/* A disabled button fires no pointer events, so the tooltip
              would never open on the element itself -- wrap it in a span
              that can receive the hover. */}
          <TooltipTrigger asChild>
            <span className="inline-flex">{trigger}</span>
          </TooltipTrigger>
          <TooltipContent>{disabledReason}</TooltipContent>
        </Tooltip>
      ) : (
        trigger
      )}

      <Dialog open={open} onOpenChange={(v) => { if (!busy) setOpen(v); }}>
        <DialogContent className="w-[calc(100vw-2rem)] sm:max-w-md bg-card border-border text-foreground">
          <DialogHeader>
            <DialogTitle className="text-xl text-foreground">Remove file</DialogTitle>
            <DialogDescription className="text-muted-foreground">
              This removes the file and its chunks from the knowledge base. It cannot be undone.
            </DialogDescription>
          </DialogHeader>
          {/* On its own line rather than inline in the sentence above:
              knowledge base filenames are routinely long enough (release
              names, hashed exports) to wrap over several lines and break
              mid-word, which buries the sentence and makes the dialog
              grow unpredictably. Truncated to one line, full name on
              hover. */}
          <p
            className="truncate rounded-md border border-border bg-muted/30 px-3 py-2 text-sm font-medium text-foreground"
            title={fileLabel}
          >
            {fileLabel}
          </p>
          {error ? (
            <p className="text-sm text-red-700 dark:text-red-300" role="alert">{error}</p>
          ) : null}
          <DialogFooter className="gap-2 sm:gap-0">
            <Button
              type="button"
              variant="outline"
              onClick={() => setOpen(false)}
              disabled={busy}
            >
              Cancel
            </Button>
            <Button
              type="button"
              variant="destructive"
              onClick={() => void handleConfirm()}
              disabled={busy}
            >
              {busy ? "Removing…" : "Remove"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  );
}

/** Download → fetch presigned S3 URL → open in new tab. Button stays
 *  disabled with a spinner while the request is in flight so quick
 *  double-clicks don't fan out into duplicate signs. Errors toast and
 *  log; the button re-enables. */
function DownloadAction({
  doc,
  datapackName,
  datapackId,
}: {
  doc: KnowledgeBaseDocument;
  datapackName: string;
  datapackId: string;
}) {
  const [busy, setBusy] = useState(false);
  const disabled = busy || !doc.document_uri;

  const handleClick = useCallback(async () => {
    if (!doc.document_uri || busy) return;
    setBusy(true);
    try {
      const resp = await requestKnowledgeBaseDownloadURL(datapackId, doc.document_uri);
      // Open in a new tab. `noopener` keeps the new context isolated;
      // S3 issues a 302 to the actual file blob so the browser handles
      // the download natively (preserves filename via response headers).
      window.open(resp.url, "_blank", "noopener,noreferrer");
    } catch (err) {
      toast({
        title: "Download failed",
        description: getApiErrorMessage(err, "Could not generate download URL."),
      });
    } finally {
      setBusy(false);
    }
  }, [doc.document_uri, datapackName, datapackId, busy]);

  return (
    <button
      type="button"
      disabled={disabled}
      onClick={() => void handleClick()}
      className="inline-flex items-center gap-1.5 text-xs font-medium text-brand hover:text-brand/80 disabled:cursor-not-allowed disabled:opacity-60"
    >
      <Download className="h-3.5 w-3.5" aria-hidden />
      {busy ? "Preparing…" : "Download"}
    </button>
  );
}

function KvRow({
  label,
  value,
  mono = false,
}: {
  label: string;
  value: ReactNode;
  mono?: boolean;
}) {
  return (
    <div className="flex flex-wrap items-baseline gap-2">
      <dt className="text-[11px] uppercase tracking-wider text-muted-foreground">{label}:</dt>
      <dd
        className={cn(
          "min-w-0 flex-1 truncate",
          mono ? "font-mono text-xs text-foreground/90" : "text-foreground/90",
        )}
        title={typeof value === "string" ? value : undefined}
      >
        {value}
      </dd>
    </div>
  );
}
