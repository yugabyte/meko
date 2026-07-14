import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { format, subDays } from "date-fns";
import {
  Layers,
  Share2,
  Brain,
  Sparkles,
  FileText,
  Cpu,
  Users,
  MessageSquare,
  Flame,
} from "lucide-react";
import { fetchAdminMetrics, fetchAdminUsers, fetchUsageMetrics } from "@/lib/adminApi";
import { MetricCard } from "../components/MetricCard";
import { DailyLoginsChart } from "../components/DailyLoginsChart";
import { UnloggedUsersTable } from "../components/UnloggedUsersTable";
import { UserGrowthChart } from "../components/UserGrowthChart";
import { TierSplitChart } from "../components/TierSplitChart";
import { ConversationsByAgentChart } from "../components/ConversationsByAgentChart";
import { UserScopeSelect, ALL_USERS } from "../components/UserScopeSelect";
import { SingleDatePicker } from "../components/SingleDatePicker";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";

export function DashboardPage() {
  const [scope, setScope] = useState<string>(ALL_USERS);
  const isAll = scope === ALL_USERS;

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

  const usage = useQuery({
    queryKey: ["admin-usage", scope, start, end],
    queryFn: () => fetchUsageMetrics(scope, start, end),
    staleTime: 60_000,
  });

  // Cognito/CloudWatch login charts are org-wide only; fetched for "all" scope.
  const login = useQuery({
    queryKey: ["admin-metrics"],
    queryFn: fetchAdminMetrics,
    staleTime: 60_000,
    enabled: isAll,
  });

  const u = usage.data;
  const loading = usage.isLoading;

  return (
    <div className="flex flex-col gap-4 p-6 overflow-auto">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold">Dashboard</h1>
          <p className="text-sm text-muted-foreground mt-0.5">
            {isAll ? "Platform-wide usage overview" : "Usage for the selected user"}
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
            {isAll && (
              <MetricCard
                title="Total Users"
                value={u?.total_users}
                icon={Users}
                description="All-time"
                isLoading={loading}
              />
            )}
            <MetricCard
              title="Datapacks"
              value={u?.datapacks}
              icon={Layers}
              description={isAll ? "All-time · all users" : "All-time · owned by user"}
              isLoading={loading}
            />
            <MetricCard
              title="Shares"
              value={u?.shared_datapacks}
              icon={Share2}
              description={isAll ? "Across all users" : "Shares by user in range"}
              isLoading={loading}
            />
            <MetricCard
              title="Private Memories"
              value={u?.private_memories}
              icon={Brain}
              description=""
              isLoading={loading}
            />
            <MetricCard
              title="Collective Memories"
              value={u?.collective_memories}
              icon={Sparkles}
              description="Promoted learnings"
              isLoading={loading}
            />
            <MetricCard
              title="Conversations"
              value={u?.langfuse_enabled ? u?.conversations : undefined}
              icon={MessageSquare}
              description="Chat sessions"
              isLoading={loading}
            />
            <MetricCard
              title="Token Burn"
              value={u?.langfuse_enabled ? u?.token_burn : undefined}
              icon={Flame}
              description="Total tokens"
              isLoading={loading}
            />
          </div>

          <ConversationsByAgentChart
            data={u?.conversations_by_agent}
            enabled={u?.langfuse_enabled}
            isLoading={loading}
          />

          {isAll && (
            <>
              <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
                <TierSplitChart free={u?.free_users} pro={u?.pro_users} isLoading={loading} />
                <DailyLoginsChart
                  data={login.data?.usage.dailyLogins}
                  isLoading={login.isLoading}
                />
              </div>
              <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
                <UserGrowthChart
                  data={login.data?.usage.userRegistrations}
                  totalUsers={u?.total_users}
                  isLoading={login.isLoading}
                />
                <UnloggedUsersTable
                  users={login.data?.usage.neverLoggedInUsers}
                  isLoading={login.isLoading}
                />
              </div>
            </>
          )}
        </TabsContent>

        <TabsContent value="system" className="flex flex-col gap-4 mt-4">
          <div className="grid grid-cols-2 gap-4">
            <MetricCard
              title="KB Documents Processed"
              value={u?.kb_documents}
              icon={FileText}
              description="Completed RAG documents"
              isLoading={loading}
            />
            <MetricCard
              title="Total Chunks Processed"
              value={u?.kb_chunks}
              icon={Cpu}
              description="Chunks persisted to vector store"
              isLoading={loading}
            />
          </div>
        </TabsContent>
      </Tabs>
    </div>
  );
}
