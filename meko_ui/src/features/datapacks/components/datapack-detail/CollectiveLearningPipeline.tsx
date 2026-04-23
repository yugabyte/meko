/**
 * Overview-tab “Collective learning pipeline” panel: section header, optional configure affordance,
 * and a vertical ME → Collective → KO flow with color-coded stages and extract/promote connectors.
 */
import { ArrowDown, ChevronRight } from "lucide-react";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

type StageTone = "memory" | "collective" | "knowledge";

type PipelineStageProps = {
  tone: StageTone;
  title: string;
  value: number;
  subtitle: string;
  /** e.g. "auto" badge on collective */
  cornerBadge?: string;
};

function PipelineStage({ tone, title, value, subtitle, cornerBadge }: PipelineStageProps) {
  const toneStyles: Record<StageTone, string> = {
    memory: "border-blue-500/40 bg-gradient-to-br from-blue-950/70 via-[#0f1729] to-[#0b1220]",
    collective: "border-purple-500/40 bg-gradient-to-br from-purple-950/65 via-[#141228] to-[#0b1220]",
    knowledge: "border-emerald-900/50 bg-gradient-to-br from-emerald-950/55 via-[#0c1814] to-[#0b1220]",
  };

  const titleTone: Record<StageTone, string> = {
    memory: "text-blue-200/95",
    collective: "text-purple-200/95",
    knowledge: "text-emerald-200/95",
  };

  return (
    <div
      className={cn(
        "relative w-full rounded-2xl border-2 px-8 py-7 text-center sm:px-10 sm:py-8",
        toneStyles[tone],
      )}
    >
      {cornerBadge ? (
        <span className="absolute right-3 top-3 rounded-md border border-purple-400/30 bg-purple-500/20 px-2 py-0.5 text-[10px] font-semibold uppercase tracking-wide text-purple-100">
          {cornerBadge}
        </span>
      ) : null}
      <div
        className={cn(
          "text-[11px] font-semibold uppercase tracking-[0.2em]",
          titleTone[tone],
        )}
      >
        {title}
      </div>
      <div className="mt-3 text-4xl font-bold tabular-nums tracking-tight text-white">{value}</div>
      <p className="mt-2 text-sm text-muted-foreground">{subtitle}</p>
    </div>
  );
}

function FlowConnector({ label }: { label: string }) {
  return (
    <div className="flex flex-col items-center gap-1 py-2">
      <div className="flex h-10 w-px flex-col items-center bg-gradient-to-b from-border via-muted-foreground/40 to-border" />
      <ArrowDown className="h-4 w-4 text-muted-foreground/80" aria-hidden />
      <span className="rounded-full border border-border/80 bg-muted/20 px-3 py-0.5 text-[11px] font-medium uppercase tracking-wide text-muted-foreground">
        {label}
      </span>
    </div>
  );
}

type Props = {
  /** Main counts (placeholders until wired). */
  memoryCount?: number;
  collectiveCount?: number;
  knowledgeCount?: number;
};

export function CollectiveLearningPipeline({
  memoryCount = 0,
  collectiveCount = 0,
  knowledgeCount = 0,
}: Props) {
  return (
    <section className="overflow-hidden rounded-2xl border border-border bg-gradient-to-b from-[#1c2438] via-[#171f2e] to-[#141a28]">
      <div className="border-b border-border/60 bg-[#1C2333]/40 px-6 py-5 sm:px-8">
        <div className="flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between">
          <div className="min-w-0 space-y-1.5">
            <h2 className="text-lg font-semibold tracking-tight text-white sm:text-xl">
              Collective learning pipeline
            </h2>
            <p className="max-w-3xl text-sm leading-relaxed text-muted-foreground">
              Agent memories are extracted into a shared queue, reviewed, and promoted into knowledge every agent can reuse.
            </p>
          </div>
        </div>
      </div>

      <div className="flex flex-col items-stretch px-4 py-10 sm:px-8 sm:py-12">
        <div className="flex w-full min-w-0 flex-col items-stretch gap-0">
          <PipelineStage
            tone="memory"
            title="ME · MEMORY"
            value={memoryCount}
            subtitle="per-agent memories, 0 agents"
          />
          <FlowConnector label="promote" />
          <PipelineStage
            tone="knowledge"
            title="KO · KNOWLEDGE"
            value={knowledgeCount}
            subtitle="shared across all agents"
          />
          <FlowConnector label="extract" />
          <PipelineStage
            tone="collective"
            title="LEARNINGS"
            value={collectiveCount}
            subtitle="promoted"
          />

        </div>
      </div>
    </section>
  );
}
