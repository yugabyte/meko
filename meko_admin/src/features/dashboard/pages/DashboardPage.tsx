import { useQuery } from "@tanstack/react-query";
import {
  Layers,
  FileText,
  Cpu,
  ListTodo,
  Users,
} from "lucide-react";
import { fetchAdminMetrics } from "@/lib/adminApi";
import { MetricCard } from "../components/MetricCard";
import { DailyLoginsChart } from "../components/DailyLoginsChart";
import { UnloggedUsersTable } from "../components/UnloggedUsersTable";
import { UserGrowthChart } from "../components/UserGrowthChart";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";

export function DashboardPage() {
  const { data, isLoading, isError } = useQuery({
    queryKey: ["admin-metrics"],
    queryFn: fetchAdminMetrics,
    staleTime: 60_000,
    refetchInterval: 60_000,
  });

  return (
    <div className="flex flex-col gap-4 p-6 overflow-auto">
      <div>
        <h1 className="text-xl font-semibold">Dashboard</h1>
        <p className="text-sm text-muted-foreground mt-0.5">
          Platform health and usage overview
        </p>
      </div>

      {isError && (
        <Alert variant="destructive">
          <AlertDescription>
            Failed to load metrics. Check server logs and ensure the database
            and AWS credentials are configured.
          </AlertDescription>
        </Alert>
      )}

      <Tabs defaultValue="usage">
        <TabsList>
          <TabsTrigger value="usage">Usage</TabsTrigger>
          <TabsTrigger value="system">System</TabsTrigger>
        </TabsList>

        <TabsContent value="usage" className="flex flex-col gap-4 mt-4">
          <div className="grid grid-cols-2 gap-4">
            <MetricCard
              title="Total Users"
              value={data?.usage.totalUsers}
              icon={Users}
              description="Registered in meko_system"
              isLoading={isLoading}
            />
            <MetricCard
              title="Datapacks Created"
              value={data?.system.datapacksCreated}
              icon={Layers}
              description="Total across all users"
              isLoading={isLoading}
            />
          </div>
          <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
            <DailyLoginsChart
              data={data?.usage.dailyLogins}
              isLoading={isLoading}
            />
            <UnloggedUsersTable
              users={data?.usage.neverLoggedInUsers}
              isLoading={isLoading}
            />
          </div>
          <UserGrowthChart
            data={data?.usage.userRegistrations}
            totalUsers={data?.usage.totalUsers}
            isLoading={isLoading}
          />
        </TabsContent>

        <TabsContent value="system" className="flex flex-col gap-4 mt-4">
          <div className="grid grid-cols-2 gap-4 lg:grid-cols-3">
            <MetricCard
              title="Work Items Queued"
              value={data?.system.queuedWorkItems}
              icon={ListTodo}
              description="pg_dist_rag queue depth"
              isLoading={isLoading}
            />
            <MetricCard
              title="Documents Processed"
              value={data?.system.documentsProcessed}
              icon={FileText}
              description="Completed RAG documents"
              isLoading={isLoading}
            />
            <MetricCard
              title="Embeddings Generated"
              value={data?.system.embeddingsGenerated}
              icon={Cpu}
              description="Chunks persisted to vector store"
              isLoading={isLoading}
            />
          </div>
        </TabsContent>
      </Tabs>
    </div>
  );
}
