/**
 * One column in Sync Data: title, optional file list, hidden file input, and add-file / add-URL actions.
 * Skills uses `accept` and callers filter uploads; Knowledge and Conversations accept broader file types.
 */
import type { LucideIcon } from "lucide-react";
import type { RefObject } from "react";
import { cn } from "@/lib/utils";
import type { ListedSource, SyncSectionId } from "./types";

type Tone = "emerald" | "blue" | "purple";

type Props = {
  section: SyncSectionId;
  title: string;
  description: string;
  badge: string;
  icon: LucideIcon;
  tone: Tone;
  items: ListedSource[];
  fileInputRef: RefObject<HTMLInputElement | null>;
  accept: string | undefined;
  onPickFiles: (files: FileList | null) => void;
  onAddUrl: () => void;
  onRemove: (id: string) => void;
};

const toneBorder: Record<Tone, string> = {
  emerald: "border-emerald-500/35 bg-emerald-950/20",
  blue: "border-blue-500/35 bg-blue-950/20",
  purple: "border-purple-500/35 bg-purple-950/20",
};

const toneBadge: Record<Tone, string> = {
  emerald: "border-emerald-500/40 bg-emerald-500/15 text-emerald-100",
  blue: "border-blue-500/40 bg-blue-500/15 text-blue-100",
  purple: "border-purple-500/40 bg-purple-500/15 text-purple-100",
};

const toneIcon: Record<Tone, string> = {
  emerald: "text-emerald-400",
  blue: "text-blue-400",
  purple: "text-purple-400",
};

const toneLink: Record<Tone, string> = {
  emerald: "text-emerald-400 hover:text-emerald-300",
  blue: "text-blue-400 hover:text-blue-300",
  purple: "text-purple-400 hover:text-purple-300",
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
  onPickFiles,
  onAddUrl,
  onRemove,
}: Props) {
  return (
    <div
      className={cn(
        "flex min-h-[280px] min-w-0 flex-1 flex-col rounded-xl border-2 p-4 sm:p-5",
        toneBorder[tone],
      )}
    >
      <div className="flex items-start justify-between gap-2 border-b border-border/60 pb-3">
        <div className="flex min-w-0 items-center gap-2">
          <Icon className={cn("h-5 w-5 shrink-0", toneIcon[tone])} aria-hidden />
          <h3 className="truncate text-sm font-semibold text-foreground">{title}</h3>
        </div>
        <span
          className={cn(
            "shrink-0 rounded-full border px-2 py-0.5 text-[10px] font-semibold uppercase tracking-wide",
            toneBadge[tone],
          )}
        >
          {badge}
        </span>
      </div>
      <p className="mt-3 text-xs leading-relaxed text-muted-foreground">{description}</p>

      {/* Triggered programmatically from “+ Add files” so we can style the tile without nesting a raw input. */}
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
          items.map((it) => (
            <li
              key={it.id}
              className="flex items-center justify-between gap-2 rounded-md bg-background/40 px-2 py-1.5 font-mono text-muted-foreground"
            >
              <span className="min-w-0 truncate" title={it.label}>
                {it.label}
              </span>
              <button
                type="button"
                className="shrink-0 text-xs text-muted-foreground hover:text-foreground"
                onClick={() => onRemove(it.id)}
              >
                Remove
              </button>
            </li>
          ))
        )}
      </ul>

      <div className="mt-4 flex flex-wrap gap-x-3 gap-y-1 border-t border-border/50 pt-3 text-xs">
        <button
          type="button"
          className={cn("font-medium hover:underline", toneLink[tone])}
          onClick={() => fileInputRef.current?.click()}
        >
          + Add files
        </button>
        <button type="button" className={cn("font-medium hover:underline", toneLink[tone])} onClick={onAddUrl}>
          + Add URL
        </button>
      </div>
    </div>
  );
}
