// ExecutiveDashboardPage: a VP/manager-facing overview for a selected date
// range (default: last 4 months). Fetched as four parallel panels — growth,
// engagement, knowledge, savings — so each renders on its own timeline instead
// of the whole page waiting on the slowest (KB pipeline / Langfuse) query.
import { useState, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { format, subMonths } from "date-fns";
import { Users, Layers, MessageSquare, Brain, FileText, Boxes } from "lucide-react";
import {
  fetchExecGrowth,
  fetchExecEngagement,
  fetchExecKnowledge,
  fetchExecSavings,
} from "@/lib/adminApi";
import { MetricCard } from "@/features/dashboard/components/MetricCard";
import { SingleDatePicker } from "@/features/dashboard/components/SingleDatePicker";
import { ConversationsByAgentChart } from "@/features/dashboard/components/ConversationsByAgentChart";
import { SavingsHero } from "@/features/exec/components/SavingsHero";
import { SavingsLeaderboard } from "@/features/exec/components/SavingsLeaderboard";
import { KbDocsByTypeChart } from "@/features/exec/components/KbDocsByTypeChart";
import { DatapacksByWeekChart } from "@/features/exec/components/DatapacksByWeekChart";
import { Alert, AlertDescription } from "@/components/ui/alert";

export function ExecutiveDashboardPage() {
  // Default window: the last 4 months (inclusive), Start/End reusing the same
  // pickers as the Metrics page.
  const [startDate, setStartDate] = useState<Date | undefined>(subMonths(new Date(), 4));
  const [endDate, setEndDate] = useState<Date | undefined>(new Date());
  const start = startDate ? format(startDate, "yyyy-MM-dd") : undefined;
  const end = endDate ? format(endDate, "yyyy-MM-dd") : undefined;

  // Four independent panels; the one date picker drives every query key, so a
  // range change refetches all four in parallel. Each tile/chart binds to its
  // own panel's data + loading, so fast panels paint before the slow ones.
  const growth = useQuery({
    queryKey: ["admin-exec-growth", start, end],
    queryFn: () => fetchExecGrowth(start, end),
    staleTime: 60_000,
  });
  const engagement = useQuery({
    queryKey: ["admin-exec-engagement", start, end],
    queryFn: () => fetchExecEngagement(start, end),
    staleTime: 60_000,
  });
  const knowledge = useQuery({
    queryKey: ["admin-exec-knowledge", start, end],
    queryFn: () => fetchExecKnowledge(start, end),
    staleTime: 60_000,
  });
  const savings = useQuery({
    queryKey: ["admin-exec-savings", start, end],
    queryFn: () => fetchExecSavings(start, end),
    staleTime: 60_000,
  });

  const g = growth.data;
  const e = engagement.data;
  const k = knowledge.data;
  const s = savings.data;
  const anyError = [growth, engagement, knowledge, savings].some((q) => q.isError);

  return (
    <div className="flex flex-col gap-4 p-6 overflow-auto">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold">Executive Dashboard</h1>
          <p className="text-sm text-muted-foreground mt-0.5">
            Adoption and efficiency for the selected range
            {start && end ? ` · ${start} → ${end}` : ""}
          </p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <SingleDatePicker
            label="Start"
            value={startDate}
            onChange={setStartDate}
            disabled={(day) => (endDate ? day > endDate : false)}
          />
          <SingleDatePicker
            label="End"
            value={endDate}
            onChange={setEndDate}
            disabled={(day) => day > new Date() || (startDate ? day < startDate : false)}
          />
        </div>
      </div>

      {anyError && (
        <Alert variant="destructive">
          <AlertDescription>
            Some panels failed to load. Ensure api_server is running and ADMIN_API_KEY matches on
            both sides.
          </AlertDescription>
        </Alert>
      )}

      <Section title="Users & Datapacks">
        <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
          <MetricCard
            title="New Users"
            value={g?.users_created}
            icon={Users}
            description="Created"
            isLoading={growth.isLoading}
            accentClassName="text-blue-400"
          />
          <MetricCard
            title="New Datapacks"
            value={g?.datapacks_created}
            icon={Layers}
            description="Created"
            isLoading={growth.isLoading}
            accentClassName="text-orange-400"
          />
        </div>
        <DatapacksByWeekChart
          data={g?.datapacks_by_week}
          start={startDate}
          end={endDate}
          isLoading={growth.isLoading}
        />
      </Section>

      <Section title="Conversations, Memory & Knowledge">
        <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-4">
          <MetricCard
            title="Conversations"
            value={e?.langfuse_enabled ? e?.conversations : undefined}
            icon={MessageSquare}
            description={e && !e.langfuse_enabled ? "Langfuse not configured" : "Chat sessions"}
            isLoading={engagement.isLoading}
            accentClassName="text-cyan-400"
          />
          <MetricCard
            title="Promoted Memories"
            value={e?.promoted_memories}
            icon={Brain}
            description="Promoted to KB"
            isLoading={engagement.isLoading}
            accentClassName="text-violet-400"
          />
          <MetricCard
            title="KB Documents"
            value={k?.kb_documents}
            icon={FileText}
            description="Indexed (RAG'd)"
            isLoading={knowledge.isLoading}
            accentClassName="text-emerald-400"
          />
          <MetricCard
            title="Chunks Generated"
            value={k?.kb_chunks}
            icon={Boxes}
            description="From indexed docs"
            isLoading={knowledge.isLoading}
            accentClassName="text-amber-400"
          />
        </div>
        <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
          <ConversationsByAgentChart
            data={e?.conversations_by_agent}
            enabled={e?.langfuse_enabled}
            isLoading={engagement.isLoading}
          />
          <KbDocsByTypeChart data={k?.kb_documents_by_type} isLoading={knowledge.isLoading} />
        </div>
      </Section>

      <Section title="Token Savings">
        <SavingsHero
          totalRequests={s?.total_requests ?? 0}
          totalTokensSaved={s?.total_tokens_saved ?? 0}
          isLoading={savings.isLoading}
        />
        <SavingsLeaderboard users={s?.top_users} isLoading={savings.isLoading} />
      </Section>
    </div>
  );
}

// A labeled group of tiles/charts. A top rule + uppercase heading separates the
// dashboard's three areas (growth, engagement/knowledge, savings).
function Section({ title, children }: { title: string; children: ReactNode }) {
  return (
    <section className="flex flex-col gap-4 border-t border-border pt-5 first-of-type:border-t-0 first-of-type:pt-1">
      <h2 className="text-xs font-semibold uppercase tracking-wider text-muted-foreground">
        {title}
      </h2>
      {children}
    </section>
  );
}
