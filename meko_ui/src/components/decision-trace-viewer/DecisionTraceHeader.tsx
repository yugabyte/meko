// Header card sitting at the top of the expanded Decision Trace.
// Surfaces the four chip totals (total / tok / db / llm), the
// trace_id with a copy button, and an optional Open-in-Langfuse
// deep-link.

import { useEffect, useRef, useState } from "react";
import {
  Check,
  Clock,
  Copy,
  Cpu,
  Database,
  ExternalLink,
  Hash,
} from "lucide-react";
import { ROUTES } from "@/app/routes/paths";
import { cn } from "@/lib/utils";
import { copyToClipboard } from "@/lib/clipboard";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { formatDuration } from "./helpers";

export type DbBreakdownEntry = {
  name: string;
  prettyName: string;
  count: number;
  ms: number;
};

export function DecisionTraceHeader({
  totalMs,
  totalTokens,
  dbCalls,
  dbBreakdown,
  llmCalls,
  conversationId,
  langfuseProjectId,
}: {
  totalMs: number;
  totalTokens: number;
  dbCalls: number;
  dbBreakdown: DbBreakdownEntry[];
  llmCalls: number;
  conversationId: string;
  langfuseProjectId?: string;
}) {
  const traceUrl =
    langfuseProjectId && conversationId
      ? `${ROUTES.observeHub}?${new URLSearchParams({
          project: langfuseProjectId,
          session: conversationId.replace(/-/g, ""),
        }).toString()}`
      : "";

  const [copied, setCopied] = useState(false);
  const copyResetTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  useEffect(() => {
    return () => {
      if (copyResetTimer.current) clearTimeout(copyResetTimer.current);
    };
  }, []);
  const handleCopy = async (e: React.MouseEvent) => {
    e.stopPropagation();
    const ok = await copyToClipboard(conversationId);
    if (!ok) return;
    setCopied(true);
    if (copyResetTimer.current) clearTimeout(copyResetTimer.current);
    copyResetTimer.current = setTimeout(() => setCopied(false), 1400);
  };

  const dbTooltip = (
    <div>
      <span className="mb-1 block text-[11px] font-medium text-blue-200">
        DB calls
      </span>
      {dbBreakdown.length === 0 ? (
        <span className="block text-[10px] text-blue-100/75">None.</span>
      ) : (
        <span className="block space-y-0.5">
          {dbBreakdown.map((b) => (
            <span
              key={b.name}
              className="flex items-center justify-between gap-3"
            >
              <code className="font-mono text-[10px] text-blue-100/90">
                {b.prettyName}
              </code>
              <span className="tabular-nums text-[10px] text-blue-100/70">
                ×{b.count}
                {b.ms > 0 ? ` · ${formatDuration(b.ms)}` : ""}
              </span>
            </span>
          ))}
        </span>
      )}
    </div>
  );

  return (
    <div className="rounded-md border border-purple-500/15 bg-purple-500/[0.02] px-3 py-2">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-[11px]">
        <HeaderChip
          icon={<Clock className="h-3 w-3 text-purple-600 dark:text-purple-300" aria-hidden />}
          value={formatDuration(totalMs)}
          label="total"
        />
        <HeaderChip
          icon={<Hash className="h-3 w-3 text-amber-600 dark:text-amber-300" aria-hidden />}
          value={totalTokens.toLocaleString()}
          label="tok"
        />
        <HeaderChip
          icon={<Database className="h-3 w-3 text-blue-600 dark:text-blue-300" aria-hidden />}
          value={dbCalls.toString()}
          label="db"
          tooltip={dbTooltip}
        />
        <HeaderChip
          icon={<Cpu className="h-3 w-3 text-emerald-600 dark:text-emerald-300" aria-hidden />}
          value={llmCalls.toString()}
          label="llm"
        />
        <div className="ml-auto flex items-center gap-1.5">
          <span className="text-muted-foreground">trace_id</span>
          <code
            className="font-mono text-foreground/80 truncate max-w-[14ch]"
            title={conversationId}
          >
            {conversationId}
          </code>
          <button
            type="button"
            onClick={handleCopy}
            className={cn(
              "inline-flex cursor-pointer items-center justify-center rounded p-0.5 transition-colors",
              copied
                ? "text-emerald-600 dark:text-emerald-300"
                : "text-muted-foreground hover:bg-purple-500/10 hover:text-foreground",
            )}
            aria-label={copied ? "Copied trace id" : "Copy trace id"}
            title={copied ? "Copied" : "Copy trace id"}
          >
            {copied ? (
              <Check className="h-3 w-3" aria-hidden />
            ) : (
              <Copy className="h-3 w-3" aria-hidden />
            )}
          </button>
          {traceUrl ? (
            <a
              href={traceUrl}
              target="_blank"
              rel="noreferrer noopener"
              className="inline-flex items-center gap-1 rounded px-1.5 py-0.5 text-purple-700 dark:text-purple-200 hover:bg-purple-500/15 hover:text-purple-800 dark:hover:text-purple-100 transition-colors"
              title="Open this conversation in the Observe Hub (new tab)"
            >
              <ExternalLink className="h-3 w-3" aria-hidden />
              Open
            </a>
          ) : null}
        </div>
      </div>
    </div>
  );
}

function HeaderChip({
  icon,
  value,
  label,
  tooltip,
}: {
  icon: React.ReactNode;
  value: string;
  label: string;
  tooltip?: React.ReactNode;
}) {
  const chip = (
    <span
      className={cn(
        "inline-flex items-center gap-1 tabular-nums",
        tooltip ? "cursor-help" : null,
      )}
    >
      {icon}
      <span className="font-mono text-foreground/90">{value}</span>
      <span
        className={cn(
          "text-muted-foreground",
          tooltip
            ? "border-b border-dashed border-muted-foreground/40"
            : null,
        )}
      >
        {label}
      </span>
    </span>
  );

  if (!tooltip) return chip;

  return (
    <Tooltip delayDuration={0}>
      <TooltipTrigger asChild>{chip}</TooltipTrigger>
      <TooltipContent
        side="top"
        align="start"
        sideOffset={8}
        collisionPadding={12}
        className="z-[90] w-[280px] rounded-lg border border-blue-400/20 bg-[#0A1222]/95 px-2.5 py-2 text-left text-[10px] text-blue-100 shadow-lg"
      >
        {tooltip}
      </TooltipContent>
    </Tooltip>
  );
}
