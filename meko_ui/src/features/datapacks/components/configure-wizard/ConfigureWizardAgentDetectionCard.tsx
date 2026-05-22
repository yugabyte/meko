/**
 * Static layout for future agent round-trip detection (Install wizard).
 * Preview is blurred with a ShareComingSoonPlaceholder-style overlay until detection ships.
 */
import type { ReactNode } from "react";
import { Check, Database, Shield, Terminal } from "lucide-react";
import { cn } from "@/lib/utils";

function FlowConnector({ className }: { className?: string }) {
  return (
    <div
      className={cn("h-px min-w-[12px] flex-1 shrink bg-teal-500/35", className)}
      aria-hidden
    />
  );
}

function FlowNode({
  icon: Icon,
  label,
  className,
}: {
  icon: typeof Database;
  label: string;
  className?: string;
}) {
  return (
    <div
      className={cn(
        "flex min-w-[4.5rem] flex-col items-center gap-1.5 rounded-lg border border-teal-500/35 bg-input/90 px-2.5 py-2.5 text-center shadow-sm",
        className,
      )}
    >
      <Icon className="h-4 w-4 text-teal-400" strokeWidth={1.75} aria-hidden />
      <span className="text-[11px] font-medium leading-tight text-foreground/90">{label}</span>
    </div>
  );
}

function FlowPill({ children }: { children: ReactNode }) {
  return (
    <div className="shrink-0 rounded-full border border-teal-500/40 bg-teal-500/5 px-3 py-1.5 text-center font-mono text-[11px] leading-snug text-emerald-700 dark:text-emerald-200/95">
      {children}
    </div>
  );
}

function AgentDetectionPreviewBody() {
  return (
    <div className="space-y-5">
      <div className="flex gap-3 sm:gap-4">
        <div
          className="flex h-11 w-11 shrink-0 items-center justify-center rounded-lg border border-teal-500/45 bg-input/90"
          aria-hidden
        >
          <Shield className="h-5 w-5 text-teal-400" strokeWidth={1.75} />
        </div>
        <div className="min-w-0 space-y-1.5">
          <div className="flex flex-wrap items-center gap-2 sm:gap-2.5">
            <h2 className="text-base font-semibold tracking-tight text-foreground sm:text-lg">
              Agent detected — round-trip verified
            </h2>
            <span className="inline-flex items-center gap-1 rounded-full border border-emerald-500/45 bg-emerald-500/10 px-2 py-0.5 text-[11px] font-medium text-emerald-700 dark:text-emerald-200">
              <Check className="h-3 w-3 text-emerald-400" strokeWidth={2.5} aria-hidden />
              healthy
            </span>
          </div>
          <p className="text-sm leading-relaxed text-muted-foreground">
            Meko wrote a test memory and Claude Code read it back successfully.
          </p>
        </div>
      </div>

      <div className="flex min-w-0 items-center gap-1.5 overflow-x-auto pb-1 pt-1 sm:gap-2">
        <FlowNode icon={Database} label="meko" />
        <FlowConnector />
        <FlowPill>memory.add(&quot;ping&quot;)</FlowPill>
        <FlowConnector />
        <FlowNode icon={Terminal} label="claude code" />
        <FlowConnector />
        <FlowPill>memory.search -&gt; &quot;ping&quot; ✓</FlowPill>
        <FlowConnector />
        <div className="flex min-w-[4.5rem] flex-col items-center gap-1.5 rounded-lg border border-emerald-500/35 bg-emerald-500/5 px-2.5 py-2.5 text-center shadow-sm">
          <Check className="h-4 w-4 text-emerald-400" strokeWidth={2.25} aria-hidden />
          <span className="text-[11px] font-medium leading-tight text-emerald-700 dark:text-emerald-100/95">verified</span>
        </div>
      </div>
    </div>
  );
}

export function ConfigureWizardAgentDetectionCard() {
  return (
    <div className="overflow-hidden rounded-xl border border-emerald-500/30 bg-emerald-950/[0.14] shadow-sm ring-1 ring-emerald-500/10">
      <div
        className="relative min-h-[220px] overflow-hidden"
        role="status"
        aria-label="Agent detection — coming soon"
      >
        <div className="pointer-events-none select-none px-4 py-5 blur-[3px] opacity-60 sm:px-6 sm:py-6">
          <AgentDetectionPreviewBody />
        </div>
        <div className="absolute inset-0 flex items-center justify-center bg-background/50 backdrop-blur-[2px]">
          <p className="rounded-lg border border-border bg-card px-4 py-2 text-sm font-semibold tracking-tight text-foreground shadow-sm">
            Coming Soon
          </p>
        </div>
      </div>

      <div className="flex justify-end border-t border-emerald-500/20 bg-background/30 px-4 py-3 sm:px-6">
        <span className="text-sm text-muted-foreground">Troubleshoot?</span>
      </div>
    </div>
  );
}
