/**
 * Configure wizard step: wraps the shared MCP install guide in a card consistent with other steps.
 * No local state; all interaction lives inside McpInstallGuide.
 */
import { McpInstallGuide } from "@/features/datapacks/components/install/McpInstallGuide";

export function ConfigureWizardInstallStep() {
  return (
    <div className="rounded-xl border border-border bg-card/30 p-4 sm:p-6">
      <McpInstallGuide />
    </div>
  );
}
