import { cn } from "@/lib/utils";

/** Static unified-diff renderer: +/- line coloring, no diff library needed
 *  since this always renders a single pre-fetched diff string, never a
 *  live diffing computation. */
export function PrDiffViewer({ title, diff }: { title: string; diff: string }) {
  const lines = diff.split("\n");
  return (
    <div className="overflow-hidden rounded-md border border-border">
      <div className="border-b border-border bg-muted/40 px-3 py-2 text-sm font-medium">{title}</div>
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
