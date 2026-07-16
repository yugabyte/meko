import { useEffect, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { format, subDays } from "date-fns";
import {
  Layers,
  Share2,
  Brain,
  Sparkles,
  FileText,
  Cpu,
  MessageSquare,
  Flame,
} from "lucide-react";
import { fetchAdminUsers, fetchUsageMetrics } from "@/lib/adminApi";
import { MetricCard } from "@/features/dashboard/components/MetricCard";
import { ConversationsByAgentChart } from "@/features/dashboard/components/ConversationsByAgentChart";
import { UserScopeSelect } from "@/features/dashboard/components/UserScopeSelect";
import { SingleDatePicker } from "@/features/dashboard/components/SingleDatePicker";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";

export function MetricsPage() {
  // Metrics are strictly per-user: aggregating across ~1000 users is too
  // expensive, so we always scope to a single selected user (defaulting to the
  // first one once the user list loads).
  const [scope, setScope] = useState<string>("");

  // Default window: last 30 days (inclusive). Separate Start/End pickers.
  const [startDate, setStartDate] = useState<Date | undefined>(subDays(new Date(), 29));
  const [endDate, setEndDate] = useState<Date | undefined>(new Date());
  const start = startDate ? format(startDate, "yyyy-MM-dd") : undefined;
  const end = endDate ? format(endDate, "yyyy-MM-dd") : undefined;

  const usersQuery = useQuery({
    queryKey: ["admin-users"],
    queryFn: fetchAdminUsers,
    staleTime: 60_000,
  });

  // Default to the first user once the list arrives (and only while nothing is
  // selected yet, so a manual pick is never clobbered by a background refetch).
  useEffect(() => {
    if (!scope && usersQuery.data && usersQuery.data.length > 0) {
      setScope(usersQuery.data[0].id);
    }
  }, [scope, usersQuery.data]);

  const usage = useQuery({
    queryKey: ["admin-usage", scope, start, end],
    queryFn: () => fetchUsageMetrics(scope, start, end),
    staleTime: 60_000,
    enabled: !!scope,
  });

  const u = usage.data;
  const loading = usage.isLoading || (!scope && usersQuery.isLoading);

  return (
    <div className="flex flex-col gap-4 p-6 overflow-auto">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold">Metrics</h1>
          <p className="text-sm text-muted-foreground mt-0.5">
            Usage & system metrics · selected user
            {start && end ? ` · activity ${start} → ${end}` : ""}
          </p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <SingleDatePicker
            label="Start"
            value={startDate}
            onChange={setStartDate}
            disabled={(d) => (endDate ? d > endDate : false)}
          />
          <SingleDatePicker
            label="End"
            value={endDate}
            onChange={setEndDate}
            disabled={(d) => d > new Date() || (startDate ? d < startDate : false)}
          />
          <UserScopeSelect
            users={usersQuery.data ?? []}
            value={scope}
            onChange={setScope}
            disabled={usersQuery.isLoading}
          />
        </div>
      </div>

      {usage.isError && (
        <Alert variant="destructive">
          <AlertDescription>
            Failed to load usage metrics. Ensure api_server is running and ADMIN_API_KEY matches on
            both sides.
          </AlertDescription>
        </Alert>
      )}

      <Tabs defaultValue="usage">
        <TabsList>
          <TabsTrigger value="usage">Usage</TabsTrigger>
          <TabsTrigger value="system">System</TabsTrigger>
        </TabsList>

        <TabsContent value="usage" className="flex flex-col gap-4 mt-4">
          <div className="grid grid-cols-2 gap-4 lg:grid-cols-4">
            <MetricCard
              title="Datapacks"
              value={u?.datapacks}
              icon={Layers}
              description="All-time · owned by user"
              isLoading={loading}
              accentClassName="text-orange-400"
            />
            <MetricCard
              title="Shares"
              value={u?.shared_datapacks}
              icon={Share2}
              description="Shares by user in range"
              isLoading={loading}
              accentClassName="text-violet-400"
            />
            <MetricCard
              title="Private Memories"
              value={u?.private_memories}
              icon={Brain}
              description=""
              isLoading={loading}
              accentClassName="text-emerald-400"
            />
            <MetricCard
              title="Collective Memories"
              value={u?.collective_memories}
              icon={Sparkles}
              description="Promoted learnings"
              isLoading={loading}
              accentClassName="text-pink-400"
            />
            <MetricCard
              title="Conversations"
              value={u?.langfuse_enabled ? u?.conversations : undefined}
              icon={MessageSquare}
              description="Chat sessions"
              isLoading={loading}
              accentClassName="text-cyan-400"
            />
            <MetricCard
              title="Token Burn"
              value={u?.langfuse_enabled ? u?.token_burn : undefined}
              icon={Flame}
              description="Total tokens"
              isLoading={loading}
              accentClassName="text-amber-400"
            />
          </div>

          <ConversationsByAgentChart
            data={u?.conversations_by_agent}
            enabled={u?.langfuse_enabled}
            isLoading={loading}
          />
        </TabsContent>

        <TabsContent value="system" className="flex flex-col gap-4 mt-4">
          <div className="grid grid-cols-2 gap-4">
            <MetricCard
              title="KB Documents Processed"
              value={u?.kb_documents}
              icon={FileText}
              description="Completed RAG documents"
              isLoading={loading}
              accentClassName="text-sky-400"
            />
            <MetricCard
              title="Total Chunks Processed"
              value={u?.kb_chunks}
              icon={Cpu}
              description="Chunks persisted to vector store"
              isLoading={loading}
              accentClassName="text-teal-400"
            />
          </div>
        </TabsContent>
      </Tabs>
    </div>
  );
}
