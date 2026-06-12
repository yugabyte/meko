import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import { Download } from "lucide-react";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";
import { ApiKeyStatusBanner } from "@/features/datapacks/components/install/ApiKeyStatusBanner";
import { McpInstallGuide } from "@/features/datapacks/components/install/McpInstallGuide";
import { useDatapacks } from "@/features/datapacks/hooks/useDatapacks";
import { getDatapackProvisionStatus, type ProvisionStatus } from "@/lib/mekoApi";
import { isDatapackListGrantOwner } from "@/lib/datapackShareGrants";

export default function HomePage() {
  // Connect page is workspace-scoped (not bound to a specific datapack),
  // so we synthesise an MCP-URL context by picking *any* owner datapack
  // from the user's list. The URL is the same across all datapacks for
  // a given account — it's a per-account endpoint, not per-datapack —
  // so the choice doesn't affect the install command content.
  //
  // We deliberately filter to *owner* datapacks: a contributor on a
  // shared datapack would technically have a provision-status URL, but
  // the install command they'd run is supposed to point at *their own*
  // workspace, not someone else's. Skipping shared rows avoids that
  // footgun even though the API would happily resolve them.
  const { data: datapacks } = useDatapacks();
  const firstOwner = useMemo(
    () => datapacks?.find((dp) => isDatapackListGrantOwner(dp)) ?? null,
    [datapacks],
  );

  // Provision-status is what carries the MCP server URL. The URL is
  // an account-scoped endpoint that doesn't change across an account's
  // lifetime, so we mark it `staleTime: Infinity` — fetched once per
  // page load (or rather, once per cache miss after a hard refresh),
  // then served from cache forever. Gate the query on `firstOwner` so
  // we don't fire it before the datapack list resolves.
  const { data: provisionStatus } = useQuery<ProvisionStatus>({
    queryKey: ["datapack-provision-status", firstOwner?.datapack_id ?? ""],
    queryFn: ({ signal }) => {
      if (!firstOwner) throw new Error("No owner datapack found");
      return getDatapackProvisionStatus(firstOwner.datapack_name, {
        signal,
        datapackId: firstOwner.datapack_id,
      });
    },
    enabled: !!firstOwner,
    staleTime: Infinity,
  });

  return (
    <div className="flex-1 h-screen overflow-y-auto bg-background text-foreground flex flex-col">
      <AppWorkspaceHeader />

      <main className="flex-1 space-y-8 p-8 pb-16 max-w-6xl mx-auto w-full">
        <div className="flex flex-col gap-2">
          <div className="flex items-center gap-3">
            <Download className="w-6 h-6 text-blue-400 shrink-0" aria-hidden />
            <h1 className="text-2xl font-semibold tracking-tight">Connect</h1>
          </div>
          <p className="pl-9 text-sm text-muted-foreground max-w-2xl leading-relaxed">
            Choose your environment and follow the steps.
          </p>
        </div>

        {/* Same warning/info banner the configure-wizard install step uses.
            Shows an amber "Generate API key" alert when the user has zero
            active tokens (the install command below needs one), or a
            neutral name+prefix chip list when they already have one. */}
        <ApiKeyStatusBanner />

        <McpInstallGuide
          className="mt-8"
          mcpServerUrl={provisionStatus?.mcp_server_url ?? null}
        />
      </main>
    </div>
  );
}
