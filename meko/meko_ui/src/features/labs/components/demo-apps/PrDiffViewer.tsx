import { ExternalLink } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";

/** Real +/- line counts and changed-file count, derived from the diff text
 *  itself -- not hardcoded, so they stay correct if the sample diff ever
 *  changes. `diff --git a/X b/Y` header lines are excluded from the +/-
 *  counts (they're metadata, not content). */
function diffStats(diff: string): { added: number; removed: number; files: number } {
  const lines = diff.split("\n");
  let added = 0;
  let removed = 0;
  let files = 0;
  for (const line of lines) {
    if (line.startsWith("diff --git ")) files += 1;
    else if (line.startsWith("+") && !line.startsWith("+++")) added += 1;
    else if (line.startsWith("-") && !line.startsWith("---")) removed += 1;
  }
  return { added, removed, files };
}

/** Static unified-diff renderer: a GitHub-PR-style header (title + optional
 *  description + a real +/- file-stat bar, all derived from the diff text
 *  itself) above +/- line coloring -- no diff library needed since this
 *  always renders a single pre-fetched diff string, never a live diffing
 *  computation. `description` is optional so a caller that only has a diff
 *  (no authored PR description) can omit it. */
export function PrDiffViewer({
  title,
  description,
  diff,
  isSample,
  url,
}: {
  title: string;
  description?: string;
  diff: string;
  /** Marks this as bundled sample content, not a real submitted PR --
   *  otherwise indistinguishable from a genuine diff now that BYOC-PR
   *  support exists (see BringYourOwnPrStage), so this needs to be explicit
   *  at every call site rather than assumed. */
  isSample?: boolean;
  /** Links out to the real PR on GitHub -- only set for a bring-your-own-PR
   *  review, where there's an actual page to link to. */
  url?: string;
}) {
  const lines = diff.split("\n");
  const stats = diffStats(diff);
  return (
    <div className="overflow-hidden rounded-md border border-border">
      <div className="border-b border-border px-3 py-2.5">
        <div className="flex items-center gap-2">
          <div className="text-sm font-medium">{title}</div>
          {isSample && (
            <Badge variant="outline" className="shrink-0">
              Sample PR
            </Badge>
          )}
          {url && (
            <a
              href={url}
              target="_blank"
              rel="noreferrer"
              className="inline-flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground"
            >
              <ExternalLink className="h-3 w-3" /> View on GitHub
            </a>
          )}
        </div>
        {description && <p className="mt-1.5 text-xs text-muted-foreground">{description}</p>}
        <div className="mt-2 flex items-center gap-3 font-mono text-xs text-muted-foreground">
          <span>
            {stats.files} file{stats.files === 1 ? "" : "s"} changed
          </span>
          <span>
            <span className="text-emerald-600 dark:text-emerald-400">+{stats.added}</span>{" "}
            <span className="text-red-600 dark:text-red-400">−{stats.removed}</span>
          </span>
        </div>
      </div>
      <pre className="max-h-64 overflow-auto p-3 font-mono text-xs leading-relaxed">
        {lines.map((line, i) => (
          <div
            key={i}
            className={cn(
              line.startsWith("+") && !line.startsWith("+++") && "bg-emerald-500/10 text-emerald-700 dark:text-emerald-300",
              line.startsWith("-") && !line.startsWith("---") && "bg-red-500/10 text-red-700 dark:text-red-300",
            )}
          >
            {line || " "}
          </div>
        ))}
      </pre>
    </div>
  );
}
