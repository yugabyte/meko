// "Files" tab on the SharedKnowledge summary page.
//
// Three stacked sections inside one card-style container:
//   1. FILES SUMMARY · AUTO + "Upload file" CTA. Heaviest source
//      wrapped to its own line for readability.
//   2. Drag-and-drop upload area (reuses the existing `FileUpload`
//      orchestrator so the upload pipeline + KB-create flow stay
//      shared with the modal version).
//   3. Filter row (file-type chips + "Indexed only" toggle) + rows.
//      Each row is collapsible: click the chevron to reveal the
//      auto-summary, chunking/embedding metadata, and per-file
//      actions (Remove from KB, Download).
//
// Search is owned by the parent page and passed in as `searchQuery`
// so the same input filters across both tabs.

import { memo, useCallback, useEffect, useRef, useState } from "react";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  ChevronDown,
  ChevronLeft,
  ChevronRight,
  Download,
  File as FileIcon,
  Loader2,
  Sparkles,
  Trash2,
  Upload,
  X,
} from "lucide-react";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { formatRelativeTime, useNowMs } from "@/lib/relativeTime";
import FileUpload from "@/features/datapacks/components/FileUpload";
import {
  deleteKnowledgeBaseDocument,
  getApiErrorMessage,
  getKnowledgeBaseDocuments,
  requestKnowledgeBaseDownloadURL,
  type KnowledgeBaseDocument,
} from "@/lib/api/mekoApi";
import { toast } from "@/hooks/use-toast";
import { documentNameLastSegment } from "./knowledgeDocumentName";
import {
  KB_DOCUMENT_STATUS,
  kbDocumentStatusTone,
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

function formatBytes(n: number | null | undefined): string {
  if (n == null) return "—";
  if (n < 1024) return `${n} B`;
  const k = n / 1024;
  if (k < 1024) return `${k.toFixed(2)} KB`;
  const m = k / 1024;
  if (m < 1024) return `${m.toFixed(2)} MB`;
  const g = m / 1024;
  return `${g.toFixed(2)} GB`;
}

/** Per-extension icon tone for the file row. Color alone signals the
 *  type at a glance — same lucide glyph, different `text-*-300` shade.
 *  Unknown / unset types fall through to emerald (the section's accent). */
function fileIconToneClass(documentType?: string | null): string {
  switch ((documentType || "").toLowerCase()) {
    case ".pdf":
      return "text-red-700 dark:text-red-300";
    case ".md":
      return "text-blue-700 dark:text-blue-300";
    case ".txt":
      return "text-sky-700 dark:text-sky-300";
    case ".json":
    case ".yaml":
    case ".yml":
      return "text-amber-700 dark:text-amber-300";
    default:
      return "text-emerald-700 dark:text-emerald-300";
  }
}

/** File-type chip values. "all" is the default (no filter); the rest map
 *  to {@link KnowledgeBaseDocument.document_type} suffixes. */
const FILE_TYPE_FILTERS = [
  { id: "all", label: "All types" },
  { id: ".md", label: ".md" },
  { id: ".pdf", label: ".pdf" },
  { id: ".txt", label: ".txt" },
  { id: ".json", label: ".json" },
] as const;
type FileTypeFilter = (typeof FILE_TYPE_FILTERS)[number]["id"];

const STATUS_TONE: Record<string, string> = {
  muted: "border-border/60 bg-muted/30 text-muted-foreground",
  blue: "border-blue-500/35 bg-blue-500/10 text-blue-700 dark:text-blue-200",
  green: "border-emerald-500/35 bg-emerald-500/10 text-emerald-700 dark:text-emerald-200",
  red: "border-red-500/35 bg-red-500/10 text-red-700 dark:text-red-200",
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
}: Props) {
  const [typeFilter, setTypeFilter] = useState<FileTypeFilter>("all");
  const [indexedOnly, setIndexedOnly] = useState(false);
  const [uploadOpen, setUploadOpen] = useState(false);
  const [page, setPage] = useState(1);

  // Owned fetch state — this component fetches its own page of documents
  // so the parent doesn't have to hold the full list in memory.
  const [documents, setDocuments] = useState<KnowledgeBaseDocument[]>([]);
  const [total, setTotal] = useState(0);
  const [hasMore, setHasMore] = useState(false);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  // When the parent's searchQuery changes, reset to page 1 before the
  // fetch effect fires. Updating state during render (the React 18
  // "getDerivedStateFromProps" pattern) is safe here: React discards
  // the current render and immediately re-renders with page=1, so the
  // effect always sees the correct page alongside the new query.
  const prevSearchRef = useRef(searchQuery);
  if (prevSearchRef.current !== searchQuery) {
    prevSearchRef.current = searchQuery;
    if (page !== 1) setPage(1);
  }

  useEffect(() => {
    const ac = new AbortController();
    setLoading(true);
    setError(null);
    void getKnowledgeBaseDocuments(datapackName, {
      page,
      page_size: PAGE_SIZE,
      q: searchQuery.trim() || undefined,
      type_filter: typeFilter !== "all" ? typeFilter : undefined,
      indexed_only: indexedOnly || undefined,
      datapackId,
      signal: ac.signal,
    })
      .then((resp) => {
        if (ac.signal.aborted) return;
        setDocuments(resp.items);
        setTotal(resp.total);
        setHasMore(resp.has_more);
      })
      .catch((e) => {
        if (ac.signal.aborted) return;
        if ((e as { name?: string })?.name === "CanceledError") return;
        setDocuments([]);
        setTotal(0);
        setHasMore(false);
        setError(getApiErrorMessage(e, "Could not load knowledge documents."));
      })
      .finally(() => {
        if (!ac.signal.aborted) setLoading(false);
      });
    return () => ac.abort();
  }, [datapackName, datapackId, page, searchQuery, typeFilter, indexedOnly, refreshKey]);

  // Reset to page 1 when type filter or indexed-only toggle changes.
  // Called directly in the event handler so React 18 batches both
  // state updates into one re-render.
  const handleTypeFilterChange = (next: FileTypeFilter) => {
    setTypeFilter(next);
    setPage(1);
  };
  const handleIndexedOnlyChange = (next: boolean) => {
    setIndexedOnly(next);
    setPage(1);
  };

  const totalPages = Math.max(1, Math.ceil(total / PAGE_SIZE));
  const hasActiveFilter = searchQuery.trim() !== "" || typeFilter !== "all" || indexedOnly;

  return (
    <div className="space-y-6">
      <SummaryCard
        filesSummary={filesSummary}
        heaviestSourceName={heaviestSourceName}
        onUploadClick={() => setUploadOpen((v) => !v)}
        uploadOpen={uploadOpen}
      />

      {uploadOpen ? (
        <section className="rounded-xl border border-border bg-card p-6">
          <FileUpload
            datapackName={datapackName}
            datapackId={datapackId}
            onKnowledgeBaseCreated={onKnowledgeBaseCreated}
          />
        </section>
      ) : null}

      <FilterRow
        typeFilter={typeFilter}
        onTypeFilterChange={handleTypeFilterChange}
        indexedOnly={indexedOnly}
        onIndexedOnlyChange={handleIndexedOnlyChange}
      />

      <FilesTable
        documents={documents}
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
            <Sparkles className="h-3.5 w-3.5 text-purple-700 dark:text-purple-300" aria-hidden />
            <span className="text-[10px] font-semibold uppercase tracking-wider text-purple-700 dark:text-purple-200/95">
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
        ) : (
          <Button
            type="button"
            onClick={onUploadClick}
            className="shrink-0 gap-1.5 bg-blue-600 text-white hover:bg-blue-700"
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

function FilterRow({
  typeFilter,
  onTypeFilterChange,
  indexedOnly,
  onIndexedOnlyChange,
}: {
  typeFilter: FileTypeFilter;
  onTypeFilterChange: (next: FileTypeFilter) => void;
  indexedOnly: boolean;
  onIndexedOnlyChange: (next: boolean) => void;
}) {
  return (
    <div className="flex flex-wrap items-center gap-2">
      <span className="text-xs text-muted-foreground">Filter:</span>
      {FILE_TYPE_FILTERS.map((f) => {
        const active = typeFilter === f.id;
        return (
          <button
            key={f.id}
            type="button"
            onClick={() => onTypeFilterChange(f.id)}
            className={cn(
              "rounded-md border px-2.5 py-1 text-xs font-medium transition-colors",
              active
                ? "border-blue-500/50 bg-blue-500/15 text-blue-700 dark:text-blue-200"
                : "border-border bg-input text-muted-foreground hover:text-foreground",
            )}
          >
            {f.label}
          </button>
        );
      })}
      <span className="ml-1 text-muted-foreground/60">·</span>
      <button
        type="button"
        onClick={() => onIndexedOnlyChange(!indexedOnly)}
        className={cn(
          "rounded-md border px-2.5 py-1 text-xs font-medium transition-colors",
          indexedOnly
            ? "border-emerald-500/50 bg-emerald-500/15 text-emerald-700 dark:text-emerald-200"
            : "border-border bg-input text-muted-foreground hover:text-foreground",
        )}
        aria-pressed={indexedOnly}
      >
        Indexed only
      </button>
    </div>
  );
}

function FilesTable({
  documents,
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
}: {
  documents: KnowledgeBaseDocument[];
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
  if (loading && documents.length === 0) {
    return (
      <div className="rounded-xl border border-dashed border-border bg-muted/5 px-6 py-12 text-center text-sm text-muted-foreground">
        Loading documents…
      </div>
    );
  }
  if (documents.length === 0) {
    return (
      <div className="rounded-xl border border-dashed border-border bg-muted/5 px-6 py-12 text-center text-sm text-muted-foreground">
        {total === 0 && !hasActiveFilter
          ? "No knowledge documents yet. Upload files to see them here."
          : "No files match the current search or filter."}
      </div>
    );
  }

  return (
    <section className="rounded-xl border border-border bg-card">
      <header className="grid grid-cols-[24px_minmax(0,2fr)_minmax(0,0.7fr)_minmax(0,0.6fr)_minmax(0,0.9fr)_minmax(0,1fr)_minmax(0,0.7fr)] gap-3 border-b border-border/60 px-5 py-3 text-[10px] font-semibold uppercase tracking-wider text-muted-foreground">
        <span aria-hidden />
        <span>File</span>
        <span>Size</span>
        <span>Chunks</span>
        <span>Uploaded</span>
        <span className="font-mono">MD5</span>
        <span>Status</span>
      </header>
      <ul className="divide-y divide-border/40">
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
          />
        ))}
      </ul>
    </section>
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
}: FileRowProps) {
  const [expanded, setExpanded] = useState(false);
  const toggle = useCallback(() => setExpanded((v) => !v), []);
  const now = useNowMs();

  const tone = kbDocumentStatusTone(doc.status);
  const fullName = doc.document_name?.trim() || "—";
  const displayName = documentNameLastSegment(fullName).replace(/^\d+_/, "");
  const chunks = doc.chunks_processed;
  // Prefer S3 upload time; fall back to pipeline completion for legacy docs.
  const uploadTimestamp = doc.uploaded_at ?? doc.completed_at;
  const uploadedRel = uploadTimestamp ? formatRelativeTime(uploadTimestamp, now) : "—";
  const rawChecksum = doc.document_checksum?.replace(/^"+|"+$/g, "") ?? "";
  const md5 = rawChecksum
    ? `${rawChecksum.slice(0, 8)}…${rawChecksum.slice(-4)}`
    : "—";

  return (
    <li>
      {/* Header row — clicking the chevron expands the panel below.
          Grid is a 6-cell row: chevron · file · chunks · uploaded ·
          md5 · status. The chevron column keeps a fixed 24px width so
          the file-name column aligns regardless of expand state. */}
      <div className="grid grid-cols-[24px_minmax(0,2fr)_minmax(0,0.7fr)_minmax(0,0.6fr)_minmax(0,0.9fr)_minmax(0,1fr)_minmax(0,0.7fr)] gap-3 px-5 py-3">
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
        <button
          type="button"
          onClick={toggle}
          className="flex min-w-0 items-center gap-2 text-left"
          aria-label={expanded ? "Collapse file details" : "Expand file details"}
        >
          <FileIcon
            className={cn("h-3.5 w-3.5 shrink-0", fileIconToneClass(doc.document_type))}
            aria-hidden
          />
          <span className="truncate text-sm text-foreground" title={fullName}>
            {displayName}
          </span>
        </button>
        <span className="self-center text-sm tabular-nums text-muted-foreground">
          {formatBytes(doc.size_bytes)}
        </span>
        <span className="self-center text-sm tabular-nums text-foreground/90">
          {chunks == null ? "—" : chunks.toLocaleString()}
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
        <span className="self-center">
          <span
            className={cn(
              "inline-flex items-center rounded-full border px-2 py-0.5 text-[10px] font-semibold uppercase tracking-wide",
              STATUS_TONE[tone],
            )}
          >
            {statusLabel(doc.status)}
          </span>
        </span>
      </div>

      {expanded ? (
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
            <Sparkles className="h-3 w-3 text-purple-700 dark:text-purple-300" aria-hidden />
            <span className="text-[10px] font-semibold uppercase tracking-wider text-purple-700 dark:text-purple-200/95">
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
              value={doc.chunks_processed != null ? doc.chunks_processed.toLocaleString() : "—"}
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
            <KvRow label="size" value={formatBytes(doc.size_bytes)} />
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
  return (
    <div className="flex items-center gap-3">
      <DownloadAction doc={doc} datapackName={datapackName} datapackId={datapackId} />
      {canDelete ? (
        <RemoveFromKBAction
          doc={doc}
          datapackName={datapackName}
          datapackId={datapackId}
          onDocumentDeleted={onDocumentDeleted}
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
}: {
  doc: KnowledgeBaseDocument;
  datapackName: string;
  datapackId: string;
  onDocumentDeleted: (documentId: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const fileLabel = documentNameLastSegment(doc.document_name) || doc.document_name;

  const handleConfirm = useCallback(async () => {
    setBusy(true);
    setError(null);
    try {
      await deleteKnowledgeBaseDocument(datapackName, doc.id, { datapackId });
      setOpen(false);
      onDocumentDeleted(doc.id);
      toast({
        title: "File removed",
        description: `"${fileLabel}" was deleted from the knowledge base.`,
      });
    } catch (err) {
      setError(getApiErrorMessage(err, "Could not delete this file."));
    } finally {
      setBusy(false);
    }
  }, [doc.id, doc.document_name, datapackName, datapackId, onDocumentDeleted, fileLabel]);

  return (
    <>
      <button
        type="button"
        onClick={() => { setError(null); setOpen(true); }}
        className="inline-flex items-center gap-1.5 text-xs font-medium text-red-700 dark:text-red-300 hover:text-red-700 dark:text-red-200"
      >
        <Trash2 className="h-3.5 w-3.5" aria-hidden />
        Remove from KB
      </button>

      <Dialog open={open} onOpenChange={(v) => { if (!busy) setOpen(v); }}>
        <DialogContent className="w-[calc(100vw-2rem)] sm:max-w-md bg-card border-border text-foreground">
          <DialogHeader>
            <DialogTitle className="text-xl text-foreground">Remove file</DialogTitle>
            <DialogDescription className="text-muted-foreground">
              Are you sure you want to remove{" "}
              <span className="font-medium text-foreground">{fileLabel}</span> from the knowledge
              base? This removes the file and its chunks. It cannot
              be undone.
            </DialogDescription>
          </DialogHeader>
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
      const resp = await requestKnowledgeBaseDownloadURL(
        datapackName,
        doc.document_uri,
        { datapackId },
      );
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
      className="inline-flex items-center gap-1.5 text-xs font-medium text-blue-700 dark:text-blue-300 hover:text-blue-700 dark:text-blue-200 disabled:cursor-not-allowed disabled:opacity-60"
    >
      <Download className="h-3.5 w-3.5" aria-hidden />
      {busy ? "Preparing…" : "Download"}
    </button>
  );
}

/** Prev / Next pagination footer below the files table. */
function PaginationRow({
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

      <span className="text-xs text-muted-foreground tabular-nums">
        {loading ? (
          <Loader2 className="inline h-3 w-3 animate-spin" aria-hidden />
        ) : (
          <>
            Page <span className="font-medium text-foreground">{page}</span> of{" "}
            <span className="font-medium text-foreground">{totalPages}</span>
          </>
        )}
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

function KvRow({
  label,
  value,
  mono = false,
}: {
  label: string;
  value: string;
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
        title={value}
      >
        {value}
      </dd>
    </div>
  );
}
