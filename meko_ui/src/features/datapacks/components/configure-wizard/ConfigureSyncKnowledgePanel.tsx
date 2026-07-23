/**
 * Knowledge column: header + drop zone, detected list with inclusion checkboxes, upload CTA, disabled add-URL link.
 */
import { useEffect, useMemo, useState, type DragEvent } from "react";
import { AlertCircle, Book, FileText, Upload } from "lucide-react";
import type { RefObject } from "react";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { formatFileSize } from "@/lib/fileSize";
import { KNOWLEDGE_UPLOAD_STATUS } from "@/lib/knowledgeUploadStatus";
import { cn } from "@/lib/utils";
import type { ListedSource } from "./types";
import { wizardTileToneBadge } from "./configureWizardTileStyles";

const ADD_URL_TOOLTIP = "Coming soon";

function isPendingOrError(it: ListedSource): boolean {
  if (it.kind !== "file" || !it.file) return false;
  const s = it.uploadStatus;
  return !s || s === KNOWLEDGE_UPLOAD_STATUS.PENDING || s === KNOWLEDGE_UPLOAD_STATUS.ERROR;
}

type Props = {
  items: ListedSource[];
  fileInputRef: RefObject<HTMLInputElement | null>;
  accept: string;
  /** Shown under drop zone, e.g. "pdf, txt, md, json, mp4" */
  extensionsHint: string;
  onPickFiles: (files: FileList | null) => void;
  onRemove: (id: string) => void;
  knowledgeUploading: boolean;
  onUploadKnowledgeFiles: (includedIds: Set<string>) => void;
  onRetryKnowledgeUpload: (id: string) => void;
};

export function ConfigureSyncKnowledgePanel({
  items,
  fileInputRef,
  accept,
  extensionsHint,
  onPickFiles,
  onRemove,
  knowledgeUploading,
  onUploadKnowledgeFiles,
  onRetryKnowledgeUpload,
}: Props) {
  const [includedIds, setIncludedIds] = useState<Set<string>>(() => new Set());
  const [dragOver, setDragOver] = useState(false);

  const idSignature = useMemo(() => items.map((i) => i.id).sort().join("\0"), [items]);

  useEffect(() => {
    setIncludedIds(new Set(items.map((i) => i.id)));
  }, [idSignature]);

  const toggleInclude = (id: string) => {
    setIncludedIds((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  const pendingForUpload = items.filter(
    (it) =>
      it.kind === "file" &&
      it.file &&
      includedIds.has(it.id) &&
      (it.uploadStatus === KNOWLEDGE_UPLOAD_STATUS.PENDING ||
        it.uploadStatus === KNOWLEDGE_UPLOAD_STATUS.ERROR),
  );

  const hasKnowledgeUploadErrors = items.some(
    (it) => it.kind === "file" && it.uploadStatus === KNOWLEDGE_UPLOAD_STATUS.ERROR,
  );

  const showUploadCta = pendingForUpload.length > 0;
  const selectedCount = items.filter((it) => includedIds.has(it.id)).length;
  const badgeLabel = items.length === 0 ? "0/0" : `${selectedCount}/${items.length}`;

  const openPicker = () => fileInputRef.current?.click();

  const onDrop = (e: DragEvent) => {
    e.preventDefault();
    setDragOver(false);
    if (e.dataTransfer.files?.length) onPickFiles(e.dataTransfer.files);
  };

  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-hidden">
      <div className="shrink-0">
        <div className="flex items-start justify-between gap-2 border-b border-border/60 pb-3">
          <div className="flex min-w-0 items-center gap-2">
            <Book className="h-5 w-5 shrink-0 text-emerald-400" aria-hidden />
            <h3 className="truncate text-sm font-semibold text-foreground">Knowledge</h3>
          </div>
          <span
            className={cn(
              "shrink-0 rounded-full border px-2.5 py-0.5 text-xs font-semibold tabular-nums tracking-normal",
              wizardTileToneBadge.emerald,
            )}
          >
            {badgeLabel}
          </span>
        </div>

        <input
          ref={fileInputRef}
          type="file"
          className="sr-only"
          accept={accept}
          multiple
          onChange={(e) => {
            onPickFiles(e.target.files);
            e.target.value = "";
          }}
          aria-label="Upload knowledge files"
        />

        <p className="mt-3 text-xs leading-relaxed text-muted-foreground">
          Files we&apos;ll index and chunk for retrieval.
        </p>

        <div
          role="button"
          tabIndex={0}
          onKeyDown={(e) => {
            if (e.key === "Enter" || e.key === " ") {
              e.preventDefault();
              openPicker();
            }
          }}
          onClick={openPicker}
          onDragOver={(e) => {
            e.preventDefault();
            setDragOver(true);
          }}
          onDragLeave={() => setDragOver(false)}
          onDrop={onDrop}
          className={cn(
            "mt-4 flex cursor-pointer flex-col items-center justify-center gap-2 rounded-lg border-2 border-dashed px-4 py-8 text-center transition-colors",
            "border-emerald-500/45 bg-emerald-500/[0.04] hover:border-emerald-400/60 hover:bg-emerald-500/[0.07]",
            dragOver && "border-emerald-400 bg-emerald-500/15",
          )}
        >
          <Upload className="h-8 w-8 text-emerald-400" strokeWidth={1.5} aria-hidden />
          <p className="text-sm font-medium text-emerald-700 dark:text-emerald-200">Drop files or click to upload</p>
          <p className="text-[11px] text-muted-foreground">{extensionsHint}</p>
        </div>
      </div>

      <div className="mt-5 flex min-h-0 flex-1 flex-col overflow-hidden">
        <div
          className="min-h-0 flex-1 overflow-y-auto overscroll-y-contain pb-1 [-webkit-overflow-scrolling:touch]"
          aria-label="Staged knowledge files"
        >
          <p className="text-[10px] font-semibold uppercase tracking-wide text-muted-foreground">
            Detected in project
          </p>
          <ul className="mt-2 space-y-1.5">
          {items.length === 0 ? (
            <li className="rounded-md border border-border/50 bg-background/30 px-3 py-6 text-center text-xs text-muted-foreground">
              No files yet — add files above.
            </li>
          ) : (
            items.map((it) => {
              const short =
                it.kind === "file" && it.file
                  ? (() => {
                      const s = it.uploadStatus;
                      if (!s || s === KNOWLEDGE_UPLOAD_STATUS.PENDING) return "Not uploaded";
                      if (s === KNOWLEDGE_UPLOAD_STATUS.UPLOADING)
                        return `Uploading${it.uploadProgress != null ? ` ${it.uploadProgress}%` : "…"}`;
                      if (s === KNOWLEDGE_UPLOAD_STATUS.DONE) return it.uploadNote ?? "Uploaded";
                      if (s === KNOWLEDGE_UPLOAD_STATUS.SKIPPED) return it.uploadNote ?? "Skipped";
                      if (s === KNOWLEDGE_UPLOAD_STATUS.ERROR) return "Upload failed";
                      return null;
                    })()
                  : null;
              const failed = it.kind === "file" && it.uploadStatus === KNOWLEDGE_UPLOAD_STATUS.ERROR;
              const detail = failed && it.uploadError ? it.uploadError : short ?? undefined;
              const sizeLabel =
                it.kind === "file" ? formatFileSize(it.file?.size ?? it.sizeBytes ?? 0) : "—";
              const canToggle = it.kind === "file" && isPendingOrError(it);
              const checked = includedIds.has(it.id);

              return (
                <li
                  key={it.id}
                  className={cn(
                    "rounded-lg border p-2.5",
                    failed
                      ? "border-border/55 bg-muted/15 ring-1 ring-inset ring-destructive/20"
                      : "border-border/50 bg-background/35",
                  )}
                >
                  <div className="flex gap-2.5">
                    <div className="pt-0.5">
                      {canToggle ? (
                        <Checkbox
                          checked={checked}
                          onCheckedChange={() => toggleInclude(it.id)}
                          aria-label={`Include ${it.label}`}
                        />
                      ) : (
                        <Checkbox
                          checked
                          disabled
                          className="opacity-70"
                          aria-label={`${it.label} locked`}
                        />
                      )}
                    </div>
                    <FileText className="mt-0.5 h-4 w-4 shrink-0 text-muted-foreground" aria-hidden />
                    <div className="min-w-0 flex-1 space-y-1.5">
                      <div className="flex items-start justify-between gap-2">
                        <p className="truncate text-xs font-medium text-foreground" title={it.label}>
                          {it.label}
                        </p>
                        {!failed ? (
                          <button
                            type="button"
                            className="shrink-0 text-[10px] text-muted-foreground underline-offset-2 hover:text-foreground hover:underline"
                            onClick={() => onRemove(it.id)}
                          >
                            Remove
                          </button>
                        ) : null}
                      </div>
                      <div className="flex flex-wrap items-center gap-x-2 gap-y-0.5 text-[10px]">
                        <span className="tabular-nums text-muted-foreground">{sizeLabel}</span>
                        {short ? (
                          failed && detail ? (
                            <Tooltip>
                              <TooltipTrigger asChild>
                                <span className="cursor-default font-medium text-destructive/95" tabIndex={0}>
                                  {short}
                                </span>
                              </TooltipTrigger>
                              <TooltipContent side="top" className="max-w-xs text-xs">
                                {detail}
                              </TooltipContent>
                            </Tooltip>
                          ) : (
                            <span className="text-muted-foreground/90">{short}</span>
                          )
                        ) : null}
                      </div>
                      {failed ? (
                        <div className="flex flex-wrap items-center gap-2 pt-0.5">
                          <Button
                            type="button"
                            variant="outline"
                            size="sm"
                            className="h-7 border-border/70 text-xs text-foreground hover:bg-muted/60"
                            disabled={knowledgeUploading}
                            onClick={() => onRetryKnowledgeUpload(it.id)}
                          >
                            Try again
                          </Button>
                          <button
                            type="button"
                            className="text-[10px] text-muted-foreground underline-offset-2 hover:text-foreground hover:underline"
                            onClick={() => onRemove(it.id)}
                          >
                            Remove
                          </button>
                        </div>
                      ) : null}
                    </div>
                  </div>
                </li>
              );
            })
          )}
          </ul>

          {hasKnowledgeUploadErrors ? (
            <div
              className="mt-3 flex gap-2 rounded-lg border border-border/55 bg-muted/20 px-3 py-2.5 text-xs leading-snug text-muted-foreground"
              role="alert"
            >
              <AlertCircle className="mt-0.5 h-4 w-4 shrink-0 text-destructive/75" aria-hidden />
              <span>
                Some uploads failed. Use <span className="font-medium text-foreground">Try again</span> on a row, then{" "}
                <span className="font-medium text-foreground">Upload files</span>, or remove and re-add the file.
              </span>
            </div>
          ) : null}
        </div>

        <div className="shrink-0 space-y-3 border-t border-border/50 pt-3">
          {showUploadCta ? (
            <Button
              type="button"
              variant="success"
              className="w-full font-semibold shadow-sm disabled:opacity-60"
              disabled={knowledgeUploading || pendingForUpload.length === 0}
              onClick={() => onUploadKnowledgeFiles(new Set(includedIds))}
            >
              {knowledgeUploading
                ? "Uploading…"
                : hasKnowledgeUploadErrors
                  ? "Retry upload"
                  : "Upload files"}
            </Button>
          ) : null}

          <div>
            <Tooltip>
              <TooltipTrigger asChild>
                <span className="inline-flex w-full justify-center sm:justify-start">
                  <button type="button" disabled className="cursor-not-allowed text-sm text-muted-foreground/50">
                    + Add URL or external source
                  </button>
                </span>
              </TooltipTrigger>
              <TooltipContent side="top">{ADD_URL_TOOLTIP}</TooltipContent>
            </Tooltip>
          </div>
        </div>
      </div>
    </div>
  );
}
