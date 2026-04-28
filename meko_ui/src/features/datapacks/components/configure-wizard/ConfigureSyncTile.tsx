/**
 * One column in Sync Knowledge: title, file list or coming-soon placeholder, and actions.
 * Knowledge supports staged uploads to S3; Conversations and Skills use a blurred “Coming soon” body.
 */
import { AlertCircle, type LucideIcon } from "lucide-react";
import type { RefObject } from "react";
import { Button } from "@/components/ui/button";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { KNOWLEDGE_UPLOAD_STATUS } from "@/lib/knowledgeUploadStatus";
import { cn } from "@/lib/utils";
import type { ListedSource, SyncSectionId } from "./types";
import {
  wizardTileRootClass,
  wizardTileToneBadge,
  wizardTileToneIcon,
  wizardTileToneLink,
  type WizardSyncTone,
} from "./configureWizardTileStyles";

/**
 * Flip here when shipping each feature (no prop drilling from parents).
 * - Conversations / Skills: LinkSharing-style placeholder until sync UI exists.
 * - Knowledge “Add URL”: disabled with tooltip until URL ingest exists.
 */
const CONVERSATIONS_AND_SKILLS_COMING_SOON = true;
const KNOWLEDGE_ADD_URL_COMING_SOON = true;
const KNOWLEDGE_ADD_URL_TOOLTIP = "Coming soon";

function isComingSoonSection(section: SyncSectionId): boolean {
  if (!CONVERSATIONS_AND_SKILLS_COMING_SOON) return false;
  return section === "conversations" || section === "skills";
}

type Props = {
  section: SyncSectionId;
  title: string;
  description: string;
  badge: string;
  icon: LucideIcon;
  tone: WizardSyncTone;
  items: ListedSource[];
  fileInputRef: RefObject<HTMLInputElement | null>;
  accept: string | undefined;
  onPickFiles: (files: FileList | null) => void;
  onAddUrl: () => void;
  onRemove: (id: string) => void;
  /** Knowledge: show Upload files CTA when there are pending local files. */
  showKnowledgeUpload?: boolean;
  knowledgeUploading?: boolean;
  onUploadKnowledgeFiles?: () => void;
  /** Reset a failed knowledge row to pending so the user can run Upload files again. */
  onRetryKnowledgeUpload?: (id: string) => void;
};

function uploadStatusShort(it: ListedSource): string | null {
  if (it.kind !== "file" || !it.file) return null;
  const s = it.uploadStatus;
  if (!s || s === KNOWLEDGE_UPLOAD_STATUS.PENDING) return "Not uploaded";
  if (s === KNOWLEDGE_UPLOAD_STATUS.UPLOADING)
    return `Uploading${it.uploadProgress != null ? ` ${it.uploadProgress}%` : "…"}`;
  if (s === KNOWLEDGE_UPLOAD_STATUS.DONE) return "Uploaded";
  if (s === KNOWLEDGE_UPLOAD_STATUS.ERROR) return "Upload failed";
  return null;
}

export function ConfigureSyncTile({
  section,
  title,
  description,
  badge,
  icon: Icon,
  tone,
  items,
  fileInputRef,
  accept,
  onPickFiles,
  onAddUrl,
  onRemove,
  showKnowledgeUpload = false,
  knowledgeUploading = false,
  onUploadKnowledgeFiles,
  onRetryKnowledgeUpload,
}: Props) {
  const comingSoon = isComingSoonSection(section);
  const addUrlDisabled = section === "knowledge" && KNOWLEDGE_ADD_URL_COMING_SOON;

  const hasKnowledgeUploadErrors =
    !comingSoon && items.some((it) => it.kind === "file" && it.uploadStatus === KNOWLEDGE_UPLOAD_STATUS.ERROR);

  return (
    <div className={wizardTileRootClass(tone)}>
      <div className="flex items-start justify-between gap-2 border-b border-border/60 pb-3">
        <div className="flex min-w-0 items-center gap-2">
          <Icon className={cn("h-5 w-5 shrink-0", wizardTileToneIcon[tone])} aria-hidden />
          <h3 className="truncate text-sm font-semibold text-foreground">{title}</h3>
        </div>
        {comingSoon ? (
          <span className="shrink-0 rounded-full border border-border bg-muted/40 px-2 py-0.5 text-[10px] font-semibold uppercase tracking-wide text-muted-foreground">
            Coming Soon
          </span>
        ) : (
          <span
            className={cn(
              "shrink-0 rounded-full border px-2 py-0.5 text-[10px] font-semibold uppercase tracking-wide",
              wizardTileToneBadge[tone],
            )}
          >
            {badge}
          </span>
        )}
      </div>
      <p className="mt-3 text-xs leading-relaxed text-muted-foreground">{description}</p>

      {comingSoon ? (
        <div
          className="relative mt-4 min-h-[160px] flex-1 overflow-hidden rounded-lg border border-dashed border-border bg-muted/5"
          role="status"
          aria-label={`${title} — coming soon`}
        >
          <div className="pointer-events-none select-none px-3 py-4 blur-[3px] opacity-60">
            <ul className="space-y-2 text-[11px] text-muted-foreground">
              <li className="rounded-md bg-background/40 px-2 py-2 font-mono">example-export.json</li>
              <li className="rounded-md bg-background/40 px-2 py-2 font-mono">notes-from-session.md</li>
            </ul>
            <div className="mt-4 flex gap-3 border-t border-border/40 pt-3">
              <span className={cn("font-medium", wizardTileToneLink[tone])}>+ Add files</span>
              <span className={cn("font-medium", wizardTileToneLink[tone])}>+ Add URL</span>
            </div>
          </div>
          <div className="absolute inset-0 flex items-center justify-center bg-background/50 backdrop-blur-[2px]">
            <p className="rounded-lg border border-border bg-card px-4 py-2 text-sm font-semibold tracking-tight text-foreground shadow-sm">
              Coming Soon
            </p>
          </div>
        </div>
      ) : (
        <>
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
            aria-label={`Upload files for ${section}`}
          />

          <ul className="mt-4 min-h-0 flex-1 space-y-1.5 overflow-y-auto text-[11px]">
            {items.length === 0 ? (
              <li className="text-muted-foreground/80">No files yet.</li>
            ) : (
              items.map((it) => {
                const short = uploadStatusShort(it);
                const failed = it.kind === "file" && it.uploadStatus === KNOWLEDGE_UPLOAD_STATUS.ERROR;
                const detail = failed && it.uploadError ? it.uploadError : short ?? undefined;
                return (
                  <li
                    key={it.id}
                    className={cn(
                      "flex flex-col gap-1.5 rounded-md px-2 py-1.5 font-mono text-muted-foreground sm:flex-row sm:items-center sm:justify-between sm:gap-2",
                      failed
                        ? "border border-destructive/35 bg-destructive/10"
                        : "bg-background/40",
                    )}
                  >
                    <span className="flex min-w-0 items-center gap-1.5 truncate">
                      {failed ? <AlertCircle className="h-3.5 w-3.5 shrink-0 text-destructive" aria-hidden /> : null}
                      <span className="min-w-0 truncate" title={it.label}>
                        {it.label}
                      </span>
                    </span>
                    <div className="flex shrink-0 flex-wrap items-center justify-end gap-2">
                      {short ? (
                        failed && detail ? (
                          <Tooltip>
                            <TooltipTrigger asChild>
                              <span
                                className="cursor-default text-[10px] font-medium text-destructive"
                                tabIndex={0}
                              >
                                {short}
                              </span>
                            </TooltipTrigger>
                            <TooltipContent side="top" className="max-w-xs text-xs">
                              {detail}
                            </TooltipContent>
                          </Tooltip>
                        ) : (
                          <span className="text-[10px] text-muted-foreground/90">{short}</span>
                        )
                      ) : null}
                      {failed && onRetryKnowledgeUpload ? (
                        <Button
                          type="button"
                          variant="outline"
                          size="sm"
                          className="h-7 border-destructive/40 text-xs text-destructive hover:bg-destructive/15 hover:text-destructive"
                          disabled={knowledgeUploading}
                          onClick={() => onRetryKnowledgeUpload(it.id)}
                        >
                          Try again
                        </Button>
                      ) : null}
                      <button
                        type="button"
                        className="text-xs text-muted-foreground hover:text-foreground"
                        onClick={() => onRemove(it.id)}
                      >
                        Remove
                      </button>
                    </div>
                  </li>
                );
              })
            )}
          </ul>

          {hasKnowledgeUploadErrors ? (
            <div
              className="mt-3 rounded-lg border border-destructive/30 bg-destructive/10 px-3 py-2 text-xs leading-snug text-destructive"
              role="alert"
            >
              One or more files could not be uploaded. Use <span className="font-semibold">Try again</span> on a row,
              then <span className="font-semibold">Upload files</span>, or remove the file and add it again.
            </div>
          ) : null}

          <div className="mt-4 flex flex-col gap-3 border-t border-border/50 pt-3">
            <div className="flex flex-wrap items-center gap-2 sm:gap-3">
              <Button
                type="button"
                size="default"
                className={cn(
                  "min-h-9 font-semibold shadow-sm",
                  tone === "emerald" && "bg-emerald-600 text-white hover:bg-emerald-500",
                  tone === "blue" && "bg-blue-600 text-white hover:bg-blue-500",
                  tone === "purple" && "bg-purple-600 text-white hover:bg-purple-500",
                )}
                onClick={() => fileInputRef.current?.click()}
              >
                Add files
              </Button>
              {addUrlDisabled ? (
                <Tooltip>
                  <TooltipTrigger asChild>
                    <span className="inline-flex">
                      <Button type="button" size="sm" variant="ghost" disabled className="text-muted-foreground">
                        Add URL
                      </Button>
                    </span>
                  </TooltipTrigger>
                  <TooltipContent side="top">{KNOWLEDGE_ADD_URL_TOOLTIP}</TooltipContent>
                </Tooltip>
              ) : (
                <Button type="button" size="sm" variant="outline" onClick={onAddUrl}>
                  Add URL
                </Button>
              )}
            </div>
            {showKnowledgeUpload && onUploadKnowledgeFiles ? (
              <Button
                type="button"
                className="w-full bg-emerald-600 font-semibold text-white shadow-sm hover:bg-emerald-500 disabled:opacity-60 sm:w-auto"
                disabled={knowledgeUploading}
                onClick={onUploadKnowledgeFiles}
              >
                {knowledgeUploading
                  ? "Uploading…"
                  : hasKnowledgeUploadErrors
                    ? "Retry upload"
                    : "Upload files"}
              </Button>
            ) : null}
          </div>
        </>
      )}
    </div>
  );
}
