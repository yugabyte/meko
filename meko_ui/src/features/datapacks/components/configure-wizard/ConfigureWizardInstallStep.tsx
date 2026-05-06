/**
 * Configure wizard step: wraps the shared MCP install guide in a card consistent with other steps.
 * No local state; all interaction lives inside McpInstallGuide.
 */
import { ConfigureWizardAgentDetectionCard } from "@/features/datapacks/components/configure-wizard/ConfigureWizardAgentDetectionCard";
import { McpInstallGuide } from "@/features/datapacks/components/install/McpInstallGuide";

type Props = {
  mcpServerUrl?: string | null;
  mcpApiKey?: string | null;
  provisionLoading?: boolean;
};

export function ConfigureWizardInstallStep({
  mcpServerUrl = null,
  mcpApiKey = null,
  provisionLoading = false,
}: Props) {
  return (
    <div className="space-y-6">
      {provisionLoading ? (
        <p className="text-sm text-muted-foreground">Loading your MCP URL and API key for this datapack…</p>
      ) : null}
      <div className="rounded-xl border border-border bg-card/30 p-4 sm:p-6">
        <McpInstallGuide mcpServerUrl={mcpServerUrl} mcpApiKey={mcpApiKey} />
      </div>
      <ConfigureWizardAgentDetectionCard />
    </div>
  );
}
