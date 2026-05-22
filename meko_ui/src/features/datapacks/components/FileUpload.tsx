import { useCallback, useEffect, useRef, useState } from "react";
import { Upload, X, FileText, CheckCircle2, AlertCircle, Loader2, Database } from "lucide-react";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import {
  getApiErrorMessage,
  requestUploadURL,
  confirmUpload,
  createKnowledgeBase,
  getKBStatus,
  type PipelineDetail,
  type SourceStatus,
} from "@/lib/mekoApi";
import {
  KNOWLEDGE_UPLOAD_ACCEPTED_EXTENSIONS,
  KNOWLEDGE_UPLOAD_MAX_BATCH,
  KNOWLEDGE_UPLOAD_MAX_FILE_SIZE_MB,
  filterFilesForKnowledgeUploadBatch,
  filterDuplicateKnowledgeFileNames,
} from "@/lib/knowledgeFileUploadRules";
import { formatFileSize } from "@/lib/fileSize";
import { KNOWLEDGE_UPLOAD_STATUS, type KnowledgeUploadStatus } from "@/lib/knowledgeUploadStatus";

type FileEntry = {
  file: File;
  status: KnowledgeUploadStatus;
  progress: number;
  error?: string;
};

type Props = {
  datapackName: string;
  datapackId?: string;
  /** Optional pre-staged files (e.g. a sample seed). Captured on first mount only. */
  initialFiles?: File[];
};

export default function FileUpload({ datapackName, datapackId, initialFiles }: Props) {
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
  const [newUploadsAfterKB, setNewUploadsAfterKB] = useState(0);
  const pollGraceRef = useRef(0);

  const showWarning = useCallback((msg: string) => {
    setWarning(msg);
    clearTimeout(warningTimer.current);
    warningTimer.current = setTimeout(() => setWarning(null), 4000);
  }, []);

  const pendingCount = files.filter((f) => f.status === KNOWLEDGE_UPLOAD_STATUS.PENDING).length;
  const isUploading = files.some((f) => f.status === KNOWLEDGE_UPLOAD_STATUS.UPLOADING);
  const batchUsed = files.filter(
    (f) => f.status === KNOWLEDGE_UPLOAD_STATUS.PENDING || f.status === KNOWLEDGE_UPLOAD_STATUS.UPLOADING,
  ).length;
  const batchRemaining = KNOWLEDGE_UPLOAD_MAX_BATCH - batchUsed;
  const batchFull = batchRemaining <= 0;

  const addFiles = useCallback((incoming: FileList | File[]) => {
    const all = Array.from(incoming);
    setFiles((prev) => {
      const { unique, duplicateCount } = filterDuplicateKnowledgeFileNames(
        all,
        prev.map((e) => e.file.name),
      );
      const currentBatch = prev.filter(
        (f) => f.status === KNOWLEDGE_UPLOAD_STATUS.PENDING || f.status === KNOWLEDGE_UPLOAD_STATUS.UPLOADING,
      ).length;
      const { accepted, warningParts } = filterFilesForKnowledgeUploadBatch(unique, currentBatch);
      if (duplicateCount > 0) {
        warningParts.unshift(`${duplicateCount} skipped (already in queue)`);
      }
      if (warningParts.length > 0) showWarning(warningParts.join(". "));
      if (accepted.length === 0) return prev;
      return [
        ...prev,
        ...accepted.map((f) => ({ file: f, status: KNOWLEDGE_UPLOAD_STATUS.PENDING, progress: 0 })),
      ];
    });
  }, [showWarning]);

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

  const uploadFileToS3 = (url: string, file: File, index: number): Promise<void> => {
    const UPLOAD_TIMEOUT_MS = 30000;
    return new Promise((resolve, reject) => {
      const xhr = new XMLHttpRequest();
      xhr.timeout = UPLOAD_TIMEOUT_MS;
      xhr.open("PUT", url);
      xhr.setRequestHeader("Content-Type", file.type || "application/octet-stream");

      xhr.upload.onprogress = (e) => {
        if (e.lengthComputable) {
          const pct = Math.round((e.loaded / e.total) * 100);
          setFiles((prev) =>
            prev.map((f, idx) => (idx === index ? { ...f, progress: pct } : f)),
          );
        }
      };

      xhr.onload = () => {
        if (xhr.status >= 200 && xhr.status < 300) resolve();
        else reject(new Error(`${xhr.status} ${xhr.statusText}: ${xhr.responseText}`));
      };
      xhr.onerror = () => reject(new Error(`Network error (status=${xhr.status})`));
      xhr.ontimeout = () => reject(new Error(`Request timed out after ${UPLOAD_TIMEOUT_MS / 1000}s`));
      xhr.send(file);
    });
  };

  const handleUpload = async () => {
    if (files.length === 0) return;

    let uploaded = 0;

    for (let i = 0; i < files.length; i++) {
      const entry = files[i];
      if (entry.status !== KNOWLEDGE_UPLOAD_STATUS.PENDING) continue;

      setFiles((prev) =>
        prev.map((f, idx) => (idx === i ? { ...f, status: KNOWLEDGE_UPLOAD_STATUS.UPLOADING, progress: 0 } : f)),
      );

      try {
        const { url, key } = await requestUploadURL(
          datapackName,
          entry.file.name,
          entry.file.type || "application/octet-stream",
          entry.file.size,
          { datapackId },
        );

        await uploadFileToS3(url, entry.file, i);
        await confirmUpload(datapackName, key, entry.file.name, entry.file.size, { datapackId });

        uploaded++;
        setFiles((prev) =>
          prev.map((f, idx) => (idx === i ? { ...f, status: KNOWLEDGE_UPLOAD_STATUS.DONE, progress: 100 } : f)),
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
    const queuedNames = files
      .filter((f) => f.status === KNOWLEDGE_UPLOAD_STATUS.DONE)
      .map((f) => f.file.name);
    const previousKbCreated = kbCreated;

    // Flip UI state immediately so users see progress feedback right away.
    setQueuedDocNames(queuedNames);
    setKbCreated(true);
    setPollingActive(true);
    setNewUploadsAfterKB(0);
    pollGraceRef.current = 3;
    setCreatingKB(true);
    try {
      await createKnowledgeBase(datapackName, { datapackId });
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

  const fetchStatus = useCallback(async () => {
    try {
      const status = await getKBStatus(datapackName, { datapackId });
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
      }
    } catch {
      // silently retry on next interval
    }
  }, [datapackName, kbCreated]);

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
  const sourcesDiscovering = sourceStatuses.length > 0 && sourceStatuses.some((s) => s.status !== "COMPLETED");
  const sourcesReady = sourceStatuses.length > 0 && sourceStatuses.every((s) => s.status === "COMPLETED");

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
              <h3 className="text-sm font-medium text-foreground">Upload files to Knowledge Base</h3>
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
                {isUploading ? <Loader2 className="w-4 h-4 animate-spin" /> : <Upload className="w-4 h-4" />}
                {isUploading ? "Uploading..." : `Upload ${pendingCount} file${pendingCount > 1 ? "s" : ""}`}
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
            {kbBusy
              ? "Knowledge base is processing — wait for it to finish before adding more files"
              : batchFull
                ? "Batch full — upload or remove files to add more"
                : <>Drag & drop files here, or <span className="text-blue-400 underline">browse</span></>}
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
          <div className="flex items-center gap-2 rounded-lg border border-yellow-500/30 bg-yellow-500/10 px-4 py-2 text-xs text-yellow-300">
            <AlertCircle className="w-4 h-4 shrink-0" />
            {warning}
          </div>
        )}

        {doneCount > 0 && !isUploading && (
          <div className="flex items-center gap-2 rounded-lg border border-green-500/30 bg-green-500/10 px-4 py-2 text-xs text-green-300">
            <CheckCircle2 className="w-4 h-4 shrink-0" />
            {doneCount} file{doneCount > 1 ? "s" : ""} uploaded successfully.
            {errorCount > 0 && ` ${errorCount} failed.`}
            {" "}You can add more files.
          </div>
        )}

        {/* File list — auto-fill grid so multiple files render side-by-side
            on wider viewports; capped height with vertical scroll once it
            overflows so the dialog itself doesn't stretch. */}
        {files.length > 0 && (
          <ul
            className="grid max-h-72 gap-2 overflow-y-auto pr-1"
            style={{ gridTemplateColumns: "repeat(auto-fill, minmax(min(100%, 18rem), 1fr))" }}
          >
            {files.map((entry, i) => (
              <li
                key={`${entry.file.name}-${i}`}
                className="rounded-lg border border-border bg-card px-4 py-2.5 space-y-1.5"
              >
                <div className="flex items-center justify-between">
                  <div className="flex items-center gap-3 min-w-0">
                    <FileText className="w-4 h-4 shrink-0 text-muted-foreground" />
                    <span className="text-sm text-foreground truncate">{entry.file.name}</span>
                    <span className="text-xs text-muted-foreground shrink-0">
                      {formatFileSize(entry.file.size)}
                    </span>
                  </div>
                  <div className="flex items-center gap-2 shrink-0">
                    {entry.status === KNOWLEDGE_UPLOAD_STATUS.UPLOADING && (
                      <span className="text-xs text-blue-400 tabular-nums">{entry.progress}%</span>
                    )}
                    {entry.status === KNOWLEDGE_UPLOAD_STATUS.DONE && (
                      <CheckCircle2 className="w-4 h-4 text-green-400" />
                    )}
                    {entry.status === KNOWLEDGE_UPLOAD_STATUS.ERROR && (
                      <span className="flex items-center gap-1 text-xs text-red-400">
                        <AlertCircle className="w-4 h-4" />
                        {entry.error}
                      </span>
                    )}
                    {(entry.status === KNOWLEDGE_UPLOAD_STATUS.PENDING ||
                      entry.status === KNOWLEDGE_UPLOAD_STATUS.ERROR) && (
                        <button
                          type="button"
                          className="text-muted-foreground hover:text-white transition-colors"
                          onClick={() => removeFile(i)}
                        >
                          <X className="w-4 h-4" />
                        </button>
                      )}
                  </div>
                </div>
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
                  {creatingKB ? <Loader2 className="w-4 h-4 animate-spin" /> : <Database className="w-4 h-4" />}
                  {creatingKB ? "Creating..." : "Create Knowledge Base"}
                </Button>
              )}
              {kbCreated && allDone && newUploadsAfterKB > 0 && (
                <Button
                  className="gap-2 bg-purple-100 hover:bg-purple-200 text-purple-700 dark:bg-purple-500 dark:hover:bg-purple-600 dark:text-white"
                  onClick={handleCreateKB}
                  disabled={creatingKB}
                >
                  {creatingKB ? <Loader2 className="w-4 h-4 animate-spin" /> : <Database className="w-4 h-4" />}
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
            <p className="text-xs text-muted-foreground">Upload files and create a knowledge base to get started.</p>
          )}

          {!kbCreated && creatingKB && (
            <div className="flex items-center gap-2">
              <Loader2 className="w-3.5 h-3.5 animate-spin text-purple-400 shrink-0" />
              <span className="text-xs text-muted-foreground">Creating knowledge base...</span>
            </div>
          )}

          {kbCreated && allDone && newUploadsAfterKB > 0 && (
            <p className="text-xs text-muted-foreground">
              {newUploadsAfterKB} new file{newUploadsAfterKB !== 1 ? "s" : ""} uploaded. Click "Add to Knowledge Base" to process.
            </p>
          )}

          {/* Per-document details — capped + scrollable so a large batch
              doesn't push the dialog (or page) into a long stack. */}
          {(pipelineDetails.length > 0 || (queuedDocNames.length > 0 && kbCreated && !allDone)) && (
            <div className="max-h-72 space-y-1.5 overflow-y-auto pr-1">
              {pipelineDetails.length === 0 &&
                queuedDocNames.map((name, i) => (
                  <div key={`queued-${i}-${name}`} className="flex items-center justify-between rounded-lg border border-border bg-card px-4 py-2">
                    <div className="flex items-center gap-2 min-w-0">
                      <Loader2 className="w-3.5 h-3.5 text-yellow-400 animate-spin shrink-0" />
                      <span className="text-xs text-foreground truncate">{name}</span>
                    </div>
                    <span className="text-xs font-medium shrink-0 ml-3 text-yellow-400">
                      Queued
                    </span>
                  </div>
                ))}
              {pipelineDetails.map((d, i) => {
                const filename = d.document_name.split("/").pop() ?? d.document_name;
                const displayName = filename.replace(/^\d+_/, "");
                return (
                  <div key={i} className="flex items-center justify-between rounded-lg border border-border bg-card px-4 py-2">
                    <div className="flex items-center gap-2 min-w-0">
                      {d.document_status === "COMPLETED" ? (
                        <CheckCircle2 className="w-3.5 h-3.5 text-green-400 shrink-0" />
                      ) : d.document_status === "FAILED" ? (
                        <AlertCircle className="w-3.5 h-3.5 text-red-400 shrink-0" />
                      ) : (
                        <Loader2 className="w-3.5 h-3.5 text-yellow-400 animate-spin shrink-0" />
                      )}
                      <span className="text-xs text-foreground truncate">{displayName}</span>
                    </div>
                    <span className={cn(
                      "text-xs font-medium shrink-0 ml-3",
                      d.document_status === "COMPLETED" ? "text-green-400" :
                        d.document_status === "FAILED" ? "text-red-400" :
                          "text-yellow-400",
                    )}>
                      {d.document_status === "COMPLETED"
                        ? `Ready${d.chunks_processed ? ` · ${d.chunks_processed} ${d.chunks_processed === 1 ? "chunk" : "chunks"}` : ""}`
                        : d.document_status === "FAILED" ? "Failed"
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
