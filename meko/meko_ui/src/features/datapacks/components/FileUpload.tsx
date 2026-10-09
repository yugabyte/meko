import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Upload, X, CheckCircle2, AlertCircle, Info, Loader2, Clock } from "lucide-react";
import { fileTypeVisual } from "@/features/datapacks/components/knowledge-documents/fileTypeIcons";
import { Button } from "@/components/ui/button";
import { useCurrentUser } from "@/hooks/useCurrentUser";
import { cn } from "@/lib/utils";
import {
  getApiErrorMessage,
  requestUploadURL,
  confirmUpload,
  createKnowledgeBase,
  deletePendingUpload,
  getKBStatus,
  listPendingUploads,
  sha256Hex,
  type PendingUpload,
  type PipelineDetail,
  type SourceStatus,
} from "@/lib/api/mekoApi";
import { useKnowledgeUploadCache, type FileEntry } from "@/features/datapacks/knowledgeUploadCache";
import {
  KNOWLEDGE_UPLOAD_ACCEPTED_EXTENSIONS,
  KNOWLEDGE_UPLOAD_EXTENSIONS_HINT,
  KNOWLEDGE_UPLOAD_MAX_BATCH,
  dedupNote,
  filterFilesForKnowledgeUploadBatch,
  knowledgeFileExtension,
  sanitizeFilenameForMatch,
  unsupportedKnowledgeFileMessage,
} from "@/lib/knowledgeFileUploadRules";
import { useKnowledgeUploadMaxFileSizeMB } from "@/hooks/useKnowledgeUploadMaxFileSizeMB";
import { formatFileSize } from "@/lib/fileSize";
import { formatRelativeTime, useNowMs } from "@/lib/relativeTime";
import { uploadFileToS3 } from "@/lib/uploadFileToS3";
import { verifyFileContent } from "@/lib/sniffFileType";
import { KNOWLEDGE_UPLOAD_STATUS } from "@/lib/knowledgeUploadStatus";
import { KB_DOCUMENT_STATUS } from "@/features/datapacks/components/knowledge-documents/knowledgeDocumentStatus";
import { queryClient } from "@/lib/queryClient";
import { Queue } from "@/lib/queue";
import { ACCOUNT_USAGE_QUERY_KEY } from "@/hooks/useAccountUsage";
import { TELEMETRY_EVENTS } from "@/lib/telemetry/events";
import { trackEvent } from "@/lib/telemetry/rudderstack";
import { bytesToKb, fileTypeFromName } from "@/lib/telemetry/helpers";

let fileEntryIdCounter = 0;
function nextFileEntryId(): string {
  fileEntryIdCounter += 1;
  return `f${fileEntryIdCounter}`;
}

/** Why a process run was queued: a fresh upload finished, or a leftover
 *  S3 orphan was found. */
const PROCESS_TRIGGER_REASON = {
  UPLOAD: "upload",
  ORPHAN: "orphan",
} as const;
type ProcessTriggerReason = (typeof PROCESS_TRIGGER_REASON)[keyof typeof PROCESS_TRIGGER_REASON];

/** "A run is owed" signal -- upload finished or orphan found. `runProcess`
 *  always re-syncs everything outstanding, so this carries no file payload. */
type ProcessTrigger = {
  reason: ProcessTriggerReason;
  queuedAt: number;
};

function pipelineDocumentDisplayName(documentName: string): string {
  const filename = documentName.split("/").pop() ?? documentName;
  return filename.replace(/^\d+_/, "");
}

type Props = {
  datapackName: string;
  datapackId: string;
  /** Optional pre-staged files. Staged once per distinct array reference. */
  initialFiles?: File[];
  /** Called after createKnowledgeBase succeeds. */
  onKnowledgeBaseCreated?: () => void;
  /** Files not yet showing up as a real row wherever the parent renders
   *  its own list (e.g. the persistent Files table, which is otherwise
   *  blind to a datapack's very first upload until dist_rag's
   *  CREATE_SOURCE worker discovers it — see the isNewSource comment in
   *  CreateKB). Status comes from `pipelineDetails` once dist_rag has
   *  picked the file up (so a placeholder can show "Processing", not
   *  just "Queued" forever); falls back to QUEUED for names dist_rag
   *  hasn't reported on at all yet. */
  onQueuedDocsChange?: (docs: { name: string; status: string }[]) => void;
  /** Mirrors this component's own `pollingActive` -- true for as long as
   *  it is actively polling getKBStatus for an in-flight upload. A parent
   *  section rendering its own document list can key its refetch polling
   *  off this directly instead of independently reconstructing "is
   *  anything still processing?" from `onQueuedDocsChange`, whose queued
   *  names get cleared out (see setQueuedDocNames([]) below) as soon as
   *  dist_rag reports *any* pipeline row, well before the specific file
   *  is actually done -- too early a signal to safely stop polling on. */
  onProcessingActiveChange?: (active: boolean) => void;
  /** Bumped by the parent after a document is removed from the KB (or a
   *  new one is created) elsewhere -- e.g. the "Remove from KB" action in
   *  the persistent Files table below. Forces a fresh getKBStatus fetch
   *  so a deleted file's row in the "Knowledge Base" progress list above
   *  actually disappears instead of lingering until the next poll tick
   *  (or forever, once polling has already stopped because everything
   *  looked done). */
  refreshKey?: number;
  /** Shared pulse from the parent -- bumped every 3s during processing.
   *  Triggers a fresh getKBStatus fetch so the widget and the parent's
   *  document list stay in perfect sync. */
  pollTick?: number;
  /** Minimal presentation: no header block, a single-line drop zone. For a
   *  surface where the surrounding page already said what the uploader is
   *  and the widget is the escape hatch rather than the main event (the
   *  Labs knowledge lesson). The upload behaviour is identical. */
  compact?: boolean;
};

export default function FileUpload({
  datapackName,
  datapackId,
  initialFiles,
  onKnowledgeBaseCreated,
  onQueuedDocsChange,
  onProcessingActiveChange,
  refreshKey,
  pollTick,
  compact,
}: Props) {
  const { data: currentUser } = useCurrentUser();
  // Session-scoped caches (see KnowledgeUploadCacheProvider). Radix unmounts
  // Dialog.Content on close and the datapack pages toggle via display:none,
  // so these let file/status state survive close/reopen; the provider drops
  // them on logout so nothing leaks across sessions.
  const { fileUploadStateCache, statusCache } = useKnowledgeUploadCache();
  const [files, setFiles] = useState<FileEntry[]>(() => fileUploadStateCache.get(datapackId) ?? []);
  // Last `initialFiles` array already staged; compared by reference so a
  // new array (e.g. a fresh sample seed) gets re-added.
  const seededInitialFilesRef = useRef<File[] | undefined>(undefined);
  // Write-through so state survives close/reopen.
  useEffect(() => {
    fileUploadStateCache.set(datapackId, files);
  }, [datapackId, files]);
  const [isDragOver, setIsDragOver] = useState(false);
  const [warning, setWarning] = useState<string | null>(null);
  const [totalUploaded, setTotalUploaded] = useState(0);
  const inputRef = useRef<HTMLInputElement>(null);
  const warningTimer = useRef<ReturnType<typeof setTimeout>>(undefined);

  const [kbCreated, setKbCreated] = useState(() => statusCache.get(datapackId)?.kbCreated ?? false);
  const [pollingActive, setPollingActive] = useState(false);
  const onProcessingActiveChangeRef = useRef(onProcessingActiveChange);
  onProcessingActiveChangeRef.current = onProcessingActiveChange;
  useEffect(() => {
    onProcessingActiveChangeRef.current?.(pollingActive);
  }, [pollingActive]);
  const [sourceStatuses, setSourceStatuses] = useState<SourceStatus[]>(
    () => statusCache.get(datapackId)?.sourceStatuses ?? [],
  );
  const [pipelineDetails, setPipelineDetails] = useState<PipelineDetail[]>(
    () => statusCache.get(datapackId)?.pipelineDetails ?? [],
  );
  useEffect(() => {
    statusCache.set(datapackId, { sourceStatuses, pipelineDetails, kbCreated });
  }, [datapackId, sourceStatuses, pipelineDetails, kbCreated]);

  const initialStatusLoadedRef = useRef(false);

  const [queuedDocNames, setQueuedDocNames] = useState<string[]>([]);
  // Ref indirection so fetchStatus (a useCallback that must stay stable
  // across polls) can read the current queued list without depending on
  // it directly -- see the "truly empty vs. not-yet-discovered" check
  // inside fetchStatus below.
  const queuedDocNamesRef = useRef(queuedDocNames);
  queuedDocNamesRef.current = queuedDocNames;
  // Pairs each queued name with its live pipeline status when dist_rag has
  // reported one (matched the same way doneAwaitingPipeline below matches
  // a raw upload against pipelineDetails), falling back to QUEUED for a
  // name dist_rag hasn't discovered at all yet.
  //
  // Three sources, most-authoritative first, so the parent (Files table)
  // has an unbroken placeholder from the moment a file's upload succeeds
  // until its real row shows up in ListDistRagDocuments -- each handoff
  // between them was a window where the file appeared nowhere:
  //   1. names dist_rag has discovered but not finished (non-terminal
  //      details), covering "discovered" -> "listed"
  //   2. optimistic names in queuedDocNames, covering "batch uploaded" ->
  //      "discovered"
  //   3. individual files whose upload has succeeded, covering "this file
  //      uploaded" -> "whole batch uploaded"
  const queuedDocsWithStatus = useMemo(() => {
    // Keyed by sanitized name for stable matching across the lifecycle.
    const results = new Map<string, { displayName: string; status: string }>();

    // 1. Start with everything dist_rag has discovered but not finished yet.
    // These are the "most real" placeholders we have.
    for (const d of pipelineDetails) {
      const status = d.document_status;
      if (status !== KB_DOCUMENT_STATUS.COMPLETED && status !== KB_DOCUMENT_STATUS.FAILED) {
        const displayName = pipelineDocumentDisplayName(d.document_name);
        results.set(sanitizeFilenameForMatch(displayName), { displayName, status });
      }
    }

    // 2. Overlay anything still in queuedDocNames (optimistic).
    // If it's already in results from above, its status was already set correctly.
    // If not, it's still QUEUED.
    for (const name of queuedDocNames) {
      const sanitized = sanitizeFilenameForMatch(name);
      if (!results.has(sanitized)) {
        results.set(sanitized, { displayName: name, status: KB_DOCUMENT_STATUS.QUEUED });
      }
    }

    // 3. Files whose own upload has succeeded (confirmUpload returned, so
    // the bytes are in S3 and claimed) but which aren't in queuedDocNames
    // yet. runProcess is what populates that list, and it runs once after
    // the whole batch finishes -- so on a multi-file upload the first file
    // to land would otherwise show nothing until the slowest one caught
    // up. Keyed off each file's own DONE, this flips it to Queued the
    // moment its own upload succeeds. Files still uploading are
    // deliberately absent: nothing is queued until the bytes are actually
    // there, and the upload widget already shows their progress.
    for (const f of files) {
      if (f.status !== KNOWLEDGE_UPLOAD_STATUS.DONE) continue;
      const sanitized = sanitizeFilenameForMatch(f.file.name);
      if (!results.has(sanitized)) {
        results.set(sanitized, { displayName: f.file.name, status: KB_DOCUMENT_STATUS.QUEUED });
      }
    }

    return Array.from(results.values()).map(({ displayName, status }) => ({
      name: displayName,
      status,
    }));
  }, [queuedDocNames, pipelineDetails, files]);
  // Ref indirection so the effect below doesn't need onQueuedDocsChange as
  // a dependency -- an unmemoized parent callback would otherwise thrash it.
  const onQueuedDocsChangeRef = useRef(onQueuedDocsChange);
  onQueuedDocsChangeRef.current = onQueuedDocsChange;
  useEffect(() => {
    onQueuedDocsChangeRef.current?.(queuedDocsWithStatus);
  }, [queuedDocsWithStatus]);
  // S3-side orphans: PUT to S3 but never registered with dist_rag.
  // Fetched on mount and after each successful Create/Add-to-KB.
  const [pendingOrphans, setPendingOrphans] = useState<PendingUpload[]>([]);
  const [deletingOrphanKeys, setDeletingOrphanKeys] = useState<Set<string>>(() => new Set());
  const nowMs: number = useNowMs();
  // Armed on a fresh Create-KB action; fires onKnowledgeBaseCreated once
  // polling confirms all docs reached a terminal status.
  const pendingKBReadyNotificationRef = useRef(false);
  // Ensures we only fire the "indexing complete" callback once per
  // transition, preventing the infinite refresh loop seen when refreshKey
  // and fetchStatus trigger each other. Reset whenever a fresh process
  // starts (runProcess).
  const didNotifyCreatedRef = useRef(false);
  // While a source is discovering, the backend skips syncing new uploads --
  // re-trigger a create once it flips to COMPLETED so they aren't stranded.
  const sourceWasDiscoveringRef = useRef(false);
  const pollGraceRef = useRef(0);
  // Telemetry bookkeeping: byte sizes by filename, and doc_names already
  // seen, so document_uploaded fires once per genuinely new COMPLETED doc.
  const uploadedSizesRef = useRef<Map<string, number>>(new Map());
  const seenDocNamesRef = useRef<Set<string>>(new Set());
  // Ref indirection so fetchStatus doesn't need to depend on (and thrash
  // polling from) an unmemoized parent callback.
  const onKnowledgeBaseCreatedRef = useRef(onKnowledgeBaseCreated);
  onKnowledgeBaseCreatedRef.current = onKnowledgeBaseCreated;
  // Mirrors `files` for handleUpload's loop to re-check live status by id
  // mid-loop instead of trusting a stale snapshot.
  const filesRef = useRef<FileEntry[]>(files);
  useEffect(() => {
    filesRef.current = files;
  }, [files]);
  // On close (unmount), keep only still-staged rows in the cache. Anything
  // done/skipped/errored has already been surfaced this session, and an
  // in-flight upload loses its progress callbacks on unmount anyway (the
  // component's setFiles can no longer be reached), so it can't be
  // meaningfully resumed -- only a never-started, staged file can.
  useEffect(() => {
    return () => {
      const staged = filesRef.current.filter((f) => f.status === KNOWLEDGE_UPLOAD_STATUS.PENDING);
      fileUploadStateCache.set(datapackId, staged);
    };
  }, [datapackId]);
  // Serializes createKnowledgeBase calls (no unique constraint backend-side,
  // so overlapping calls can double-insert). Triggers queue up; runProcess
  // always re-syncs everything outstanding in one call, so drain empties
  // the whole queue per pass rather than one trigger per run.
  const processQueueRef = useRef<Queue<ProcessTrigger>>(new Queue());
  const processInFlightRef = useRef(false);
  // Ref indirection so the stable fetchPendingOrphans can reach the latest
  // scheduleProcess closure without being recreated itself.
  const scheduleProcessRef = useRef<(reason?: ProcessTrigger["reason"]) => void>(() => {});

  const showWarning = useCallback((msg: string) => {
    setWarning(msg);
    clearTimeout(warningTimer.current);
    warningTimer.current = setTimeout(() => setWarning(null), 4000);
  }, []);

  const pendingCount = files.filter((f) => f.status === KNOWLEDGE_UPLOAD_STATUS.PENDING).length;
  const isUploading = files.some((f) => f.status === KNOWLEDGE_UPLOAD_STATUS.UPLOADING);
  // Uploaded+confirmed files not yet showing up in pipelineDetails. Memoized
  // so its identity is stable across renders (it's a dependency of addFiles).
  const pipelineDocNames = useMemo(
    () => new Set(pipelineDetails.map((d) => pipelineDocumentDisplayName(d.document_name))),
    [pipelineDetails],
  );
  const doneAwaitingPipeline = files.filter(
    (f) =>
      f.status === KNOWLEDGE_UPLOAD_STATUS.DONE &&
      !pipelineDocNames.has(sanitizeFilenameForMatch(f.file.name)),
  ).length;
  // Exclude orphans already in pipelineDetails so they aren't double-counted.
  const orphansAwaitingPipeline = pendingOrphans.filter((o) => !pipelineDocNames.has(o.filename));
  const pipelineInFlight = pipelineDetails.filter(
    (d) =>
      d.document_status !== KB_DOCUMENT_STATUS.COMPLETED &&
      d.document_status !== KB_DOCUMENT_STATUS.FAILED,
  ).length;
  // Total non-terminal files: staged, uploaded-but-not-yet-in-pipeline,
  // orphans, and actively processing docs. Uploading is always allowed,
  // just capped in total.
  const inFlightCount =
    files.filter(
      (f) =>
        f.status === KNOWLEDGE_UPLOAD_STATUS.PENDING ||
        f.status === KNOWLEDGE_UPLOAD_STATUS.UPLOADING,
    ).length +
    doneAwaitingPipeline +
    orphansAwaitingPipeline.length +
    pipelineInFlight;
  const batchRemaining = KNOWLEDGE_UPLOAD_MAX_BATCH - inFlightCount;
  const batchFull = batchRemaining <= 0;

  // Tier-aware per-file cap (5 MB free / 50 MB standard / 60 MB pro) — used
  // by both the client-side filter and the hint copy below.
  const maxFileSizeMB = useKnowledgeUploadMaxFileSizeMB();

  const addFiles = useCallback(
    (incoming: FileList | File[]) => {
      const all = Array.from(incoming);
      setFiles((prev) => {
        // Recompute the files-derived part of the in-flight count from `prev`
        // (not the render-scope snapshot) so rapid successive calls don't share
        // a stale count and over-fill the batch. The orphan/pipeline parts come
        // from other state and are stable within this updater.
        const prevInFlightCount =
          prev.filter(
            (f) =>
              f.status === KNOWLEDGE_UPLOAD_STATUS.PENDING ||
              f.status === KNOWLEDGE_UPLOAD_STATUS.UPLOADING ||
              (f.status === KNOWLEDGE_UPLOAD_STATUS.DONE &&
                !pipelineDocNames.has(sanitizeFilenameForMatch(f.file.name))),
          ).length +
          orphansAwaitingPipeline.length +
          pipelineInFlight;
        const { accepted, warningParts } = filterFilesForKnowledgeUploadBatch(
          all,
          prevInFlightCount,
          maxFileSizeMB,
        );
        if (warningParts.length > 0) showWarning(warningParts.join(". "));
        if (accepted.length === 0) return prev;
        return [
          ...prev,
          ...accepted.map((f) => ({
            id: nextFileEntryId(),
            file: f,
            status: KNOWLEDGE_UPLOAD_STATUS.PENDING,
            progress: 0,
          })),
        ];
      });
    },
    [
      showWarning,
      pipelineDocNames,
      orphansAwaitingPipeline.length,
      pipelineInFlight,
      maxFileSizeMB,
    ],
  );

  useEffect(() => {
    if (initialFiles && initialFiles.length > 0 && initialFiles !== seededInitialFilesRef.current) {
      seededInitialFilesRef.current = initialFiles;
      addFiles(initialFiles);
    }
  }, [initialFiles, addFiles]);

  const removeFile = useCallback((id: string) => {
    setFiles((prev) => prev.filter((f) => f.id !== id));
  }, []);

  // Countdown expiry (or its own manual X) removes the card outright --
  // doneCount/errorCount are derived live from `files`, so a dismissed
  // entry drops out of the "N failed" tally along with its row.
  const dismissNotice = removeFile;

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

  const uploadFile = (
    url: string,
    fields: Record<string, string>,
    file: File,
    id: string,
  ): Promise<void> =>
    uploadFileToS3(url, fields, file, (pct) =>
      setFiles((prev) => prev.map((f) => (f.id === id ? { ...f, progress: pct } : f))),
    );

  // Runs (or queues via scheduleProcess) the create/add-to-KB step. Plain
  // function so it always closes over the latest state when it fires.
  const runProcess = async () => {
    // Read from filesRef (not `files`) so an async invocation — e.g. at the
    // end of handleUpload or via the process queue — filters the latest
    // statuses rather than a stale closure snapshot.
    const queuedEntries = filesRef.current.filter((f) => f.status === KNOWLEDGE_UPLOAD_STATUS.DONE);
    const queuedNames = queuedEntries.map((f) => f.file.name);
    // Telemetry snapshot: merge sizes across passes, snapshot docs already
    // indexed before this pass so we only emit for new ones.
    uploadedSizesRef.current = new Map([
      ...Array.from(uploadedSizesRef.current.entries()),
      ...queuedEntries.map(
        (f) => [sanitizeFilenameForMatch(f.file.name), f.file.size] as [string, number],
      ),
    ]);
    seenDocNamesRef.current = new Set(pipelineDetails.map((d) => d.document_name));
    const previousKbCreated = kbCreated;

    // Flip UI state immediately for instant feedback. Merge, don't
    // overwrite, queued names in case an earlier pass hasn't shown up yet.
    const nextQueuedNames = Array.from(new Set([...queuedDocNamesRef.current, ...queuedNames]));
    setQueuedDocNames(nextQueuedNames);

    // Notify parent immediately so the "Queued" placeholders show up at the
    // same moment as the widget's "Queued" list.
    const queuedDocs = nextQueuedNames.map((name) => ({ name, status: KB_DOCUMENT_STATUS.QUEUED }));
    onQueuedDocsChangeRef.current?.(queuedDocs);

    // Only reset the latch if this is a fresh UPLOAD (user clicked a button)
    // or ORPHAN check (page load). Terminal indexing completion itself
    // (branch "all-done") sets it to true; resetting it here ensures that
    // if the user then uploads a *second* batch, we are ready to notify
    // again when that batch finishes.
    didNotifyCreatedRef.current = false;
    setKbCreated(true);
    setPollingActive(true);
    pollGraceRef.current = 3;
    try {
      await createKnowledgeBase(datapackId, {});
      // Arm the post-poll notification; fetchStatus fires it once docs
      // reach a terminal status (indexing is still in flight here).
      pendingKBReadyNotificationRef.current = true;
      // Prime status immediately instead of waiting for the next poll tick.
      await fetchStatus();
    } catch (err) {
      setKbCreated(previousKbCreated);
      if (!previousKbCreated) setPollingActive(false);
      showWarning(getApiErrorMessage(err, "Failed to process uploaded files."));
    } finally {
      processInFlightRef.current = false;
      // Drain again: a trigger enqueued mid-run needs its own pass.
      drainProcessQueue();
    }
  };

  // If a run is active, leave the queue untouched -- its `finally` drains
  // again once done. Otherwise pop everything (coalesced into one
  // runProcess pass) and start a run.
  const drainProcessQueue = () => {
    if (processInFlightRef.current) return;
    if (processQueueRef.current.isEmpty()) return;
    while (!processQueueRef.current.isEmpty()) processQueueRef.current.dequeue();
    processInFlightRef.current = true;
    void runProcess();
  };

  // Single entry point for "something is ready to be processed". Only this
  // should ever push onto the queue.
  const scheduleProcess = (reason: ProcessTrigger["reason"] = PROCESS_TRIGGER_REASON.UPLOAD) => {
    processQueueRef.current.enqueue({ reason, queuedAt: Date.now() });
    drainProcessQueue();
  };
  scheduleProcessRef.current = scheduleProcess;

  const handleUpload = async () => {
    const idsToUpload = files
      .filter((f) => f.status === KNOWLEDGE_UPLOAD_STATUS.PENDING)
      .map((f) => f.id);
    if (idsToUpload.length === 0) return;

    let uploaded = 0;
    let matchedOrphan = false;

    for (const id of idsToUpload) {
      // Re-check live status: the user may have removed this entry before its turn.
      const entry = filesRef.current.find((f) => f.id === id);
      if (!entry || entry.status !== KNOWLEDGE_UPLOAD_STATUS.PENDING) continue;

      // Mark UPLOADING before any async step, no await in between: otherwise
      // the gap while a file is mid-sniff/mid-hash leaves the upload button
      // clickable and a second click re-enters this loop over the same
      // still-PENDING ids (filesRef.current not yet updated) and double-uploads.
      setFiles((prev) =>
        prev.map((f) =>
          f.id === id ? { ...f, status: KNOWLEDGE_UPLOAD_STATUS.UPLOADING, progress: 0 } : f,
        ),
      );

      // Decided from the filename, so it costs nothing: refuse before the hash
      // and the presign, neither of which an unsupported type can ever use.
      const unsupported = unsupportedKnowledgeFileMessage(entry.file.name);
      if (unsupported) {
        // No banner: the card below says this, and says which file it is.
        setFiles((prev) =>
          prev.map((f) =>
            f.id === id
              ? { ...f, status: KNOWLEDGE_UPLOAD_STATUS.ERROR, progress: 0, error: unsupported }
              : f,
          ),
        );
        continue;
      }

      // Reads only 3 KB, far cheaper than hashing the whole file, so a
      // spoofed file fails before the hash and the presign, not after.
      // uploadFileToS3 re-checks right before the transfer too, since the
      // file can still change between now and then.
      const contentIssue = await verifyFileContent(entry.file);
      if (contentIssue) {
        setFiles((prev) =>
          prev.map((f) =>
            f.id === id
              ? { ...f, status: KNOWLEDGE_UPLOAD_STATUS.ERROR, progress: 0, error: contentIssue }
              : f,
          ),
        );
        continue;
      }

      try {
        const contentHash = await sha256Hex(entry.file);
        const { url, fields, key, already_uploaded, existing_filename } = await requestUploadURL(
          datapackId,
          entry.file.name,
          entry.file.size,
          contentHash,
        );

        if (already_uploaded) {
          // Identical content exists already. If it matches a pending
          // orphan it's unprocessed, not truly already in the KB.
          const awaitingProcessing = pendingOrphans.some(
            (o) => existing_filename && o.filename === existing_filename,
          );
          if (awaitingProcessing) matchedOrphan = true;
          const note = dedupNote(entry.file.name, existing_filename, awaitingProcessing);
          setFiles((prev) =>
            prev.map((f) =>
              f.id === id
                ? { ...f, status: KNOWLEDGE_UPLOAD_STATUS.SKIPPED, progress: 0, note }
                : f,
            ),
          );
          continue;
        }

        await uploadFile(url, fields ?? {}, entry.file, id);
        await confirmUpload(datapackId, key, entry.file.name, entry.file.size);
        // Post-confirm, since confirm is what marks the bytes as "yours".
        void queryClient.invalidateQueries({ queryKey: [ACCOUNT_USAGE_QUERY_KEY] });

        uploaded++;
        setFiles((prev) =>
          prev.map((f) =>
            f.id === id ? { ...f, status: KNOWLEDGE_UPLOAD_STATUS.DONE, progress: 100 } : f,
          ),
        );
      } catch (err) {
        setFiles((prev) =>
          prev.map((f) =>
            f.id === id
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
    }

    // Upload and process are a single action -- auto-trigger create/add-to-KB.
    // Call via the ref: handleUpload is async and spans render cycles, so the
    // directly-closed-over scheduleProcess would be stale by the time we get
    // here (matches the other async call sites below).
    if (uploaded > 0 || matchedOrphan) {
      scheduleProcessRef.current(
        uploaded > 0 ? PROCESS_TRIGGER_REASON.UPLOAD : PROCESS_TRIGGER_REASON.ORPHAN,
      );
    }
  };

  // Fire document_uploaded once per newly-completed doc, comparing raw
  // document_names against the pre-batch snapshot.
  const emitDocumentUploadedEvents = useCallback(
    (details: PipelineDetail[]) => {
      const seen = seenDocNamesRef.current;
      const hadDocsBefore = seen.size > 0;
      const totalDocs = details.length;
      details
        .filter(
          (d) => d.document_status === KB_DOCUMENT_STATUS.COMPLETED && !seen.has(d.document_name),
        )
        .forEach((d) => {
          seen.add(d.document_name);
          const name = pipelineDocumentDisplayName(d.document_name);
          const sizeBytes = uploadedSizesRef.current.get(name);
          trackEvent(TELEMETRY_EVENTS.DOCUMENT_UPLOADED, {
            email: currentUser?.email,
            datapack_id: datapackId,
            document_id: name,
            file_type: fileTypeFromName(name),
            file_size_kb: sizeBytes !== undefined ? bytesToKb(sizeBytes) : undefined,
            is_first: !hadDocsBefore,
            total_docs: totalDocs,
          });
        });
    },
    [datapackId, currentUser?.email],
  );

  // Load the S3-orphan list. Called on mount and after each Create/Add-to-KB
  // run. Silent failure -- orphans are a UX affordance, not load-bearing.
  const fetchPendingOrphans = useCallback(async () => {
    try {
      const resp = await listPendingUploads(datapackId);
      setPendingOrphans(resp.items);
      return resp.items;
    } catch {
      return [];
    }
  }, [datapackId]);

  // Functional updater avoids depending on deletingOrphanKeys, which
  // mutates on every X click.
  const removeOrphan = useCallback(
    async (key: string) => {
      let alreadyDeleting = false;
      setDeletingOrphanKeys((prev) => {
        if (prev.has(key)) {
          alreadyDeleting = true;
          return prev;
        }
        const next = new Set(prev);
        next.add(key);
        return next;
      });
      if (alreadyDeleting) return;

      try {
        await deletePendingUpload(datapackId, key);
        setPendingOrphans((prev) => prev.filter((o) => o.key !== key));
        void queryClient.invalidateQueries({ queryKey: [ACCOUNT_USAGE_QUERY_KEY] });
      } catch (err) {
        showWarning(getApiErrorMessage(err, "Failed to remove file."));
      } finally {
        setDeletingOrphanKeys((prev) => {
          const next = new Set(prev);
          next.delete(key);
          return next;
        });
      }
    },
    [datapackId, showWarning],
  );

  const kbCreatedRef = useRef(kbCreated);
  kbCreatedRef.current = kbCreated;
  const pollingActiveRef = useRef(pollingActive);
  pollingActiveRef.current = pollingActive;

  const fetchStatus = useCallback(async () => {
    try {
      const status = await getKBStatus(datapackId, {});
      const sources = status.source_statuses ?? [];
      const details = status.pipeline_details ?? [];
      if (!initialStatusLoadedRef.current) {
        initialStatusLoadedRef.current = true;
      }
      setSourceStatuses(sources);
      setPipelineDetails(details);
      // Prune individually, by name -- not "any details exist, so drop
      // everything". A KB with pre-existing docs has details.length > 0
      // from the very first poll, before dist_rag has even discovered
      // *this* upload's files; wholesale-clearing here would zero out
      // queuedDocNames (and, via queuedDocsWithStatus below, the parent's
      // placeholder Map) before the parent's own document fetch has any
      // chance to pick the new files up, permanently freezing their
      // reported status one poll cycle too early. Keeping a name until
      // dist_rag reports *that specific* document means the parent
      // keeps getting fresh QUEUED/PROCESSING updates for it right up
      // until its real row exists.
      if (details.length > 0) {
        const discovered = new Set(
          details.map((d) => pipelineDocumentDisplayName(d.document_name)),
        );
        setQueuedDocNames((prev) => {
          const next = prev.filter((name) => !discovered.has(sanitizeFilenameForMatch(name)));
          return next.length === prev.length ? prev : next;
        });
      }

      const sourcesReady =
        sources.length > 0 && sources.every((s) => s.status === KB_DOCUMENT_STATUS.COMPLETED);

      // Hydrate kbCreated from the backend (e.g. a fresh mount/reload) --
      // but only for signals that mean "there's real activity or content
      // here", not "a source row for this datapack merely exists". A
      // source registration is created once and never removed even after
      // every document under it is deleted, so `sources.length > 0` alone
      // is true forever; using it here caused a live infinite loop (see
      // the reset-to-empty branch below): this check would flip kbCreated
      // back to true every time that branch had just flipped it to false,
      // because the still-there-forever source row looked like "new
      // content" on the very next poll. `!sourcesReady` (still
      // discovering) is the part of "sources exist" that's actually
      // transient and worth hydrating for.
      if (!kbCreatedRef.current && (details.length > 0 || (sources.length > 0 && !sourcesReady))) {
        setKbCreated(true);
      }

      const allDocsDone =
        details.length > 0 &&
        details.every(
          (d) =>
            d.document_status === KB_DOCUMENT_STATUS.COMPLETED ||
            d.document_status === KB_DOCUMENT_STATUS.FAILED,
        );

      if (sources.length > 0 && !sourcesReady) {
        sourceWasDiscoveringRef.current = true;
      } else if (sourcesReady && sourceWasDiscoveringRef.current) {
        sourceWasDiscoveringRef.current = false;
        // Source just finished discovery -- re-trigger create for files
        // uploaded while it was still discovering.
        scheduleProcessRef.current(PROCESS_TRIGGER_REASON.UPLOAD);
      }

      const hasInFlightDocs = details.some(
        (d) =>
          d.document_status === KB_DOCUMENT_STATUS.PROCESSING ||
          d.document_status === KB_DOCUMENT_STATUS.QUEUED,
      );

      let branch = "none-matched";
      if (pollGraceRef.current > 0) {
        branch = "grace-decrement";
        pollGraceRef.current--;
      } else if (sourcesReady && allDocsDone) {
        branch = "all-done";
        // Real moment indexing is done. We call the created callback if
        // we were actively polling (transition from Processing to Done)
        // or if a fresh Create-KB was just armed. Bumping the callback
        // triggers a parent refetch; because we also setPollingActive(false)
        // here, the parent's poll tick stops, preventing an infinite
        // refresh loop.
        const wasPolling = pollingActiveRef.current;
        setPollingActive(false);

        if ((wasPolling || pendingKBReadyNotificationRef.current) && !didNotifyCreatedRef.current) {
          didNotifyCreatedRef.current = true;
          pendingKBReadyNotificationRef.current = false;
          emitDocumentUploadedEvents(details);
          onKnowledgeBaseCreatedRef.current?.();
          // Orphans just processed are now tracked -- refetch to collapse the list.
          void fetchPendingOrphans();
        }
      } else if (hasInFlightDocs || (sources.length > 0 && !sourcesReady)) {
        // Indexing is still running (e.g. the modal was reopened mid-process,
        // where polling was only ever armed by runProcess). Re-arm the poll
        // loop so we catch the terminal transition live instead of freezing
        // on the mount snapshot until the next close/reopen.
        branch = "still-in-flight";
        setPollingActive(true);
      } else if (
        kbCreatedRef.current &&
        details.length === 0 &&
        queuedDocNamesRef.current.length === 0
      ) {
        // Every document was deleted. Deliberately keyed on details.length
        // alone, not sources.length too: a live repro showed sources
        // staying at length 1 forever after the only document was
        // deleted -- source_statuses tracks the datapack's S3 source
        // registration itself (created once, independent of how many
        // documents currently live under it), not a per-document row, so
        // it never goes back to 0 just because the last document did.
        // Requiring sources.length === 0 here meant this branch could
        // never fire post-delete, leaving kbCreated (which nothing else
        // ever clears) stuck true forever with docsTotal pinned at 0 --
        // none of the phase branches above matched, so the widget showed
        // "Initializing..." indefinitely with nothing left to initialize.
        // By this point in the if/else-if chain, the earlier branches
        // have already ruled out "still discovering" and "docs in
        // flight" (both would have matched above), so details.length
        // being the only thing that's actually 0 here is enough on its
        // own to call this a genuine empty KB and reset to the
        // pre-upload state. The grace window (decremented above) and the
        // queuedDocNamesRef check still guard the transient window right
        // after a fresh upload, before dist_rag has discovered the new
        // document rows yet.
        branch = "reset-to-empty";
        setKbCreated(false);
        setPollingActive(false);
      }
    } catch (err) {
      // noop
    }
  }, [datapackId, emitDocumentUploadedEvents, fetchPendingOrphans]);

  // Auto-prune successfully uploaded files (status=DONE) once dist_rag has
  // finalized them -- either indexed (COMPLETED) or FAILED -- since the
  // Files table below is now the single source of truth for that status;
  // leaving a "Staged…"/checkmark row sitting here after that just
  // duplicates it. Also prunes a DONE entry that's neither finalized nor
  // in-flight at all, meaning it was deleted from the KB before dist_rag
  // ever reported a terminal status for it.
  useEffect(() => {
    if (!initialStatusLoadedRef.current || pollGraceRef.current > 0) return;

    const known = new Set([
      ...pipelineDetails.map((d) => pipelineDocumentDisplayName(d.document_name)),
      ...queuedDocNames.map((name) => sanitizeFilenameForMatch(name)),
    ]);
    const finalized = new Set(
      pipelineDetails
        .filter(
          (d) =>
            d.document_status === KB_DOCUMENT_STATUS.COMPLETED ||
            d.document_status === KB_DOCUMENT_STATUS.FAILED,
        )
        .map((d) => sanitizeFilenameForMatch(pipelineDocumentDisplayName(d.document_name))),
    );

    setFiles((prev) => {
      const next = prev.filter((f) => {
        if (f.status !== KNOWLEDGE_UPLOAD_STATUS.DONE) return true;
        const key = sanitizeFilenameForMatch(f.file.name);
        if (finalized.has(key)) return false;
        // If it's neither in the real backend list nor in our "about to be real"
        // queued list, it was likely deleted or never registered.
        return known.has(key);
      });
      return next.length === prev.length ? prev : next;
    });
  }, [pipelineDetails, queuedDocNames, refreshKey]);

  // Background polling for status + orphans list.
  useEffect(() => {
    fetchStatus();
    void fetchPendingOrphans();
    // refreshKey and pollTick aren't read inside fetchStatus/
    // fetchPendingOrphans -- they are only here to force this effect to
    // re-run after a change in the parent (e.g. the 3s synchronized poll
    // tick, or a delete elsewhere).
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [fetchStatus, fetchPendingOrphans, refreshKey, pollTick]);

  // One-time orphan auto-queue on mount.
  useEffect(() => {
    void fetchPendingOrphans().then((items) => {
      if (items.length > 0) {
        scheduleProcessRef.current(PROCESS_TRIGGER_REASON.ORPHAN);
      }
    });
  }, [fetchPendingOrphans]);

  const doneCount = files.filter((f) => f.status === KNOWLEDGE_UPLOAD_STATUS.DONE).length;
  const errorCount = files.filter((f) => f.status === KNOWLEDGE_UPLOAD_STATUS.ERROR).length;

  // Files that have something to say go first, then the rest newest-first
  // (`files` is append-only, so reversing gives that). Drop a batch where only
  // a few are bad and the bad ones would otherwise be scattered through a
  // scrolling list, each on a countdown the user can't see to stop.
  const attentionFirstFiles = [...files]
    .reverse()
    .sort((a, b) => Number(needsAttention(b)) - Number(needsAttention(a)));

  return (
    <div className={compact ? "space-y-3" : "space-y-6"}>
      {/* Upload section */}
      {/* Tighter mobile padding for more room in a narrow viewport. */}
      <div
        className={cn(
          "rounded-xl bg-card",
          compact ? "space-y-2" : "border border-dashed border-border p-4 space-y-4 md:p-6",
        )}
      >
        {/* Wraps below the icon+text on narrow widths instead of squeezing
            the button up against the description. */}
        <div className="flex flex-wrap items-center justify-between gap-3">
          {/* Compact drops the whole header: the page around it has already
              said what this is, and repeating it is most of the widget's
              height. The Upload button below still appears when files queue. */}
          {!compact && (
            <div className="flex min-w-0 items-center gap-4">
              <div className="flex h-10 w-10 shrink-0 items-center justify-center rounded-lg bg-context-knowledge/10">
                <Upload className="w-5 h-5 text-context-knowledge" />
              </div>
              <div className="min-w-0">
                <h3 className="text-sm font-medium text-foreground">
                  Upload files to Knowledge Base
                </h3>
                <p className="text-xs text-muted-foreground mt-0.5">
                  {KNOWLEDGE_UPLOAD_EXTENSIONS_HINT.toUpperCase()}
                  {Number.isFinite(maxFileSizeMB)
                    ? ` — up to ${maxFileSizeMB >= 1024 ? `${(maxFileSizeMB / 1024).toFixed(maxFileSizeMB % 1024 === 0 ? 0 : 1)}GB` : `${maxFileSizeMB}MB`} each,`
                    : " —"}{" "}
                  {KNOWLEDGE_UPLOAD_MAX_BATCH} in progress at a time.
                </p>
                {/* Its own line, and "in this session" spelled out: next to the
                    batch cap this tally read as a fraction of it -- "3 uploaded"
                    after "10 at a time" looks like 3 of 10 rather than a count
                    of what has finished since the page opened. */}
                {totalUploaded > 0 && (
                  <p className="text-xs text-muted-foreground/70 mt-0.5">
                    {totalUploaded} file{totalUploaded === 1 ? "" : "s"} uploaded in this session.
                  </p>
                )}
              </div>
            </div>
          )}
          {pendingCount > 0 && (
            <Button className="shrink-0 gap-2" onClick={handleUpload} disabled={isUploading}>
              {isUploading ? (
                <Loader2 className="w-4 h-4 animate-spin" />
              ) : (
                <Upload className="w-4 h-4" />
              )}
              {isUploading
                ? "Uploading..."
                : `Upload and process ${pendingCount} file${pendingCount > 1 ? "s" : ""}`}
            </Button>
          )}
        </div>

        {/* Drop zone. Gated only by the total in-flight cap, not "is processing". */}
        <div
          className={cn(
            "flex rounded-lg border-2 border-dashed transition-colors",
            compact
              ? "flex-row items-center justify-center gap-2 px-3 py-2.5"
              : "flex-col items-center justify-center p-8",
            batchFull
              ? "border-border/60 opacity-50 cursor-not-allowed"
              : isDragOver
                ? "border-context-knowledge bg-context-knowledge/10 cursor-pointer"
                : "border-context-knowledge/50 bg-context-knowledge/[0.04] hover:border-context-knowledge hover:bg-context-knowledge/10 cursor-pointer",
          )}
          onDragOver={(e) => {
            e.preventDefault();
            if (!batchFull) setIsDragOver(true);
          }}
          onDragLeave={() => setIsDragOver(false)}
          onDrop={(e) => {
            if (!batchFull) handleDrop(e);
            else e.preventDefault();
          }}
          onClick={() => !batchFull && inputRef.current?.click()}
        >
          <Upload
            className={cn(
              "shrink-0 text-muted-foreground",
              compact ? "h-3.5 w-3.5" : "w-8 h-8 mb-2",
            )}
          />
          <p className={cn("text-muted-foreground", compact ? "text-xs" : "text-sm")}>
            {batchFull ? (
              `${KNOWLEDGE_UPLOAD_MAX_BATCH} files in progress — wait for one to finish or remove a pending file to add more`
            ) : compact ? (
              <>
                <b className="font-semibold text-foreground">Or drop your own</b>:{" "}
                {KNOWLEDGE_UPLOAD_EXTENSIONS_HINT.toUpperCase()} ·{" "}
                {Number.isFinite(maxFileSizeMB) ? `up to ${maxFileSizeMB}MB each` : "any size"}
              </>
            ) : (
              <>
                Drag & drop files here, or{" "}
                <span className="text-context-knowledge underline">browse</span>
              </>
            )}
          </p>
          {!batchFull && inFlightCount > 0 && (
            <p className="text-xs text-muted-foreground/60 mt-1">
              {batchRemaining} of {KNOWLEDGE_UPLOAD_MAX_BATCH} slots available
            </p>
          )}
          <input
            ref={inputRef}
            type="file"
            multiple
            accept={KNOWLEDGE_UPLOAD_ACCEPTED_EXTENSIONS}
            disabled={batchFull}
            className="hidden"
            onChange={(e) => {
              if (e.target.files) addFiles(e.target.files);
              e.target.value = "";
            }}
          />
        </div>

        {warning && (
          // Darker shade for light mode; `-300` washes out on a light card.
          <div className="flex items-center gap-2 rounded-lg border border-yellow-500/30 bg-yellow-500/10 px-4 py-2 text-xs text-yellow-700 dark:text-yellow-300">
            <AlertCircle className="w-4 h-4 shrink-0" />
            {warning}
          </div>
        )}

        {/* Suppressed in compact mode: the surface embedding this one says
            what the upload achieved in its own terms, and two success banners
            stacked read as two different things having succeeded. A FAILURE
            still shows either way — that one is never redundant. */}
        {doneCount > 0 && !isUploading && (!compact || errorCount > 0) && (
          <div className="flex items-center gap-2 rounded-lg border border-success/30 bg-success/10 px-4 py-2 text-xs text-success">
            <CheckCircle2 className="w-4 h-4 shrink-0" />
            {doneCount} file{doneCount > 1 ? "s" : ""} uploaded successfully.
            {errorCount > 0 && ` ${errorCount} failed.`} You can add more files.
          </div>
        )}

        {/* File list -- auto-fill grid, capped height so a large batch scrolls.
            The cap fits roughly two cards carrying a message; shorter and a
            rejected file's explanation is cut off by the scroll edge. */}
        {files.length > 0 && (
          <ul
            className="grid max-h-80 gap-2 overflow-y-auto pr-1"
            style={{ gridTemplateColumns: "repeat(auto-fill, minmax(min(100%, 18rem), 1fr))" }}
          >
            {attentionFirstFiles.map((entry) => {
              const { Icon: FileTypeIcon, toneClass } = fileTypeVisual(
                knowledgeFileExtension(entry.file.name),
              );
              return (
                <li
                  key={entry.id}
                  className={cn(
                    "rounded-xl border px-4 py-2.5 space-y-1.5 transition-colors",
                    // Tints the whole card, not just the message, so the eye
                    // finds the odd one out in a grid. Amber for skipped since
                    // a duplicate is a no-op, not the failure red claims.
                    entry.status === KNOWLEDGE_UPLOAD_STATUS.ERROR
                      ? "border-red-500/40 bg-red-500/[0.07] dark:border-red-400/30 dark:bg-red-500/10"
                      : entry.status === KNOWLEDGE_UPLOAD_STATUS.SKIPPED
                        ? "border-amber-500/40 bg-amber-500/[0.07] dark:border-amber-400/30 dark:bg-amber-500/10"
                        : "border-border bg-card",
                  )}
                >
                  <div className="flex items-center justify-between gap-3">
                    <div className="flex items-center gap-3 min-w-0">
                      <FileTypeIcon
                        className={cn(
                          "w-4 h-4 shrink-0",
                          // A file that didn't make it drops its type color: the
                          // card already carries the state, and a green CSV
                          // glyph inside a red one reads as a mixed signal.
                          entry.status === KNOWLEDGE_UPLOAD_STATUS.ERROR
                            ? "text-red-600 dark:text-red-400"
                            : entry.status === KNOWLEDGE_UPLOAD_STATUS.SKIPPED
                              ? "text-amber-600 dark:text-amber-400"
                              : toneClass,
                        )}
                      />
                      <span className="text-sm font-medium text-foreground truncate">
                        {entry.file.name}
                      </span>
                      <span className="text-xs text-muted-foreground shrink-0 tabular-nums">
                        {formatFileSize(entry.file.size)}
                      </span>
                    </div>
                    <div className="flex items-center gap-2 shrink-0">
                      {entry.status === KNOWLEDGE_UPLOAD_STATUS.UPLOADING &&
                        (entry.progress > 0 ? (
                          <span className="text-xs text-context-knowledge tabular-nums">
                            {entry.progress.toFixed(1)}%
                          </span>
                        ) : (
                          // 0% while hashing/presigning, before S3 reports bytes.
                          <span className="flex items-center gap-1.5 text-xs text-context-knowledge">
                            <Loader2 className="w-3.5 h-3.5 animate-spin" />
                            Preparing…
                          </span>
                        ))}
                      {entry.status === KNOWLEDGE_UPLOAD_STATUS.PENDING && (
                        <span className="flex items-center gap-1.5 text-xs text-muted-foreground">
                          <Clock className="w-3.5 h-3.5" />
                          Staged…
                        </span>
                      )}
                      {entry.status === KNOWLEDGE_UPLOAD_STATUS.DONE && (
                        <CheckCircle2 className="w-4 h-4 text-success" />
                      )}
                      {entry.status === KNOWLEDGE_UPLOAD_STATUS.SKIPPED && (
                        <span className="text-xs font-medium text-amber-700 dark:text-amber-300">
                          Skipped
                        </span>
                      )}
                      {/* Only when there's no message panel below to carry its own
                        icon -- two red circles in one small card is noise. */}
                      {entry.status === KNOWLEDGE_UPLOAD_STATUS.ERROR && !entry.error && (
                        <AlertCircle
                          className="w-4 h-4 text-red-600 dark:text-red-400"
                          aria-label="Upload failed"
                        />
                      )}
                      {(entry.status === KNOWLEDGE_UPLOAD_STATUS.PENDING ||
                        entry.status === KNOWLEDGE_UPLOAD_STATUS.DONE ||
                        entry.status === KNOWLEDGE_UPLOAD_STATUS.SKIPPED ||
                        entry.status === KNOWLEDGE_UPLOAD_STATUS.ERROR) && (
                        <button
                          type="button"
                          aria-label="Remove file"
                          className="text-muted-foreground hover:text-foreground transition-colors"
                          onClick={() => removeFile(entry.id)}
                        >
                          <X className="w-4 h-4" />
                        </button>
                      )}
                    </div>
                  </div>
                  {entry.status === KNOWLEDGE_UPLOAD_STATUS.ERROR && entry.error ? (
                    <TimedNotice
                      tone="error"
                      message={entry.error}
                      onDismiss={() => dismissNotice(entry.id)}
                    />
                  ) : null}
                  {entry.status === KNOWLEDGE_UPLOAD_STATUS.SKIPPED && entry.note ? (
                    <TimedNotice
                      tone="skipped"
                      message={entry.note}
                      onDismiss={() => dismissNotice(entry.id)}
                    />
                  ) : null}
                  {entry.status === KNOWLEDGE_UPLOAD_STATUS.UPLOADING && (
                    // `bg-white/10` was invisible on a light card -- the track has
                    // to come from a token that inverts with the theme.
                    <div className="h-1.5 w-full rounded-full bg-foreground/10 overflow-hidden">
                      <div
                        className={cn(
                          "h-full rounded-full bg-context-knowledge transition-all duration-200",
                          // Pulse at 0% so the bar doesn't look frozen while presigning.
                          entry.progress > 0 ? "w-full" : "w-1/4 animate-pulse",
                        )}
                        style={entry.progress > 0 ? { width: `${entry.progress}%` } : undefined}
                      />
                    </div>
                  )}
                </li>
              );
            })}
          </ul>
        )}

        {/* Orphans from a previous session, auto-queued via fetchPendingOrphans.
            Uses orphansAwaitingPipeline so a row drops out once it shows up below. */}
        {orphansAwaitingPipeline.length > 0 && (
          <ul
            className="grid gap-2 pr-1"
            style={{ gridTemplateColumns: "repeat(auto-fill, minmax(min(100%, 18rem), 1fr))" }}
          >
            {orphansAwaitingPipeline.map((o) => {
              const isDeleting = deletingOrphanKeys.has(o.key);
              const { Icon: FileTypeIcon, toneClass } = fileTypeVisual(
                knowledgeFileExtension(o.filename),
              );
              return (
                <li
                  key={o.key}
                  className={cn(
                    "rounded-lg border border-border bg-card px-4 py-2.5 space-y-1",
                    isDeleting && "opacity-50",
                  )}
                >
                  {/* Two-row layout: filename+X on top, size+time below. */}
                  <div className="flex items-center justify-between gap-3">
                    <div className="flex items-center gap-3 min-w-0">
                      <FileTypeIcon className={cn("w-4 h-4 shrink-0", toneClass)} />
                      <span className="text-sm text-foreground truncate">{o.filename}</span>
                    </div>
                    <button
                      type="button"
                      aria-label="Remove file"
                      disabled={isDeleting}
                      className="shrink-0 text-muted-foreground transition-colors hover:text-foreground disabled:cursor-not-allowed disabled:opacity-50"
                      onClick={() => void removeOrphan(o.key)}
                    >
                      {isDeleting ? (
                        <Loader2 className="w-4 h-4 animate-spin" />
                      ) : (
                        <X className="w-4 h-4" />
                      )}
                    </button>
                  </div>
                  <div className="flex items-center justify-between pl-7 text-xs text-muted-foreground">
                    <span>{formatFileSize(o.size)}</span>
                    <div className="flex items-center gap-1">
                      <Clock className="w-3.5 h-3.5" aria-hidden />
                      <span>
                        already uploaded
                        {o.uploaded_at ? ` · ${formatRelativeTime(o.uploaded_at, nowMs)}` : ""}
                      </span>
                    </div>
                  </div>
                </li>
              );
            })}
          </ul>
        )}
      </div>
    </div>
  );
}

/** A file that uploaded nothing and is explaining why, on a countdown. */
function needsAttention(f: FileEntry): boolean {
  return (
    (f.status === KNOWLEDGE_UPLOAD_STATUS.ERROR && !!f.error) ||
    (f.status === KNOWLEDGE_UPLOAD_STATUS.SKIPPED && !!f.note)
  );
}

/**
 * Scales with how much there is to read (50ms/char, ~250-300 wpm), clamped
 * to 6-10s: 6 covers slow/non-native readers past the 2-4s an average reader
 * needs, 10 caps it before an already-read card is just clutter.
 */
function autoDismissMs(message: string): number {
  return Math.min(10_000, Math.max(6_000, 50 * message.length));
}

const NOTICE_TONES = {
  error: {
    Icon: AlertCircle,
    panel: "border-red-500/25 bg-red-500/10 dark:border-red-400/25",
    text: "text-red-800 dark:text-red-200",
    track: "bg-red-500/15 dark:bg-red-400/20",
    bar: "bg-red-500/70 dark:bg-red-400/70",
  },
  skipped: {
    Icon: Info,
    panel: "border-amber-500/25 bg-amber-500/10 dark:border-amber-400/25",
    text: "text-amber-800 dark:text-amber-200",
    track: "bg-amber-500/15 dark:bg-amber-400/20",
    bar: "bg-amber-500/70 dark:bg-amber-400/70",
  },
} as const;

/**
 * The explanation for a rejected/skipped-duplicate file, on a countdown --
 * the whole card is gone once it ends (onDismiss removes the entry). Not a
 * trap: the bar shows the deadline, hover/focus pauses it (WCAG 2.2.1), and
 * it doesn't start until on screen, so below-the-fold entries wait their turn.
 */
function TimedNotice({
  tone,
  message,
  onDismiss,
}: {
  tone: keyof typeof NOTICE_TONES;
  message: string;
  onDismiss: () => void;
}) {
  const { Icon, panel, text, track, bar } = NOTICE_TONES[tone];
  const totalMs = useMemo(() => autoDismissMs(message), [message]);
  const [remainingMs, setRemainingMs] = useState(totalMs);
  // Pointer over it, or keyboard focus on it -- either says the user is
  // reading, so the countdown waits.
  const [held, setHeld] = useState(false);
  const [onScreen, setOnScreen] = useState(false);
  const panelRef = useRef<HTMLDivElement>(null);

  // Ref so a caller passing a fresh closure each render can't restart the
  // countdown, which would leave the card up forever.
  const onDismissRef = useRef(onDismiss);
  onDismissRef.current = onDismiss;

  useEffect(() => {
    const el = panelRef.current;
    if (!el) return;
    // Half of it visible, so a notice peeking in at the edge of the scroll
    // container doesn't count as read.
    const observer = new IntersectionObserver(([e]) => setOnScreen(e.isIntersecting), {
      threshold: 0.5,
    });
    observer.observe(el);
    return () => observer.disconnect();
  }, []);

  const paused = held || !onScreen;

  useEffect(() => {
    if (paused) return;
    const TICK_MS = 100;
    const id = setInterval(() => setRemainingMs((ms) => Math.max(0, ms - TICK_MS)), TICK_MS);
    return () => clearInterval(id);
  }, [paused]);

  // Dismissing from the tick itself would fire a parent state update from
  // inside a state updater.
  useEffect(() => {
    if (remainingMs === 0) onDismissRef.current();
  }, [remainingMs]);

  return (
    <div
      ref={panelRef}
      role={tone === "error" ? "alert" : "status"}
      // Focusable, or the focus half of the pause never fires: onFocus is
      // focusin, and nothing inside here can take focus. Without the tab stop
      // a keyboard user has no way to hold the message open, which is the
      // mechanism WCAG 2.2.1 asks for.
      tabIndex={0}
      onMouseEnter={() => setHeld(true)}
      onMouseLeave={() => setHeld(false)}
      onFocus={() => setHeld(true)}
      onBlur={() => setHeld(false)}
      className={cn(
        "mt-2 space-y-2 rounded-lg border px-2.5 py-2",
        "outline-none focus-visible:ring-2 focus-visible:ring-ring",
        panel,
      )}
    >
      <p className={cn("flex items-start gap-2 text-xs leading-relaxed", text)}>
        <Icon className="mt-px h-3.5 w-3.5 shrink-0" aria-hidden />
        <span className="min-w-0 break-words">{message}</span>
      </p>
      <div aria-hidden className={cn("h-0.5 w-full overflow-hidden rounded-full", track)}>
        <div
          // Fills left to right as the countdown runs, matching the upload
          // bar above it: in this card a growing bar reads as "time spent",
          // and both bars ending full keeps the two from meaning opposite
          // things on the same surface.
          className={cn("h-full rounded-full transition-[width] duration-100 ease-linear", bar)}
          style={{ width: `${((totalMs - remainingMs) / totalMs) * 100}%` }}
        />
      </div>
    </div>
  );
}
