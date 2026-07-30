/**
 * One column in Sync step: Knowledge (interactive) or Conversations / Skills (coming-soon mock UX).
 */
import type { LucideIcon } from "lucide-react";
import { Upload } from "lucide-react";
import type { RefObject } from "react";
import { Checkbox } from "@/components/ui/checkbox";
import { cn } from "@/lib/utils";
import { ConfigureSyncKnowledgePanel } from "./ConfigureSyncKnowledgePanel";
import type { ListedSource, SyncSectionId } from "./types";
import {
  wizardTileRootClass,
  wizardTileToneBadge,
  wizardTileToneIcon,
  wizardTileToneLink,
  type WizardSyncTone,
} from "./configureWizardTileStyles";

const CONVERSATIONS_AND_SKILLS_COMING_SOON = true;

function isComingSoonSection(section: SyncSectionId): boolean {
  if (!CONVERSATIONS_AND_SKILLS_COMING_SOON) return false;
  return section === "conversations" || section === "skills";
}

function ConversationsComingSoonMock() {
  return (
    <div className="pointer-events-none select-none space-y-4 blur-[2px] opacity-65">
      <div
        className="flex cursor-default flex-col items-center justify-center gap-2 rounded-lg border-2 border-dashed border-blue-500/40 bg-blue-500/[0.04] px-4 py-7 text-center"
        aria-hidden
      >
        <Upload className="h-7 w-7 text-blue-400" strokeWidth={1.5} />
        <p className="text-sm font-medium text-blue-700 dark:text-blue-200">Drop transcripts or click to upload</p>
        <p className="text-[11px] text-muted-foreground">json, jsonl, md</p>
      </div>
      <div>
        <p className="text-[10px] font-semibold uppercase tracking-wide text-muted-foreground">
          Detected in project
        </p>
        <div className="mt-2 rounded-md border border-border/45 bg-background/40 p-3">
          <div className="grid grid-cols-3 gap-2 text-center">
            {(["Total", "Avg msgs", "Span"] as const).map((label) => (
              <div key={label} className="space-y-1">
                <div className="mx-auto h-5 w-10 rounded bg-muted/60" />
                <p className="text-[9px] font-semibold uppercase tracking-wide text-muted-foreground">{label}</p>
              </div>
            ))}
          </div>
          <div className="mt-3 space-y-2 border-t border-border/40 pt-3 text-[11px] text-muted-foreground">
            <div className="flex justify-between">
              <span>Most recent</span>
              <span className="h-3 w-12 rounded bg-muted/50" />
            </div>
            <div className="flex justify-between">
              <span>Est. memories to extract</span>
              <span className="h-3 w-8 rounded bg-muted/50" />
            </div>
          </div>
        </div>
      </div>
      <label className="flex cursor-default items-center gap-2 text-xs text-blue-700 dark:text-blue-200/90">
        <Checkbox checked disabled />
        <span className="h-3 flex-1 max-w-[14rem] rounded bg-muted/50" />
      </label>
    </div>
  );
}

function SkillsComingSoonMock() {
  return (
    <div className="pointer-events-none select-none space-y-4 blur-[2px] opacity-65">
      <div
        className="flex flex-col items-center justify-center gap-2 rounded-lg border-2 border-dashed border-purple-500/40 bg-purple-500/[0.04] px-4 py-7 text-center"
        aria-hidden
      >
        <Upload className="h-7 w-7 text-purple-400" strokeWidth={1.5} />
        <p className="text-sm font-medium text-purple-700 dark:text-purple-200">Drop skill .md files or click to upload</p>
        <p className="text-[11px] text-muted-foreground">md only · e.g. CLAUDE.md</p>
      </div>
      <div>
        <p className="text-[10px] font-semibold uppercase tracking-wide text-muted-foreground">
          Detected in project
        </p>
        <ul className="mt-2 space-y-2">
          {[0, 1, 2].map((i) => (
            <li
              key={i}
              className="flex items-start gap-2 rounded-md border border-border/45 bg-background/40 px-2 py-2"
            >
              <Checkbox
                checked
                disabled
                className="mt-0.5 border-purple-500/50 data-[state=checked]:bg-purple-600"
              />
              <div className="min-w-0 flex-1 space-y-1.5">
                <div className="h-3 w-28 max-w-full rounded bg-muted/55" />
                <div className="h-2.5 w-40 max-w-full rounded bg-muted/40" />
              </div>
            </li>
          ))}
        </ul>
      </div>
    </div>
  );
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
  /** Knowledge: hint under drop zone */
  knowledgeExtensionsHint?: string;
  onPickFiles: (files: FileList | null) => void;
  onAddUrl: () => void;
  onRemove: (id: string) => void;
  knowledgeUploading?: boolean;
  onUploadKnowledgeFiles?: (includedIds: Set<string>) => void;
  onRetryKnowledgeUpload?: (id: string) => void;
};

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
  knowledgeExtensionsHint = "pdf, txt, md, json, csv, html",
  onPickFiles,
  onRemove,
  knowledgeUploading = false,
  onUploadKnowledgeFiles,
  onRetryKnowledgeUpload,
}: Props) {
  const comingSoon = isComingSoonSection(section);

  if (section === "knowledge" && onUploadKnowledgeFiles && accept != null) {
    return (
      <div
        className={wizardTileRootClass(
          tone,
          // No `self-start` — let this column stretch to match the row
          // height set by the grid's `items-stretch`, so Knowledge ends up
          // the same height as the Conversations / Skills columns.
          "w-full min-w-0 overflow-hidden min-h-[320px] max-h-[min(32rem,72vh)]",
        )}
      >
        <ConfigureSyncKnowledgePanel
          items={items}
          fileInputRef={fileInputRef}
          accept={accept}
          extensionsHint={knowledgeExtensionsHint}
          onPickFiles={onPickFiles}
          onRemove={onRemove}
          knowledgeUploading={knowledgeUploading}
          onUploadKnowledgeFiles={onUploadKnowledgeFiles}
          onRetryKnowledgeUpload={onRetryKnowledgeUpload ?? (() => {})}
        />
      </div>
    );
  }

  return (
    <div className={wizardTileRootClass(tone, "min-h-[320px]")}>
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
          className="relative mt-4 min-h-[260px] flex-1 overflow-hidden rounded-lg border border-dashed border-border/70 bg-muted/5"
          role="status"
          aria-label={`${title} — coming soon`}
        >
          {section === "conversations" ? <ConversationsComingSoonMock /> : <SkillsComingSoonMock />}
          <div className="absolute inset-0 flex items-center justify-center bg-background/50 backdrop-blur-[2px]">
            <p className="rounded-lg border border-border bg-card px-4 py-2 text-sm font-semibold tracking-tight text-foreground shadow-sm">
              Coming Soon
            </p>
          </div>
        </div>
      ) : null}
    </div>
  );
}
