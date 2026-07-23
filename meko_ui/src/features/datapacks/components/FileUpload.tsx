import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Upload, X, FileText, CheckCircle2, AlertCircle, Loader2, Database, Clock } from "lucide-react";
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
import {
  useKnowledgeUploadCache,
  type FileEntry,
} from "@/features/datapacks/knowledgeUploadCache";
import {
  KNOWLEDGE_UPLOAD_ACCEPTED_EXTENSIONS,
  KNOWLEDGE_UPLOAD_EXTENSIONS_HINT,
  KNOWLEDGE_UPLOAD_MAX_BATCH,
  KNOWLEDGE_UPLOAD_MAX_FILE_SIZE_MB,
  dedupNote,
  filterFilesForKnowledgeUploadBatch,
  sanitizeFilenameForMatch,
} from "@/lib/knowledgeFileUploadRules";
import { formatFileSize } from "@/lib/fileSize";
import { formatRelativeTime, useNowMs } from "@/lib/relativeTime";
import { uploadFileToS3 } from "@/lib/uploadFileToS3";
import { KNOWLEDGE_UPLOAD_STATUS } from "@/lib/knowledgeUploadStatus";
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

/** "A run is owed" signal -- upload finished or orphan found. `runProcess`
 *  always re-syncs everything outstanding, so this carries no file payload. */
type ProcessTrigger = {
  reason: "upload" | "orphan";
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
};

export default function FileUpload({
  datapackName,
  datapackId,
  initialFiles,
  onKnowledgeBaseCreated,
}: Props) {
  const { data: currentUser } = useCurrentUser();
  // Session-scoped caches (see KnowledgeUploadCacheProvider). Radix unmounts
  // Dialog.Content on close and the datapack pages toggle via display:none,
  // so these let file/status state survive close/reopen; the provider drops
  // them on logout so nothing leaks across sessions.
  const { fileUploadStateCache, statusCache } = useKnowledgeUploadCache();
  const [files, setFiles] = useState<FileEntry[]>(
    () => fileUploadStateCache.get(datapackId) ?? [],
  );
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
  const [creatingKB, setCreatingKB] = useState(false);
  const [pollingActive, setPollingActive] = useState(false);
  const [sourceStatuses, setSourceStatuses] = useState<SourceStatus[]>(
    () => statusCache.get(datapackId)?.sourceStatuses ?? [],
  );
  const [pipelineDetails, setPipelineDetails] = useState<PipelineDetail[]>(
    () => statusCache.get(datapackId)?.pipelineDetails ?? [],
  );
  useEffect(() => {
    statusCache.set(datapackId, { sourceStatuses, pipelineDetails, kbCreated });
  }, [datapackId, sourceStatuses, pipelineDetails, kbCreated]);

  // Scrolls a doc into view and flashes it the moment it actually starts
  // processing (document_status flips to PROCESSING) -- not just when it
  // first shows up QUEUED. With one worker, that plays out as "previous
  // doc finishes, next one lights up", one at a time; docs already
  // PROCESSING/done from a prior fetch/session are the baseline and are
  // seeded silently by fetchStatus's first call each mount, not here.
  const docRowRefs = useRef<Map<string, HTMLDivElement>>(new Map());
  const flashedProcessingRef = useRef<Set<string>>(new Set());
  const initialStatusLoadedRef = useRef(false);
  const [flashingDocs, setFlashingDocs] = useState<Set<string>>(new Set());
  // Un-cleared timers would keep firing (and their closures alive) after
  // unmount; track by doc name so they can all be cancelled at once.
  const flashTimersRef = useRef<Map<string, ReturnType<typeof setTimeout>>>(new Map());
  useEffect(() => {
    const timers = flashTimersRef.current;
    return () => {
      timers.forEach(clearTimeout);
      timers.clear();
    };
  }, []);
  useEffect(() => {
    if (!initialStatusLoadedRef.current) return;
    const flashed = flashedProcessingRef.current;
    const started = pipelineDetails.filter(
      (d) => d.document_status === "PROCESSING" && !flashed.has(d.document_name),
    );
    if (started.length === 0) return;
    for (const d of started) flashed.add(d.document_name);
    // More than one doc can flip to PROCESSING in the same refresh --
    // calling scrollIntoView once per doc would just have each call
    // cancel the previous one's smooth scroll, landing on whichever
    // happened to run last. Scroll to only the first; every one still
    // gets its own independent flash-fade below.
    docRowRefs.current
      .get(started[0].document_name)
      ?.scrollIntoView({ behavior: "smooth", block: "center" });
    for (const d of started) {
      setFlashingDocs((prev) => new Set(prev).add(d.document_name));
      const timer = setTimeout(() => {
        flashTimersRef.current.delete(d.document_name);
        setFlashingDocs((prev) => {
          if (!prev.has(d.document_name)) return prev;
          const next = new Set(prev);
          next.delete(d.document_name);
          return next;
        });
      }, 1600);
      flashTimersRef.current.set(d.document_name, timer);
    }
  }, [pipelineDetails]);

  const [queuedDocNames, setQueuedDocNames] = useState<string[]>([]);
  // S3-side orphans: PUT to S3 but never registered with dist_rag.
  // Fetched on mount and after each successful Create/Add-to-KB.
  const [pendingOrphans, setPendingOrphans] = useState<PendingUpload[]>([]);
  const [deletingOrphanKeys, setDeletingOrphanKeys] = useState<Set<string>>(() => new Set());
  const nowMs: number = useNowMs();
  const pollRef = useRef<ReturnType<typeof setInterval>>(undefined);
  // Armed on a fresh Create-KB action; fires onKnowledgeBaseCreated once
  // polling confirms all docs reached a terminal status.
  const pendingKBReadyNotificationRef = useRef(false);
  // While a source is discovering, the backend skips syncing new uploads --
  // re-trigger a create once it flips to COMPLETED so they aren't stranded.
  const sourceWasDiscoveringRef = useRef(false);
  const [newUploadsAfterKB, setNewUploadsAfterKB] = useState(0);
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
  const [processError, setProcessError] = useState<string | null>(null);
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
    (d) => d.document_status !== "COMPLETED" && d.document_status !== "FAILED",
  ).length;
  // Total non-terminal files: staged, uploaded-but-not-yet-in-pipeline,
  // orphans, and actively processing docs. Uploading is always allowed,
  // just capped in total.
  const inFlightCount =
    files.filter(
      (f) =>
        f.status === KNOWLEDGE_UPLOAD_STATUS.PENDING || f.status === KNOWLEDGE_UPLOAD_STATUS.UPLOADING,
    ).length +
    doneAwaitingPipeline +
    orphansAwaitingPipeline.length +
    pipelineInFlight;
  const batchRemaining = KNOWLEDGE_UPLOAD_MAX_BATCH - inFlightCount;
  const batchFull = batchRemaining <= 0;

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
        const { accepted, warningParts } = filterFilesForKnowledgeUploadBatch(all, prevInFlightCount);
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
    [showWarning, pipelineDocNames, orphansAwaitingPipeline.length, pipelineInFlight],
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

  const uploadFile = (url: string, file: File, id: string): Promise<void> =>
    uploadFileToS3(url, file, (pct) =>
      setFiles((prev) => prev.map((f) => (f.id === id ? { ...f, progress: pct } : f))),
    );

  // Runs (or queues via scheduleProcess) the create/add-to-KB step. Plain
  // function so it always closes over the latest state when it fires.
  const runProcess = async () => {
    setProcessError(null);
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
    setQueuedDocNames((prev) => Array.from(new Set([...prev, ...queuedNames])));
    setKbCreated(true);
    setPollingActive(true);
    setNewUploadsAfterKB(0);
    pollGraceRef.current = 3;
    setCreatingKB(true);
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
      const message = getApiErrorMessage(err, "Failed to process uploaded files.");
      setProcessError(message);
      showWarning(message);
    } finally {
      setCreatingKB(false);
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
  const scheduleProcess = (reason: ProcessTrigger["reason"] = "upload") => {
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

      setFiles((prev) =>
        prev.map((f) => (f.id === id ? { ...f, status: KNOWLEDGE_UPLOAD_STATUS.UPLOADING, progress: 0 } : f)),
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
          // Identical content exists already. If it matches a pending
          // orphan it's unprocessed, not truly already in the KB.
          const awaitingProcessing = pendingOrphans.some(
            (o) => existing_filename && o.filename === existing_filename,
          );
          if (awaitingProcessing) matchedOrphan = true;
          const note = dedupNote(entry.file.name, existing_filename, awaitingProcessing);
          showWarning(note);
          setFiles((prev) =>
            prev.map((f) =>
              f.id === id ? { ...f, status: KNOWLEDGE_UPLOAD_STATUS.SKIPPED, progress: 0, note } : f,
            ),
          );
          continue;
        }

        await uploadFile(url, entry.file, id);
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
      if (kbCreated) setNewUploadsAfterKB((prev) => prev + uploaded);
    }

    // Upload and process are a single action -- auto-trigger create/add-to-KB.
    // Call via the ref: handleUpload is async and spans render cycles, so the
    // directly-closed-over scheduleProcess would be stale by the time we get
    // here (matches the other async call sites below).
    if (uploaded > 0 || matchedOrphan) {
      scheduleProcessRef.current(uploaded > 0 ? "upload" : "orphan");
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
        .filter((d) => d.document_status === "COMPLETED" && !seen.has(d.document_name))
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
      // Auto-queue leftover orphans too, same as a fresh upload.
      if (resp.items.length > 0) scheduleProcessRef.current("orphan");
    } catch {
      // ignore
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


  const fetchStatus = useCallback(async () => {
    try {
      const status = await getKBStatus(datapackId, {});
      const sources = status.source_statuses ?? [];
      const details = status.pipeline_details ?? [];
      if (!initialStatusLoadedRef.current) {
        // First status fetch this mount: a doc already PROCESSING at this
        // point started before the modal opened, so it's baseline, not a
        // fresh transition -- only docs that flip to PROCESSING *after*
        // this point should flash.
        initialStatusLoadedRef.current = true;
        flashedProcessingRef.current = new Set(
          details.filter((d) => d.document_status === "PROCESSING").map((d) => d.document_name),
        );
      }
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

      if (sources.length > 0 && !sourcesReady) {
        sourceWasDiscoveringRef.current = true;
      } else if (sourcesReady && sourceWasDiscoveringRef.current) {
        sourceWasDiscoveringRef.current = false;
        // Source just finished discovery -- re-trigger create for files
        // uploaded while it was still discovering.
        scheduleProcessRef.current("upload");
      }

      const hasInFlightDocs = details.some(
        (d) => d.document_status === "PROCESSING" || d.document_status === "QUEUED",
      );

      if (pollGraceRef.current > 0) {
        pollGraceRef.current--;
      } else if (sourcesReady && allDocsDone) {
        setPollingActive(false);
        // Real moment indexing is done -- ref guard fires this once per
        // Create-KB action, not on every reopen/poll.
        if (pendingKBReadyNotificationRef.current) {
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
        setPollingActive(true);
      }
    } catch {
      // silently retry on next interval
    }
  }, [datapackId, kbCreated, emitDocumentUploadedEvents, fetchPendingOrphans]);

  useEffect(() => {
    fetchStatus();
    void fetchPendingOrphans();
  }, [fetchStatus, fetchPendingOrphans]);

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

  return (
    <div className="space-y-6">
      {/* Upload section */}
      {/* Tighter mobile padding for more room in a narrow viewport. */}
      <div className="rounded-xl border border-dashed border-border bg-card p-4 space-y-4 md:p-6">
        {/* Wraps below the icon+text on narrow widths instead of squeezing
            the button up against the description. */}
        <div className="flex flex-wrap items-center justify-between gap-3">
          <div className="flex min-w-0 items-center gap-4">
            <div className="flex h-10 w-10 shrink-0 items-center justify-center rounded-lg bg-emerald-500/10">
              <Upload className="w-5 h-5 text-emerald-400" />
            </div>
            <div className="min-w-0">
              <h3 className="text-sm font-medium text-foreground">
                Upload files to Knowledge Base
              </h3>
              <p className="text-xs text-muted-foreground mt-0.5">
                {KNOWLEDGE_UPLOAD_EXTENSIONS_HINT.toUpperCase()} — up to {KNOWLEDGE_UPLOAD_MAX_FILE_SIZE_MB}MB each,{" "}
                {KNOWLEDGE_UPLOAD_MAX_BATCH} in progress at a time.
                {totalUploaded > 0 && ` ${totalUploaded} uploaded so far.`}
              </p>
            </div>
          </div>
          {pendingCount > 0 && (
            <Button
              className="shrink-0 gap-2"
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
                : `Upload and process ${pendingCount} file${pendingCount > 1 ? "s" : ""}`}
            </Button>
          )}
        </div>

        {/* Drop zone. Gated only by the total in-flight cap, not "is processing". */}
        <div
          className={cn(
            "flex flex-col items-center justify-center rounded-lg border-2 border-dashed p-8 transition-colors",
            batchFull
              ? "border-border/60 opacity-50 cursor-not-allowed"
              : isDragOver
                ? "border-emerald-400 bg-emerald-500/10 cursor-pointer"
                : "border-emerald-500/50 bg-emerald-500/[0.04] hover:border-emerald-400 hover:bg-emerald-500/10 cursor-pointer",
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
          <Upload className="w-8 h-8 text-muted-foreground mb-2" />
          <p className="text-sm text-muted-foreground">
            {batchFull ? (
              `${KNOWLEDGE_UPLOAD_MAX_BATCH} files in progress — wait for one to finish or remove a pending file to add more`
            ) : (
              <>
                Drag & drop files here, or <span className="text-emerald-400 underline">browse</span>
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

        {doneCount > 0 && !isUploading && (
          <div className="flex items-center gap-2 rounded-lg border border-green-500/30 bg-green-500/10 px-4 py-2 text-xs text-green-700 dark:text-green-300">
            <CheckCircle2 className="w-4 h-4 shrink-0" />
            {doneCount} file{doneCount > 1 ? "s" : ""} uploaded successfully.
            {errorCount > 0 && ` ${errorCount} failed.`} You can add more files.
          </div>
        )}

        {/* File list -- auto-fill grid, capped height so a large batch scrolls. */}
        {files.length > 0 && (
          <ul
            className="grid max-h-48 gap-2 overflow-y-auto pr-1"
            style={{ gridTemplateColumns: "repeat(auto-fill, minmax(min(100%, 18rem), 1fr))" }}
          >
            {/* `files` is append-only, so reversing gives newest-first. */}
            {[...files].reverse().map((entry) => (
              <li
                key={entry.id}
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
                    {entry.status === KNOWLEDGE_UPLOAD_STATUS.UPLOADING &&
                      (entry.progress > 0 ? (
                        <span className="text-xs text-emerald-700 dark:text-emerald-300 tabular-nums">
                          {entry.progress}%
                        </span>
                      ) : (
                        // 0% while hashing/presigning, before S3 reports bytes.
                        <span className="flex items-center gap-1.5 text-xs text-emerald-700 dark:text-emerald-300">
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
                      <CheckCircle2 className="w-4 h-4 text-green-600 dark:text-green-400" />
                    )}
                    {entry.status === KNOWLEDGE_UPLOAD_STATUS.SKIPPED && (
                      <span className="text-xs font-medium text-muted-foreground">Skipped</span>
                    )}
                    {/* Status-only indicator; error text moves to its own line below. */}
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
                        onClick={() => removeFile(entry.id)}
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
                  <p className="mt-1.5 break-words text-xs text-emerald-700 dark:text-emerald-300">
                    {entry.note}
                  </p>
                ) : null}
                {entry.status === KNOWLEDGE_UPLOAD_STATUS.UPLOADING && (
                  <div className="h-1.5 w-full rounded-full bg-white/10 overflow-hidden">
                    <div
                      className={cn(
                        "h-full rounded-full bg-emerald-500 transition-all duration-200",
                        // Pulse at 0% so the bar doesn't look frozen while presigning.
                        entry.progress > 0 ? "w-full" : "w-1/4 animate-pulse",
                      )}
                      style={entry.progress > 0 ? { width: `${entry.progress}%` } : undefined}
                    />
                  </div>
                )}
              </li>
            ))}
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
                      <FileText className="w-4 h-4 shrink-0 text-muted-foreground" />
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

      {/* Knowledge Base section -- status-only, process is automatic.
          Retry covers the one manual case: the auto attempt itself failed. */}
      {(totalUploaded > 0 || kbCreated || pendingOrphans.length > 0) && (
        <div className="rounded-xl border border-border bg-card p-6 space-y-4">
          <div className="flex items-center justify-between">
            <div className="flex items-center gap-3">
              <Database className="w-5 h-5 text-purple-400" />
              <h3 className="text-sm font-medium text-foreground">Knowledge Base</h3>
            </div>
            {processError && (
              <Button
                className="gap-2 bg-purple-100 hover:bg-purple-200 text-purple-700 dark:bg-purple-500 dark:hover:bg-purple-600 dark:text-white"
                onClick={() => scheduleProcess()}
                disabled={creatingKB}
              >
                {creatingKB ? (
                  <Loader2 className="w-4 h-4 animate-spin" />
                ) : (
                  <Database className="w-4 h-4" />
                )}
                {creatingKB ? "Retrying..." : "Retry"}
              </Button>
            )}
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

          {!kbCreated && !creatingKB && !processError && (
            <p className="text-xs text-muted-foreground">
              Upload files to automatically build the knowledge base.
            </p>
          )}

          {!kbCreated && creatingKB && (
            <div className="flex items-center gap-2">
              <Loader2 className="w-3.5 h-3.5 animate-spin text-purple-400 shrink-0" />
              <span className="text-xs text-muted-foreground">Creating knowledge base...</span>
            </div>
          )}

          {kbCreated && allDone && newUploadsAfterKB > 0 && (
            <div className="flex items-center gap-2">
              <Loader2 className="w-3.5 h-3.5 animate-spin text-purple-400 shrink-0" />
              <span className="text-xs text-muted-foreground">
                {newUploadsAfterKB} new file{newUploadsAfterKB !== 1 ? "s" : ""} uploaded — adding to
                knowledge base...
              </span>
            </div>
          )}

          {processError && (
            <div className="flex items-center gap-2 rounded-lg border border-red-500/30 bg-red-500/10 px-4 py-2 text-xs text-red-700 dark:text-red-300">
              <AlertCircle className="w-4 h-4 shrink-0" />
              {processError}
            </div>
          )}

          {/* Per-document details -- capped height, scrolls for large batches. */}
          {(pipelineDetails.length > 0 || (queuedDocNames.length > 0 && kbCreated && !allDone)) && (
            <div className="max-h-36 space-y-1.5 overflow-y-auto pr-1">
              {pipelineDetails.length === 0 &&
                queuedDocNames.map((name, i) => (
                  <div
                    key={`queued-${i}-${name}`}
                    className="flex items-center justify-between rounded-lg border border-border bg-card px-4 py-2"
                  >
                    <div className="flex items-center gap-2 min-w-0">
                      <Clock className="w-3.5 h-3.5 text-muted-foreground shrink-0" />
                      <span className="text-xs text-foreground truncate">{name}</span>
                    </div>
                    <span className="text-xs font-medium shrink-0 ml-3 text-muted-foreground">
                      Queued
                    </span>
                  </div>
                ))}
              {pipelineDetails.map((d) => {
                const displayName = pipelineDocumentDisplayName(d.document_name);
                return (
                  <div
                    key={d.document_name}
                    ref={(el) => {
                      if (el) docRowRefs.current.set(d.document_name, el);
                      else docRowRefs.current.delete(d.document_name);
                    }}
                    className={cn(
                      "flex items-center justify-between rounded-lg border border-border bg-card px-4 py-2 transition-colors duration-700",
                      flashingDocs.has(d.document_name) && "bg-yellow-500/10 dark:bg-yellow-400/10",
                    )}
                  >
                    <div className="flex items-center gap-2 min-w-0">
                      {/* -600 for light mode, -400 washes out there. Backend
                          (dist_rag) reports QUEUED separately from PROCESSING
                          -- a single worker only actively works one document
                          at a time, so the rest behind it are waiting, not
                          "processing". Distinguishing them here also means
                          this is already correct if/when the worker pool
                          processes several documents in parallel: each one
                          genuinely PROCESSING gets its own spinner. */}
                      {d.document_status === "COMPLETED" ? (
                        <CheckCircle2 className="w-3.5 h-3.5 text-green-600 dark:text-green-400 shrink-0" />
                      ) : d.document_status === "FAILED" ? (
                        <AlertCircle className="w-3.5 h-3.5 text-red-600 dark:text-red-400 shrink-0" />
                      ) : d.document_status === "QUEUED" ? (
                        <Clock className="w-3.5 h-3.5 text-muted-foreground shrink-0" />
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
                            : d.document_status === "QUEUED"
                              ? "text-muted-foreground"
                              : "text-yellow-600 dark:text-yellow-400",
                      )}
                    >
                      {d.document_status === "COMPLETED"
                        ? `Ready${d.chunks_processed ? ` · ${d.chunks_processed} ${d.chunks_processed === 1 ? "chunk" : "chunks"}` : ""}`
                        : d.document_status === "FAILED"
                          ? "Failed"
                          : d.document_status === "QUEUED"
                            ? "Queued"
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
