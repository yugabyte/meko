/**
 * Row of six summary metric cards for the datapack dashboard (memory, knowledge,
 * learnings, conversations, agents, tokens). Values are placeholders until real APIs exist.
 */
import {
  ArrowLeftRight,
  BookOpen,
  BrainCircuit,
  MessageCircle,
  Users,
  Zap,
} from "lucide-react";
import { Card, CardContent } from "@/components/ui/card";
import { cn } from "@/lib/utils";
import { DatapackStatus } from "@/lib/mekoApi";

/** Placeholder until dashboard APIs exist. */
const PLACEHOLDER_METRICS = {
  memory: 0,
  knowledge: 0,
  learnings: 0,
  conversations: 0,
  agents: 0,
  tokens24h: 0,
} as const;

type IconType = typeof BrainCircuit;

function MetricCard({
  icon: Icon,
  iconClass,
  label,
  value,
  sub,
}: {
  icon: IconType;
  iconClass: string;
  label: string;
  value: number;
  sub: string;
}) {
  return (
    <Card className="border-border bg-[#0E1525]/80 shadow-sm">
      <CardContent className="p-4">
        <div className="flex items-center gap-2 text-[11px] font-medium uppercase tracking-wide text-muted-foreground">
          <Icon className={cn("h-3.5 w-3.5 shrink-0", iconClass)} aria-hidden />
          <span className="truncate">{label}</span>
        </div>
        <p className="mt-2 text-2xl font-semibold tabular-nums text-white">{value}</p>
        <p className="mt-1 text-xs text-muted-foreground leading-snug line-clamp-2">{sub}</p>
      </CardContent>
    </Card>
  );
}

const z = PLACEHOLDER_METRICS;

export function DatapackMetricsRibbon({ datapackStatus }: { datapackStatus: DatapackStatus | null }) {
  const knowledgeCount = datapackStatus?.knowledge_base_count ?? 0;
  const agentCount = datapackStatus?.agent_count ?? 0;
  return (
    <div className="grid grid-cols-2 gap-3 lg:grid-cols-3 xl:grid-cols-5">
      <MetricCard
        icon={BrainCircuit}
        iconClass="text-violet-400"
        label="MEMORY (ME)"
        value={z.memory}
        sub="across all agents"
      />
      <MetricCard
        icon={BookOpen}
        iconClass="text-emerald-400"
        label="KNOWLEDGE (KO)"
        value={knowledgeCount}
        sub="0 chunks indexed"
      />
      <MetricCard
        icon={ArrowLeftRight}
        iconClass="text-amber-400"
        label="LEARNINGS (ME → KO)"
        value={z.learnings}
        sub="promoted"
      />
      <MetricCard
        icon={Users}
        iconClass="text-blue-400"
        label="AGENTS"
        value={agentCount}
        sub=""
      />
      <MetricCard
        icon={Zap}
        iconClass="text-yellow-400"
        label="TOKENS"
        value={z.tokens24h}
        sub=""
      />
    </div>
  );
}
