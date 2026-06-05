/**
 * Configure wizard step: wraps the shared MCP install guide in a card consistent with other steps.
 *
 * Above the install commands we render the shared `ApiKeyStatusBanner`
 * which handles all three states (no keys → warn + generate dialog;
 * one+ keys → neutral chips; loading/error → nothing). Extracted into
 * its own file so the Connect page (`/connect`) can reuse the exact
 * same flow.
 */
import { ConfigureWizardAgentDetectionCard } from "@/features/datapacks/components/configure-wizard/ConfigureWizardAgentDetectionCard";
import { ApiKeyStatusBanner } from "@/features/datapacks/components/install/ApiKeyStatusBanner";
import { McpInstallGuide } from "@/features/datapacks/components/install/McpInstallGuide";

type Props = {
  mcpServerUrl?: string | null;
  provisionLoading?: boolean;
};

export function ConfigureWizardInstallStep({
  mcpServerUrl = null,
  provisionLoading = false,
}: Props) {
  return (
    <div className="space-y-6">
      {provisionLoading ? (
        <p className="text-sm text-muted-foreground">Loading your MCP URL for this datapack…</p>
      ) : null}

      <ApiKeyStatusBanner />

      <div className="rounded-xl border border-border bg-card/30 p-4 sm:p-6">
        <McpInstallGuide mcpServerUrl={mcpServerUrl} />
      </div>
      <ConfigureWizardAgentDetectionCard />
    </div>
  );
}
