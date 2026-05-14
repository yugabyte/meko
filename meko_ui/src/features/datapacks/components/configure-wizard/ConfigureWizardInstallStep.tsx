/**
 * Configure wizard step: wraps the shared MCP install guide in a card consistent with other steps.
 *
 * The wizard intentionally does NOT fetch the user's token list here — the
 * install commands always render with the {@link MEKO_TOKEN_INSTALLER_PLACEHOLDER}
 * literal regardless. A small banner at the top points the user at Settings
 * to mint a token if they don't already have one (one click, always visible —
 * cheaper than wiring up a list fetch + conditional render in the wizard).
 */
import { Link } from "react-router-dom";
import { ExternalLink, KeyRound } from "lucide-react";
import { ROUTES } from "@/app/routes/paths";
import { ConfigureWizardAgentDetectionCard } from "@/features/datapacks/components/configure-wizard/ConfigureWizardAgentDetectionCard";
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

      <ApiTokenReminder />

      <div className="rounded-xl border border-border bg-card/30 p-4 sm:p-6">
        <McpInstallGuide mcpServerUrl={mcpServerUrl} />
      </div>
      <ConfigureWizardAgentDetectionCard />
    </div>
  );
}

/** Static banner — always shown — pointing users to Settings → Security to
 *  mint a token. No list fetch; the placeholder in the install commands
 *  works whether the user has a token already or not. */
function ApiTokenReminder() {
  return (
    <div className="flex flex-wrap items-start gap-3 rounded-lg border border-border bg-[#0E1525] px-4 py-3">
      <KeyRound className="mt-0.5 h-4 w-4 shrink-0 text-blue-300" aria-hidden />
      <div className="min-w-0 flex-1 text-xs leading-relaxed text-muted-foreground">
        Paste the token you generated in{" "}
        <Link
          to={ROUTES.settings}
          className="inline-flex items-center gap-0.5 font-medium text-blue-300 hover:text-blue-200"
        >
          Settings → Security
          <ExternalLink className="h-3 w-3" aria-hidden />
        </Link>
        .
      </div>
    </div>
  );
}
