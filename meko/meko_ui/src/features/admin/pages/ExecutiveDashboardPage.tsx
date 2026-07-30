// Executive Dashboard section: a VP/manager-facing overview — headcount totals
// up top, the account-wide token-burn savings hero, and a top-savers
// leaderboard.
import { useQuery } from "@tanstack/react-query";
import { Users, Layers, MessageSquare } from "lucide-react";
import { fetchExecutiveSummary } from "@/lib/api/adminApi";
import { MetricCard } from "@/features/admin/components/MetricCard";
import { SavingsHero } from "@/features/admin/components/SavingsHero";
import { SavingsLeaderboard } from "@/features/admin/components/SavingsLeaderboard";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { ADMIN_API_ERROR_HINT } from "@/features/admin/adminCopy";

export function ExecutiveDashboardPage() {
  // Loads the executive summary. Its Top savers leaderboard is computed by
  // aggregating meko_system.token_savings_log by searcher (user_id) across all
  // datapacks — fine for this admin-only, cached view.
  const query = useQuery({
    queryKey: ["admin-executive"],
    queryFn: () => fetchExecutiveSummary(),
    staleTime: 60_000,
  });

  const d = query.data;
  const loading = query.isLoading;

  return (
    <div className="flex flex-col gap-4 p-6 overflow-auto">
      <div>
        <h1 className="text-xl font-semibold">Executive Dashboard</h1>
        <p className="text-sm text-muted-foreground mt-0.5">
          Adoption and efficiency at a glance — how much context Meko trims and what it saves.
        </p>
      </div>

      {query.isError && (
        <Alert variant="destructive">
          <AlertDescription>
            Failed to load the executive summary. {ADMIN_API_ERROR_HINT}
          </AlertDescription>
        </Alert>
      )}

      {/* Top strip: headcount totals. */}
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-3">
        <MetricCard
          title="Total Users"
          value={d?.total_users}
          icon={Users}
          description="All-time"
          isLoading={loading}
          accentClassName="text-blue-400"
        />
        <MetricCard
          title="Total Datapacks"
          value={d?.total_datapacks}
          icon={Layers}
          description="Across all users"
          isLoading={loading}
          accentClassName="text-orange-400"
        />
        <MetricCard
          title="Total Conversations"
          value={d?.langfuse_enabled ? d?.total_conversations : undefined}
          icon={MessageSquare}
          description={d && !d.langfuse_enabled ? "Langfuse not configured" : "Chat sessions"}
          isLoading={loading}
          accentClassName="text-cyan-400"
        />
      </div>

      <SavingsHero
        totalRequests={d?.total_requests ?? 0}
        totalTokensSaved={d?.total_tokens_saved ?? 0}
        isLoading={loading}
      />

      <SavingsLeaderboard users={d?.top_users} isLoading={loading} />
    </div>
  );
}
