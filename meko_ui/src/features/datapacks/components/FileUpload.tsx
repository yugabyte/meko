import { useCallback, useEffect, useRef, useState } from "react";
import { Upload, X, FileText, CheckCircle2, AlertCircle, Loader2, Database } from "lucide-react";
import { Button } from "@/components/ui/button";
import { useCurrentUser } from "@/hooks/useCurrentUser";
import { cn } from "@/lib/utils";
import {
  getApiErrorMessage,
  requestUploadURL,
  confirmUpload,
  createKnowledgeBase,
  getKBStatus,
  sha256Hex,
  type PipelineDetail,
  type SourceStatus,
} from "@/lib/api/mekoApi";
import {
  KNOWLEDGE_UPLOAD_ACCEPTED_EXTENSIONS,
  KNOWLEDGE_UPLOAD_MAX_BATCH,
  KNOWLEDGE_UPLOAD_MAX_FILE_SIZE_MB,
  filterFilesForKnowledgeUploadBatch,
} from "@/lib/knowledgeFileUploadRules";
import { formatFileSize } from "@/lib/fileSize";
import { uploadFileToS3 } from "@/lib/uploadFileToS3";
import { KNOWLEDGE_UPLOAD_STATUS, type KnowledgeUploadStatus } from "@/lib/knowledgeUploadStatus";
import { queryClient } from "@/lib/queryClient";
import { ACCOUNT_USAGE_QUERY_KEY } from "@/hooks/useAccountUsage";
import { TELEMETRY_EVENTS } from "@/lib/telemetry/events";
import { trackEvent } from "@/lib/telemetry/rudderstack";
import { bytesToKb, fileTypeFromName } from "@/lib/telemetry/helpers";

type FileEntry = {
  file: File;
  status: KnowledgeUploadStatus;
  progress: number;
  error?: string;
  /** Set when the upload was skipped because identical content already existed. */
  note?: string;
};

/** Message shown when a content-hash dedup skips the actual upload. */
function dedupNote(uploadedName: string, existingName?: string): string {
  if (existingName && existingName !== uploadedName) {
    return `Identical content already in this knowledge base as "${existingName}" — skipped re-upload.`;
  }
  return "Already in this knowledge base (identical content) — skipped re-upload.";
}

function pipelineDocumentDisplayName(documentName: string): string {
  const filename = documentName.split("/").pop() ?? documentName;
  return filename.replace(/^\d+_/, "");
}

type Props = {
  datapackName: string;
  datapackId: string;
  /** Optional pre-staged files (e.g. a sample seed). Captured on first mount only. */
  initialFiles?: File[];
  /** Called after createKnowledgeBase succeeds so the parent can refresh its lists. */
  onKnowledgeBaseCreated?: () => void;
};

export default function FileUpload({
  datapackName,
  datapackId,
  initialFiles,
  onKnowledgeBaseCreated,
}: Props) {
  const { data: currentUser } = useCurrentUser();
  const [files, setFiles] = useState<FileEntry[]>(() =>
    (initialFiles ?? []).map((file) => ({
      file,
      status: KNOWLEDGE_UPLOAD_STATUS.PENDING,
      progress: 0,
    })),
  );
  const [isDragOver, setIsDragOver] = useState(false);
  const [warning, setWarning] = useState<string | null>(null);
  const [totalUploaded, setTotalUploaded] = useState(0);
  const inputRef = useRef<HTMLInputElement>(null);
  const warningTimer = useRef<ReturnType<typeof setTimeout>>(undefined);

  const [kbCreated, setKbCreated] = useState(false);
  const [creatingKB, setCreatingKB] = useState(false);
  const [pollingActive, setPollingActive] = useState(false);
  const [sourceStatuses, setSourceStatuses] = useState<SourceStatus[]>([]);
  const [pipelineDetails, setPipelineDetails] = useState<PipelineDetail[]>([]);
  const [queuedDocNames, setQueuedDocNames] = useState<string[]>([]);
  const pollRef = useRef<ReturnType<typeof setInterval>>(undefined);
  // Armed (true) when the user just triggered `createKnowledgeBase` so
  // we can fire `onKnowledgeBaseCreated` ONCE — and only after polling
  // confirms the new docs are actually indexed (all `pipeline_details`
  // entries reach COMPLETED/FAILED). Without this guard the callback
  // would fire when the POST returned, before the BE finished
  // processing, so a downstream `/summary` refetch would still see the
  // old counts. Also keeps the callback from re-firing when the dialog
  // is reopened against an already-done KB (fetchStatus runs on mount,
  // but the ref is false until a fresh Create-KB action arms it).
  const pendingKBReadyNotificationRef = useRef(false);
  const [newUploadsAfterKB, setNewUploadsAfterKB] = useState(0);
  const pollGraceRef = useRef(0);
  // Names + byte sizes of the files in the batch currently being processed, so
  // the document_uploaded telemetry (fired once processing completes) can report
  // file_size and emit only for THIS batch's docs (pipeline_details also includes
  // previously-indexed docs once a KB exists).
  const justUploadedRef = useRef<{ names: Set<string>; sizes: Map<string, number> }>({
    names: new Set(),
    sizes: new Map(),
  });

  const showWarning = useCallback((msg: string) => {
    setWarning(msg);
    clearTimeout(warningTimer.current);
    warningTimer.current = setTimeout(() => setWarning(null), 4000);
  }, []);

  const pendingCount = files.filter((f) => f.status === KNOWLEDGE_UPLOAD_STATUS.PENDING).length;
  const isUploading = files.some((f) => f.status === KNOWLEDGE_UPLOAD_STATUS.UPLOADING);
  const batchUsed = files.filter(
    (f) =>
      f.status === KNOWLEDGE_UPLOAD_STATUS.PENDING ||
      f.status === KNOWLEDGE_UPLOAD_STATUS.UPLOADING,
  ).length;
  const batchRemaining = KNOWLEDGE_UPLOAD_MAX_BATCH - batchUsed;
  const batchFull = batchRemaining <= 0;

  const addFiles = useCallback(
    (incoming: FileList | File[]) => {
      const all = Array.from(incoming);
      setFiles((prev) => {
        // No filename/metadata dedupe here: same-name, different-content files
        // must reach the upload flow. True duplicates are caught server-side by
        // content hash, which reports already-uploaded back to the UI.
        const currentBatch = prev.filter(
          (f) =>
            f.status === KNOWLEDGE_UPLOAD_STATUS.PENDING ||
            f.status === KNOWLEDGE_UPLOAD_STATUS.UPLOADING,
        ).length;
        const { accepted, warningParts } = filterFilesForKnowledgeUploadBatch(all, currentBatch);
        if (warningParts.length > 0) showWarning(warningParts.join(". "));
        if (accepted.length === 0) return prev;
        return [
          ...prev,
          ...accepted.map((f) => ({
            file: f,
            status: KNOWLEDGE_UPLOAD_STATUS.PENDING,
            progress: 0,
          })),
        ];
      });
    },
    [showWarning],
  );

  const removeFile = useCallback((index: number) => {
    setFiles((prev) => prev.filter((_, i) => i !== index));
  }, []);

  const handleDrop = useCallback(
    (e: React.DragEvent) => {
      e.preventDefault();
      setIsDragOver(false);
      if (e.dataTransfer.files.length > 0) {
        addFiles(e.dataTransfer.files);
      }
    },
    [addFiles],
  );

  const uploadFile = (url: string, file: File, index: number): Promise<void> =>
    uploadFileToS3(url, file, (pct) =>
      setFiles((prev) => prev.map((f, idx) => (idx === index ? { ...f, progress: pct } : f))),
    );

  const handleUpload = async () => {
    if (files.length === 0) return;

    let uploaded = 0;

    for (let i = 0; i < files.length; i++) {
      const entry = files[i];
      if (entry.status !== KNOWLEDGE_UPLOAD_STATUS.PENDING) continue;

      setFiles((prev) =>
        prev.map((f, idx) =>
          idx === i ? { ...f, status: KNOWLEDGE_UPLOAD_STATUS.UPLOADING, progress: 0 } : f,
        ),
      );

      try {
        const contentHash = await sha256Hex(entry.file);
        const { url, key, already_uploaded, existing_filename } = await requestUploadURL(
          datapackId,
          entry.file.name,
          entry.file.type || "application/octet-stream",
          entry.file.size,
          contentHash,
        );

        if (already_uploaded) {
          // Identical content already exists server-side: nothing is uploaded
          // or added. Mark it skipped (not "uploaded") and let the user remove
          // the row to continue with other files.
          const note = dedupNote(entry.file.name, existing_filename);
          showWarning(note);
          setFiles((prev) =>
            prev.map((f, idx) =>
              idx === i ? { ...f, status: KNOWLEDGE_UPLOAD_STATUS.SKIPPED, progress: 0, note } : f,
            ),
          );
          continue;
        }

        await uploadFile(url, entry.file, i);
        await confirmUpload(datapackId, key, entry.file.name, entry.file.size);
        // Bumps the KB storage counter the gating logic reads. Invalidate
        // here (post-confirm) rather than after each successful S3 PUT —
        // confirm is what marks the bytes as "yours" on the BE.
        void queryClient.invalidateQueries({ queryKey: [ACCOUNT_USAGE_QUERY_KEY] });

        uploaded++;
        setFiles((prev) =>
          prev.map((f, idx) =>
            idx === i ? { ...f, status: KNOWLEDGE_UPLOAD_STATUS.DONE, progress: 100 } : f,
          ),
        );
      } catch (err) {
        setFiles((prev) =>
          prev.map((f, idx) =>
            idx === i
              ? {
                  ...f,
                  status: KNOWLEDGE_UPLOAD_STATUS.ERROR,
                  progress: 0,
                  error: getApiErrorMessage(err, "Upload failed."),
                }
              : f,
          ),
        );
      }
    }

    if (uploaded > 0) {
      setTotalUploaded((prev) => prev + uploaded);
      if (kbCreated) setNewUploadsAfterKB((prev) => prev + uploaded);
    }
  };

  const handleCreateKB = async () => {
    const queuedEntries = files.filter((f) => f.status === KNOWLEDGE_UPLOAD_STATUS.DONE);
    const queuedNames = queuedEntries.map((f) => f.file.name);
    // Snapshot this batch's names + sizes for the document_uploaded telemetry.
    justUploadedRef.current = {
      names: new Set(queuedNames),
      sizes: new Map(queuedEntries.map((f) => [f.file.name, f.file.size])),
    };
    const previousKbCreated = kbCreated;

    // Flip UI state immediately so users see progress feedback right away.
    setQueuedDocNames(queuedNames);
    setKbCreated(true);
    setPollingActive(true);
    setNewUploadsAfterKB(0);
    pollGraceRef.current = 3;
    setCreatingKB(true);
    try {
      await createKnowledgeBase(datapackId, {});
      // Arm the post-poll notification. The actual callback fires later
      // inside fetchStatus, once all docs reach a terminal status —
      // firing here would be too early (the BE has only accepted the
      // create, indexing is still in flight, so a /summary refetch
      // would still see the old counts).
      pendingKBReadyNotificationRef.current = true;
      // Prime status once immediately instead of waiting for the next poll tick.
      await fetchStatus();
    } catch (err) {
      setKbCreated(previousKbCreated);
      if (!previousKbCreated) setPollingActive(false);
      setQueuedDocNames([]);
      showWarning(getApiErrorMessage(err, "Failed to create knowledge base."));
    } finally {
      setCreatingKB(false);
    }
  };

  // Fire document_uploaded once per successfully-processed doc in the current
  // batch. pipeline_details can also list previously-indexed docs, so we filter
  // to this batch's names and clear the snapshot to avoid re-emitting.
  const emitDocumentUploadedEvents = useCallback(
    (details: PipelineDetail[]) => {
      const batch = justUploadedRef.current;
      if (batch.names.size === 0) return;
      const batchDetails = details.filter((d) =>
        batch.names.has(pipelineDocumentDisplayName(d.document_name)),
      );
      const totalDocs = details.length;
      // First docs in the datapack when nothing pre-existed this batch.
      const isFirstBatch = totalDocs === batchDetails.length;
      batchDetails
        .filter((d) => d.document_status === "COMPLETED")
        .forEach((d) => {
          const name = pipelineDocumentDisplayName(d.document_name);
          const sizeBytes = batch.sizes.get(name);
          trackEvent(TELEMETRY_EVENTS.DOCUMENT_UPLOADED, {
            email: currentUser?.email,
            datapack_id: datapackId,
            document_id: name,
            file_type: fileTypeFromName(name),
            file_size_kb: sizeBytes !== undefined ? bytesToKb(sizeBytes) : undefined,
            is_first: isFirstBatch,
            total_docs: totalDocs,
          });
        });
      justUploadedRef.current = { names: new Set(), sizes: new Map() };
    },
    [datapackId, currentUser?.email],
  );

  const fetchStatus = useCallback(async () => {
    try {
      const status = await getKBStatus(datapackId, {});
      const sources = status.source_statuses ?? [];
      const details = status.pipeline_details ?? [];
      setSourceStatuses(sources);
      setPipelineDetails(details);
      if (details.length > 0) {
        setQueuedDocNames([]);
      }

      if (!kbCreated && (sources.length > 0 || details.length > 0)) {
        setKbCreated(true);
      }

      const sourcesReady = sources.length > 0 && sources.every((s) => s.status === "COMPLETED");
      const allDocsDone =
        details.length > 0 &&
        details.every((d) => d.document_status === "COMPLETED" || d.document_status === "FAILED");

      if (pollGraceRef.current > 0) {
        pollGraceRef.current--;
      } else if (sourcesReady && allDocsDone) {
        setPollingActive(false);
        // Now (not at create-KB return time) is the real moment to tell
        // the parent that derived data — counts on /summary, the KB doc
        // list, etc. — is worth re-fetching: the BE has finished
        // indexing this batch. The ref guard ensures we fire exactly
        // once per Create-KB action (subsequent fetchStatus calls and
        // dialog reopens that find an already-done KB don't re-fire).
        if (pendingKBReadyNotificationRef.current) {
          pendingKBReadyNotificationRef.current = false;
          emitDocumentUploadedEvents(details);
          onKnowledgeBaseCreated?.();
        }
      }
    } catch {
      // silently retry on next interval
    }
  }, [datapackId, kbCreated, onKnowledgeBaseCreated, emitDocumentUploadedEvents]);

  useEffect(() => {
    fetchStatus();
  }, [fetchStatus]);

  useEffect(() => {
    if (pollingActive) {
      pollRef.current = setInterval(fetchStatus, 5000);
    } else {
      clearInterval(pollRef.current);
    }
    return () => clearInterval(pollRef.current);
  }, [pollingActive, fetchStatus]);

  const doneCount = files.filter((f) => f.status === KNOWLEDGE_UPLOAD_STATUS.DONE).length;
  const errorCount = files.filter((f) => f.status === KNOWLEDGE_UPLOAD_STATUS.ERROR).length;

  // Phase computation
  const sourcesDiscovering =
    sourceStatuses.length > 0 && sourceStatuses.some((s) => s.status !== "COMPLETED");
  const sourcesReady =
    sourceStatuses.length > 0 && sourceStatuses.every((s) => s.status === "COMPLETED");

  const docsTotal = pipelineDetails.length;
  const docsCompleted = pipelineDetails.filter((d) => d.document_status === "COMPLETED").length;
  const docsFailed = pipelineDetails.filter((d) => d.document_status === "FAILED").length;
  const docsFinished = docsCompleted + docsFailed;
  const totalChunks = pipelineDetails.reduce((sum, d) => sum + (d.chunks_processed ?? 0), 0);

  const allDone = sourcesReady && docsTotal > 0 && docsFinished === docsTotal;
  const docsProcessing = kbCreated && sourcesReady && docsTotal > 0 && !allDone;
  const progressPct = docsTotal > 0 ? Math.round((docsFinished / docsTotal) * 100) : 0;

  // Disable new uploads while a KB batch is in flight — newly uploaded files
  // can't be attached until the current pipeline finishes and the user can
  // click "Add to Knowledge Base" again.
  const kbBusy = kbCreated && !allDone;

  return (
    <div className="space-y-6">
      {/* Upload section */}
      <div className="rounded-xl border border-dashed border-border bg-card p-6 space-y-4">
        <div className="flex items-center justify-between">
          <div className="flex items-center gap-4">
            <div className="flex h-10 w-10 items-center justify-center rounded-lg bg-blue-500/10">
              <Upload className="w-5 h-5 text-blue-400" />
            </div>
            <div>
              <h3 className="text-sm font-medium text-foreground">
                Upload files to Knowledge Base
              </h3>
              <p className="text-xs text-muted-foreground mt-0.5">
                PDF, TXT, MD, JSON, CSV — up to {KNOWLEDGE_UPLOAD_MAX_FILE_SIZE_MB}MB each,{" "}
                {KNOWLEDGE_UPLOAD_MAX_BATCH} per batch.
                {totalUploaded > 0 && ` ${totalUploaded} uploaded so far.`}
              </p>
            </div>
          </div>
          <div className="flex items-center gap-2">
            {pendingCount > 0 && !kbBusy && (
              <Button
                className="gap-2 bg-blue-600 hover:bg-blue-700 text-white"
                onClick={handleUpload}
                disabled={isUploading}
              >
                {isUploading ? (
                  <Loader2 className="w-4 h-4 animate-spin" />
                ) : (
                  <Upload className="w-4 h-4" />
                )}
                {isUploading
                  ? "Uploading..."
                  : `Upload ${pendingCount} file${pendingCount > 1 ? "s" : ""}`}
              </Button>
            )}
          </div>
        </div>

        {/* Drop zone */}
        <div
          className={cn(
            "flex flex-col items-center justify-center rounded-lg border-2 border-dashed p-8 transition-colors",
            batchFull || kbBusy
              ? "border-border/60 opacity-50 cursor-not-allowed"
              : isDragOver
                ? "border-blue-400 bg-blue-500/10 cursor-pointer"
                : "border-blue-500/50 bg-blue-500/[0.04] hover:border-blue-400 hover:bg-blue-500/10 cursor-pointer",
          )}
          onDragOver={(e) => {
            e.preventDefault();
            if (!batchFull && !kbBusy) setIsDragOver(true);
          }}
          onDragLeave={() => setIsDragOver(false)}
          onDrop={(e) => {
            if (!batchFull && !kbBusy) handleDrop(e);
            else e.preventDefault();
          }}
          onClick={() => !batchFull && !kbBusy && inputRef.current?.click()}
        >
          <Upload className="w-8 h-8 text-muted-foreground mb-2" />
          <p className="text-sm text-muted-foreground">
            {kbBusy ? (
              "Knowledge base is processing — wait for it to finish before adding more files"
            ) : batchFull ? (
              "Batch full — upload or remove files to add more"
            ) : (
              <>
                Drag & drop files here, or <span className="text-blue-400 underline">browse</span>
              </>
            )}
          </p>
          {!batchFull && !kbBusy && batchUsed > 0 && (
            <p className="text-xs text-muted-foreground/60 mt-1">
              {batchRemaining} of {KNOWLEDGE_UPLOAD_MAX_BATCH} slots available
            </p>
          )}
          <input
            ref={inputRef}
            type="file"
            multiple
            accept={KNOWLEDGE_UPLOAD_ACCEPTED_EXTENSIONS}
            disabled={batchFull || kbBusy}
            className="hidden"
            onChange={(e) => {
              if (e.target.files) addFiles(e.target.files);
              e.target.value = "";
            }}
          />
        </div>

        {warning && (
          // Pair a darker shade for light mode with the original `-300`
          // under `dark:`. The `-300` shades are tuned for the
          // saturated-tint backgrounds these banners use in dark mode;
          // on the same tint over a light card the text washed out
          // almost to invisible.
          <div className="flex items-center gap-2 rounded-lg border border-yellow-500/30 bg-yellow-500/10 px-4 py-2 text-xs text-yellow-700 dark:text-yellow-300">
            <AlertCircle className="w-4 h-4 shrink-0" />
            {warning}
          </div>
        )}

        {doneCount > 0 && !isUploading && (
          <div className="flex items-center gap-2 rounded-lg border border-green-500/30 bg-green-500/10 px-4 py-2 text-xs text-green-700 dark:text-green-300">
            <CheckCircle2 className="w-4 h-4 shrink-0" />
            {doneCount} file{doneCount > 1 ? "s" : ""} uploaded successfully.
            {errorCount > 0 && ` ${errorCount} failed.`} You can add more files.
          </div>
        )}

        {/* File list — auto-fill grid so multiple files render side-by-side
            on wider viewports; capped at ~3 rows so a large batch scrolls
            rather than pushing the card taller. */}
        {files.length > 0 && (
          <ul
            className="grid max-h-48 gap-2 overflow-y-auto pr-1"
            style={{ gridTemplateColumns: "repeat(auto-fill, minmax(min(100%, 18rem), 1fr))" }}
          >
            {files.map((entry, i) => (
              <li
                key={`${entry.file.name}-${i}`}
                className="rounded-lg border border-border bg-card px-4 py-2.5 space-y-1.5"
              >
                <div className="flex items-center justify-between gap-3">
                  <div className="flex items-center gap-3 min-w-0">
                    <FileText className="w-4 h-4 shrink-0 text-muted-foreground" />
                    <span className="text-sm text-foreground truncate">{entry.file.name}</span>
                    <span className="text-xs text-muted-foreground shrink-0">
                      {formatFileSize(entry.file.size)}
                    </span>
                  </div>
                  <div className="flex items-center gap-2 shrink-0">
                    {entry.status === KNOWLEDGE_UPLOAD_STATUS.UPLOADING && (
                      <span className="text-xs text-blue-700 dark:text-blue-400 tabular-nums">
                        {entry.progress}%
                      </span>
                    )}
                    {entry.status === KNOWLEDGE_UPLOAD_STATUS.DONE && (
                      <CheckCircle2 className="w-4 h-4 text-green-600 dark:text-green-400" />
                    )}
                    {entry.status === KNOWLEDGE_UPLOAD_STATUS.SKIPPED && (
                      <span className="text-xs font-medium text-muted-foreground">Skipped</span>
                    )}
                    {/* Status-only indicator on the right side — the
                        actual error message moves to a new line below
                        so a long string can't fight the filename for
                        horizontal space and visually overlap it. */}
                    {entry.status === KNOWLEDGE_UPLOAD_STATUS.ERROR && (
                      <AlertCircle
                        className="w-4 h-4 text-red-600 dark:text-red-400"
                        aria-label="Upload failed"
                      />
                    )}
                    {(entry.status === KNOWLEDGE_UPLOAD_STATUS.PENDING ||
                      entry.status === KNOWLEDGE_UPLOAD_STATUS.SKIPPED ||
                      entry.status === KNOWLEDGE_UPLOAD_STATUS.ERROR) && (
                      <button
                        type="button"
                        aria-label="Remove file"
                        className="text-muted-foreground hover:text-foreground transition-colors"
                        onClick={() => removeFile(i)}
                      >
                        <X className="w-4 h-4" />
                      </button>
                    )}
                  </div>
                </div>
                {entry.status === KNOWLEDGE_UPLOAD_STATUS.ERROR && entry.error ? (
                  <p className="mt-1.5 break-words text-xs text-red-700 dark:text-red-400">
                    {entry.error}
                  </p>
                ) : null}
                {entry.status === KNOWLEDGE_UPLOAD_STATUS.SKIPPED && entry.note ? (
                  <p className="mt-1.5 break-words text-xs text-blue-700 dark:text-blue-400">
                    {entry.note}
                  </p>
                ) : null}
                {entry.status === KNOWLEDGE_UPLOAD_STATUS.UPLOADING && (
                  <div className="h-1.5 w-full rounded-full bg-white/10 overflow-hidden">
                    <div
                      className="h-full rounded-full bg-blue-500 transition-all duration-200"
                      style={{ width: `${entry.progress}%` }}
                    />
                  </div>
                )}
              </li>
            ))}
          </ul>
        )}
      </div>

      {/* Knowledge Base section */}
      {(totalUploaded > 0 || kbCreated) && (
        <div className="rounded-xl border border-border bg-card p-6 space-y-4">
          <div className="flex items-center justify-between">
            <div className="flex items-center gap-3">
              <Database className="w-5 h-5 text-purple-400" />
              <h3 className="text-sm font-medium text-foreground">Knowledge Base</h3>
            </div>
            <div className="flex items-center gap-2">
              {!kbCreated && totalUploaded > 0 && (
                <Button
                  className="gap-2 bg-purple-100 hover:bg-purple-200 text-purple-700 dark:bg-purple-500 dark:hover:bg-purple-600 dark:text-white"
                  onClick={handleCreateKB}
                  disabled={creatingKB}
                >
                  {creatingKB ? (
                    <Loader2 className="w-4 h-4 animate-spin" />
                  ) : (
                    <Database className="w-4 h-4" />
                  )}
                  {creatingKB ? "Creating..." : "Create Knowledge Base"}
                </Button>
              )}
              {kbCreated && allDone && newUploadsAfterKB > 0 && (
                <Button
                  className="gap-2 bg-purple-100 hover:bg-purple-200 text-purple-700 dark:bg-purple-500 dark:hover:bg-purple-600 dark:text-white"
                  onClick={handleCreateKB}
                  disabled={creatingKB}
                >
                  {creatingKB ? (
                    <Loader2 className="w-4 h-4 animate-spin" />
                  ) : (
                    <Database className="w-4 h-4" />
                  )}
                  {creatingKB ? "Adding..." : "Add to Knowledge Base"}
                </Button>
              )}
            </div>
          </div>

          {/* Phase-aware status */}
          {kbCreated && !allDone && (
            <div className="space-y-2">
              <div className="flex items-center gap-2">
                <Loader2 className="w-3.5 h-3.5 animate-spin text-purple-400 shrink-0" />
                <span className="text-xs text-muted-foreground">
                  {sourcesDiscovering
                    ? "Discovering files..."
                    : docsProcessing
                      ? `Processing documents — ${docsFinished} of ${docsTotal} complete`
                      : "Initializing..."}
                </span>
              </div>
              {docsProcessing && (
                <div className="h-1.5 w-full rounded-full bg-white/10 overflow-hidden">
                  <div
                    className="h-full rounded-full bg-purple-500 transition-all duration-500"
                    style={{ width: `${progressPct}%` }}
                  />
                </div>
              )}
            </div>
          )}

          {kbCreated && allDone && (
            <p className="text-xs text-muted-foreground">Knowledge base ready</p>
          )}

          {!kbCreated && !creatingKB && (
            <p className="text-xs text-muted-foreground">
              Upload files and create a knowledge base to get started.
            </p>
          )}

          {!kbCreated && creatingKB && (
            <div className="flex items-center gap-2">
              <Loader2 className="w-3.5 h-3.5 animate-spin text-purple-400 shrink-0" />
              <span className="text-xs text-muted-foreground">Creating knowledge base...</span>
            </div>
          )}

          {kbCreated && allDone && newUploadsAfterKB > 0 && (
            <p className="text-xs text-muted-foreground">
              {newUploadsAfterKB} new file{newUploadsAfterKB !== 1 ? "s" : ""} uploaded. Click "Add
              to Knowledge Base" to process.
            </p>
          )}

          {/* Per-document details — capped at ~3 rows so a large batch
              scrolls rather than pushing the card taller. */}
          {(pipelineDetails.length > 0 || (queuedDocNames.length > 0 && kbCreated && !allDone)) && (
            <div className="max-h-36 space-y-1.5 overflow-y-auto pr-1">
              {pipelineDetails.length === 0 &&
                queuedDocNames.map((name, i) => (
                  <div
                    key={`queued-${i}-${name}`}
                    className="flex items-center justify-between rounded-lg border border-border bg-card px-4 py-2"
                  >
                    <div className="flex items-center gap-2 min-w-0">
                      <Loader2 className="w-3.5 h-3.5 text-yellow-400 animate-spin shrink-0" />
                      <span className="text-xs text-foreground truncate">{name}</span>
                    </div>
                    <span className="text-xs font-medium shrink-0 ml-3 text-yellow-400">
                      Queued
                    </span>
                  </div>
                ))}
              {pipelineDetails.map((d) => {
                const displayName = pipelineDocumentDisplayName(d.document_name);
                return (
                  <div
                    key={d.document_name}
                    className="flex items-center justify-between rounded-lg border border-border bg-card px-4 py-2"
                  >
                    <div className="flex items-center gap-2 min-w-0">
                      {/*
                        Tailwind `-400` shades are tuned for dark
                        backgrounds; on the light-mode card they wash
                        out almost to invisible. Pair a darker shade
                        for light mode (`text-{color}-600`) with the
                        original `-400` under `dark:` so each surface
                        gets a readable contrast.
                      */}
                      {d.document_status === "COMPLETED" ? (
                        <CheckCircle2 className="w-3.5 h-3.5 text-green-600 dark:text-green-400 shrink-0" />
                      ) : d.document_status === "FAILED" ? (
                        <AlertCircle className="w-3.5 h-3.5 text-red-600 dark:text-red-400 shrink-0" />
                      ) : (
                        <Loader2 className="w-3.5 h-3.5 text-yellow-600 dark:text-yellow-400 animate-spin shrink-0" />
                      )}
                      <span className="text-xs text-foreground truncate">{displayName}</span>
                    </div>
                    <span
                      className={cn(
                        "text-xs font-medium shrink-0 ml-3",
                        d.document_status === "COMPLETED"
                          ? "text-green-600 dark:text-green-400"
                          : d.document_status === "FAILED"
                            ? "text-red-600 dark:text-red-400"
                            : "text-yellow-600 dark:text-yellow-400",
                      )}
                    >
                      {d.document_status === "COMPLETED"
                        ? `Ready${d.chunks_processed ? ` · ${d.chunks_processed} ${d.chunks_processed === 1 ? "chunk" : "chunks"}` : ""}`
                        : d.document_status === "FAILED"
                          ? "Failed"
                          : "Processing"}
                    </span>
                  </div>
                );
              })}
            </div>
          )}
        </div>
      )}
    </div>
  );
}
