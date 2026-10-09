// Textarea + upload + inline parse summary for the bulk-invite tab.
//
// Owns nothing beyond ephemeral file-input state — the parsed emails
// list lives on the parent so its submit button can validate the same
// value the summary line describes.
import { useRef } from "react";
import { Upload } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/textarea";
import { toast } from "@/hooks/use-toast";
import { cn } from "@/lib/utils";
import { DATAPACK_BULK_SHARE_MAX_EMAILS } from "@/lib/api/mekoApi";
import {
  describeBulkShareParse,
  type BulkShareEmailParse,
} from "@/features/datapacks/components/datapack-share/bulkShareEmails";

/** How many skipped rows to name inline before collapsing to a count. */
const SKIPPED_PREVIEW_LIMIT = 5;

export type BulkInviteEmailInputProps = {
  text: string;
  onTextChange: (next: string) => void;
  onFileAdded: (name: string) => void;
  disabled: boolean;
  parse: BulkShareEmailParse;
  /** Post-self-filter list — passed in so the "n emails" summary matches what the submit button will actually POST. */
  emails: string[];
  selfSkipped: boolean;
  overCap: boolean;
  fileName: string | null;
  onClear: () => void;
};

export function BulkInviteEmailInput({
  text,
  onTextChange,
  onFileAdded,
  disabled,
  parse,
  emails,
  selfSkipped,
  overCap,
  fileName,
  onClear,
}: BulkInviteEmailInputProps) {
  const fileInputRef = useRef<HTMLInputElement>(null);

  const handleFile = async (file: File | undefined) => {
    if (!file) return;
    try {
      const content = await file.text();
      // Append rather than replace so a paste + a file can be combined; dedup
      // happens in parseBulkShareEmails.
      onTextChange(text.trim() ? `${text.replace(/\s*$/, "")}\n${content}` : content);
      onFileAdded(file.name);
    } catch {
      toast({
        variant: "destructive",
        title: "Could not read file",
        description: "That file could not be read as text. Export it as CSV and try again.",
      });
    } finally {
      // Reset so re-selecting the same file fires onChange again.
      if (fileInputRef.current) fileInputRef.current.value = "";
    }
  };

  return (
    <div className="space-y-2">
      <label htmlFor="bulk-share-emails" className="text-xs font-medium text-foreground">
        Email addresses
      </label>
      <Textarea
        id="bulk-share-emails"
        value={text}
        onChange={(e) => onTextChange(e.target.value)}
        disabled={disabled}
        rows={6}
        spellCheck={false}
        placeholder={"email\nana@company.com\nbo@company.com"}
        className="min-h-[132px] resize-y bg-background/80 font-mono text-xs"
      />
      <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
        <label
          htmlFor="bulk-share-csv-file"
          className={cn(
            "inline-flex cursor-pointer items-center gap-2 rounded-md border border-border bg-background/80 px-3 py-1.5 text-xs font-medium text-foreground transition-colors hover:bg-muted/40",
            disabled && "pointer-events-none opacity-60",
          )}
        >
          <Upload className="h-3.5 w-3.5" aria-hidden />
          Upload CSV
        </label>
        <input
          ref={fileInputRef}
          id="bulk-share-csv-file"
          type="file"
          accept=".csv,.txt,text/csv,text/plain"
          className="sr-only"
          disabled={disabled}
          onChange={(e) => void handleFile(e.target.files?.[0])}
        />
        {fileName ? (
          <span className="truncate text-xs text-muted-foreground">Added {fileName}</span>
        ) : null}
        <span className="text-xs text-muted-foreground" aria-live="polite">
          {text.trim()
            ? [
                describeBulkShareParse({ ...parse, emails }),
                selfSkipped ? "your own address skipped" : null,
              ]
                .filter(Boolean)
                .join(" · ")
            : `Up to ${DATAPACK_BULK_SHARE_MAX_EMAILS} addresses per batch`}
        </span>
        {text.trim() ? (
          <Button
            type="button"
            variant="ghost"
            size="sm"
            className="h-7 px-2 text-xs text-muted-foreground hover:text-foreground"
            disabled={disabled}
            onClick={onClear}
          >
            Clear
          </Button>
        ) : null}
      </div>
      {parse.skippedRows.length > 0 ? (
        <p className="text-xs text-amber-700 dark:text-amber-300">
          Skipped (no valid email): {parse.skippedRows.slice(0, SKIPPED_PREVIEW_LIMIT).join(" · ")}
          {parse.skippedRows.length > SKIPPED_PREVIEW_LIMIT
            ? ` · +${parse.skippedRows.length - SKIPPED_PREVIEW_LIMIT} more`
            : ""}
        </p>
      ) : null}
      {parse.droppedFragments.length > 0 ? (
        // Fragments come from tokens that failed the single-email regex on
        // a row that still had at least one valid address — usually a typo
        // like "bob@x. com" splitting into "bob@x." and "com". Surface them
        // so the user notices the counter dropped from 2 to 1 without
        // having to spot the plural change on the submit button.
        <p className="text-xs text-amber-700 dark:text-amber-300">
          Dropped fragments (looks like typos):{" "}
          {parse.droppedFragments.slice(0, SKIPPED_PREVIEW_LIMIT).join(" · ")}
          {parse.droppedFragments.length > SKIPPED_PREVIEW_LIMIT
            ? ` · +${parse.droppedFragments.length - SKIPPED_PREVIEW_LIMIT} more`
            : ""}
        </p>
      ) : null}
      {overCap ? (
        <p className="text-xs text-destructive">
          {emails.length} addresses is over the {DATAPACK_BULK_SHARE_MAX_EMAILS}-per-batch limit.
          Split the list and run it twice.
        </p>
      ) : null}
    </div>
  );
}
