/**
 * Configure wizard step: wraps the shared MCP install guide in a card consistent with other steps.
 * No local state; all interaction lives inside McpInstallGuide.
 */
import { McpInstallGuide } from "@/features/datapacks/components/install/McpInstallGuide";

type Props = {
  mcpServerUrl?: string | null;
  mcpApiKey?: string | null;
  provisionLoading?: boolean;
  provisionError?: string | null;
};

export function ConfigureWizardInstallStep({
  mcpServerUrl = null,
  mcpApiKey = null,
  provisionLoading = false,
  provisionError = null,
}: Props) {
  return (
    <div className="space-y-4">
      {provisionError ? (
        <div className="rounded-lg border border-amber-500/40 bg-amber-500/10 px-4 py-3 text-sm text-amber-100">
          {provisionError} Commands below still show placeholders; you can copy MCP URL and API key from the
          datapack Connect dialog after setup.
        </div>
      ) : null}
      {provisionLoading ? (
        <p className="text-sm text-muted-foreground">Loading your MCP URL and API key for this datapack…</p>
      ) : null}
      <div className="rounded-xl border border-border bg-card/30 p-4 sm:p-6">
        <McpInstallGuide mcpServerUrl={mcpServerUrl} mcpApiKey={mcpApiKey} />
      </div>
    </div>
  );
}
