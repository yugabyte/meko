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
  FileSearch,
  BrainCog,
  ScanSearch,
} from "lucide-react";
import {
  fetchAdminUsers,
  fetchUserOverview,
  fetchUserMemories,
  fetchUserConversations,
  fetchUserTokens,
  fetchUserSearches,
  fetchUserKnowledge,
} from "@/lib/adminApi";
import { MetricCard } from "@/features/dashboard/components/MetricCard";
import { ConversationsByAgentChart } from "@/features/dashboard/components/ConversationsByAgentChart";
import { KbDocsByTypeChart } from "@/features/exec/components/KbDocsByTypeChart";
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

  // Independent panels, each on its own query key so the slow ones
  // (KB pipeline scan, Langfuse aggregates) no longer block the fast tiles or
  // time out the whole tab for a heavy user. The scope + range drive every key,
  // so a change refetches them all in parallel; each tile binds to its own
  // panel's loading state.
  const overview = useQuery({
    queryKey: ["admin-user-overview", scope, start, end],
    queryFn: () => fetchUserOverview(scope, start, end),
    staleTime: 60_000,
    enabled: !!scope,
  });
  const memories = useQuery({
    queryKey: ["admin-user-memories", scope, start, end],
    queryFn: () => fetchUserMemories(scope, start, end),
    staleTime: 60_000,
    enabled: !!scope,
  });
  const conversations = useQuery({
    queryKey: ["admin-user-conversations", scope, start, end],
    queryFn: () => fetchUserConversations(scope, start, end),
    staleTime: 60_000,
    enabled: !!scope,
  });
  const tokens = useQuery({
    queryKey: ["admin-user-tokens", scope, start, end],
    queryFn: () => fetchUserTokens(scope, start, end),
    staleTime: 60_000,
    enabled: !!scope,
  });
  const searches = useQuery({
    queryKey: ["admin-user-searches", scope, start, end],
    queryFn: () => fetchUserSearches(scope, start, end),
    staleTime: 60_000,
    enabled: !!scope,
  });
  const knowledge = useQuery({
    queryKey: ["admin-user-knowledge", scope, start, end],
    queryFn: () => fetchUserKnowledge(scope, start, end),
    staleTime: 60_000,
    enabled: !!scope,
  });

  const o = overview.data;
  const mem = memories.data;
  const conv = conversations.data;
  const tok = tokens.data;
  const srch = searches.data;
  const k = knowledge.data;
  // While the user list is still loading (before a scope is auto-selected) every
  // panel shows a skeleton; after that each follows its own fetch.
  const scopeLoading = !scope && usersQuery.isLoading;
  const anyError = [overview, memories, conversations, tokens, searches, knowledge].some(
    (q) => q.isError,
  );

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

      {anyError && (
        <Alert variant="destructive">
          <AlertDescription>
            Some metrics failed to load. Ensure api_server is running and ADMIN_API_KEY matches on
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
              value={o?.datapacks}
              icon={Layers}
              description="All-time · owned by user"
              isLoading={overview.isLoading || scopeLoading}
              accentClassName="text-orange-400"
            />
            <MetricCard
              title="Shares"
              value={o?.shared_datapacks}
              icon={Share2}
              description="Shares by user in range"
              isLoading={overview.isLoading || scopeLoading}
              accentClassName="text-violet-400"
            />
            <MetricCard
              title="Private Memories"
              value={mem?.private_memories}
              icon={Brain}
              description=""
              isLoading={memories.isLoading || scopeLoading}
              accentClassName="text-emerald-400"
            />
            <MetricCard
              title="Collective Memories"
              value={mem?.collective_memories}
              icon={Sparkles}
              description="Promoted learnings"
              isLoading={memories.isLoading || scopeLoading}
              accentClassName="text-pink-400"
            />
            <MetricCard
              title="Conversations"
              value={conv?.langfuse_enabled ? conv?.conversations : undefined}
              icon={MessageSquare}
              description="Chat sessions"
              isLoading={conversations.isLoading || scopeLoading}
              accentClassName="text-cyan-400"
            />
            <MetricCard
              title="Token Burn"
              value={tok?.langfuse_enabled ? tok?.token_burn : undefined}
              icon={Flame}
              description="Total tokens"
              isLoading={tokens.isLoading || scopeLoading}
              accentClassName="text-amber-400"
            />
            <MetricCard
              title="KB Searches"
              value={srch?.kb_searches}
              icon={FileSearch}
              description="Knowledge-base lookups in range"
              isLoading={searches.isLoading || scopeLoading}
              accentClassName="text-sky-400"
            />
            <MetricCard
              title="Memory Searches"
              value={srch?.memory_searches}
              icon={BrainCog}
              description="Memory lookups in range"
              isLoading={searches.isLoading || scopeLoading}
              accentClassName="text-rose-400"
            />
            <MetricCard
              title="Context Searches"
              value={srch?.context_searches}
              icon={ScanSearch}
              description="Contextual retrievals in range"
              isLoading={searches.isLoading || scopeLoading}
              accentClassName="text-indigo-400"
            />
          </div>

          <ConversationsByAgentChart
            data={conv?.conversations_by_agent}
            enabled={conv?.langfuse_enabled}
            isLoading={conversations.isLoading || scopeLoading}
          />
        </TabsContent>

        <TabsContent value="system" className="flex flex-col gap-4 mt-4">
          <div className="grid grid-cols-2 gap-4">
            <MetricCard
              title="KB Documents Processed"
              value={k?.kb_documents}
              icon={FileText}
              description="Completed RAG documents"
              isLoading={knowledge.isLoading || scopeLoading}
              accentClassName="text-sky-400"
            />
            <MetricCard
              title="Total Chunks Processed"
              value={k?.kb_chunks}
              icon={Cpu}
              description="Chunks persisted to vector store"
              isLoading={knowledge.isLoading || scopeLoading}
              accentClassName="text-teal-400"
            />
          </div>

          <KbDocsByTypeChart
            data={k?.kb_documents_by_type}
            isLoading={knowledge.isLoading || scopeLoading}
          />
        </TabsContent>
      </Tabs>
    </div>
  );
}
