// Deploy-agent step: shown inside DeployAgentStage. Lesson 1 of "Deploy an
// agent" -- users pick between the shared, pre-deployed AgentK (no wait) and
// deploying their own dedicated instance (gated to a paid tier). Deploying
// your own is two sub-steps: pick a runtime target (only AgentCore is live;
// Vercel/Lyzr are visible "coming soon"), then walk the BYOC role-setup
// wizard -- deploying into Meko's own AWS account isn't offered here at all;
// a dedicated instance always means the user's own AWS account.
// agentDisplayName is per-lab (e.g. "AgentK") -- this component is shared
// across labs, so it never hardcodes one lab's agent name.
//
// Deployment mode/selection live in `session` (useLabRunnerSession), not
// local state -- Run a Review (a different stage entirely) needs to know
// which deployment to invoke, so it has to survive navigating away from
// this stage, the same way session.datapackId already does.

import { useState } from "react";
import { CheckCircle2, ExternalLink, Loader2, Rocket } from "lucide-react";

import { Button } from "@/components/ui/button";
import { canDeployDedicatedAgent } from "@/lib/freeTier";

import { useSharedDeploymentStatus } from "../../hooks/useDeployments";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import { ByocRoleSetup } from "./deploy/ByocRoleSetup";
import { DeploymentModeChooser } from "./deploy/DeploymentModeChooser";
import { DeploymentStatus } from "./deploy/DeploymentStatus";
import { RuntimeTargetPicker, type RuntimeTargetId } from "./deploy/RuntimeTargetPicker";

// Numbered step heading, reused for each stage of the picker so the flow
// reads as one sequence instead of cards just appearing with no context.
function DeployStepHeading({ number, title }: { number: number; title: string }) {
  return (
    <div className="flex items-center gap-2">
      <span className="flex h-6 w-6 shrink-0 items-center justify-center rounded-full bg-primary text-xs font-semibold text-primary-foreground">
        {number}
      </span>
      <p className="text-sm font-semibold">{title}</p>
    </div>
  );
}

// Always-visible info panel so users can inspect what AgentK actually is
// before (or after) deploying it, not just take it on faith. The image URI
// is only known once a deployment exists (session.deployment.container_uri)
// -- showing the exact image that got deployed is a stronger, more literal
// answer than guessing the pre-deploy naming convention client-side.
function AboutAgent({
  agentDisplayName,
  sourceRepoUrl,
  containerUri,
}: {
  agentDisplayName: string;
  sourceRepoUrl: string;
  containerUri: string | null;
}) {
  return (
    <div className="flex flex-col gap-1 rounded-md border border-border bg-muted/20 p-3 text-xs text-muted-foreground">
      <p>
        <span className="font-medium text-foreground">{agentDisplayName}</span> is open source:{" "}
        <a
          href={sourceRepoUrl}
          target="_blank"
          rel="noreferrer"
          className="inline-flex items-center gap-0.5 text-primary hover:underline"
        >
          view the source
          <ExternalLink className="h-3 w-3" />
        </a>
        .
      </p>
      {containerUri && (
        <p>
          Deployed image: <span className="font-mono">{containerUri}</span>
        </p>
      )}
    </div>
  );
}

export function DeployAgentPanel({
  agentSlug,
  agentDisplayName,
  sourceRepoUrl,
  session,
}: {
  agentSlug: string;
  agentDisplayName: string;
  sourceRepoUrl: string;
  session: ReturnType<typeof useLabRunnerSession>;
}) {
  const { deploymentMode, selectDeploymentMode } = session;
  const canDeployDedicated = canDeployDedicatedAgent(session.currentUser?.user_type);

  return (
    <div className="flex flex-col gap-4">
      <DeploymentModeChooser
        selected={deploymentMode}
        onSelect={selectDeploymentMode}
        canDeployDedicated={canDeployDedicated}
      />
      {deploymentMode === "shared" ? (
        <SharedAgentPanel
          agentSlug={agentSlug}
          agentDisplayName={agentDisplayName}
          sourceRepoUrl={sourceRepoUrl}
        />
      ) : (
        <DedicatedAgentPanel
          agentSlug={agentSlug}
          agentDisplayName={agentDisplayName}
          sourceRepoUrl={sourceRepoUrl}
          session={session}
        />
      )}
    </div>
  );
}

// Nothing to configure -- fetches the same shared-deployment status
// useLabRunnerSession resolves session.deployment/deploymentActive from
// (same query cache key, so no duplicate requests). Just shows what it is
// and whether it's ready.
function SharedAgentPanel({
  agentSlug,
  agentDisplayName,
  sourceRepoUrl,
}: {
  agentSlug: string;
  agentDisplayName: string;
  sourceRepoUrl: string;
}) {
  const { data: deployment, isLoading, isError } = useSharedDeploymentStatus(agentSlug, true);
  const active = deployment?.status === "active";
  return (
    <div className="flex flex-col gap-3 rounded-md border border-border p-4">
      <div className="flex items-center gap-2">
        <Rocket className="h-4 w-4 shrink-0 text-muted-foreground" />
        <p className="text-sm font-medium">Shared {agentDisplayName}</p>
      </div>
      <AboutAgent
        agentDisplayName={agentDisplayName}
        sourceRepoUrl={sourceRepoUrl}
        containerUri={deployment?.container_uri ?? null}
      />
      {active ? (
        <p className="flex items-center gap-1.5 text-xs text-emerald-700 dark:text-emerald-300">
          <CheckCircle2 className="h-3.5 w-3.5" aria-hidden />
          Ready. Click Next to continue.
        </p>
      ) : isError ? (
        <p className="text-xs text-destructive">
          This lab isn't configured with a shared instance yet. Deploy your own instead.
        </p>
      ) : deployment ? (
        <p className="text-xs text-destructive">
          The shared instance isn't available right now ({deployment.status}). Try deploying your
          own instead.
        </p>
      ) : (
        <p className="flex items-center gap-1.5 text-xs text-muted-foreground">
          {isLoading && <Loader2 className="h-3.5 w-3.5 animate-spin" aria-hidden />}
          Checking the shared instance…
        </p>
      )}
    </div>
  );
}

function DedicatedAgentPanel({
  agentSlug,
  agentDisplayName,
  sourceRepoUrl,
  session,
}: {
  agentSlug: string;
  agentDisplayName: string;
  sourceRepoUrl: string;
  session: ReturnType<typeof useLabRunnerSession>;
}) {
  const [runtime, setRuntime] = useState<RuntimeTargetId | null>(null);
  const { deployment, selectDeployment, clearDeployment } = session;

  if (deployment) {
    // failed/deleted are terminal (useDeploymentStatus stops polling) with
    // no automatic way forward -- offer a way back to the picker instead of
    // leaving the user stuck on a dead "Deploying" panel.
    const isTerminalFailure = deployment.status === "failed" || deployment.status === "deleted";
    return (
      <div className="flex flex-col gap-3 rounded-md border border-border p-4">
        <p className="text-sm font-medium">
          {isTerminalFailure
            ? `Couldn't deploy ${agentDisplayName}`
            : `Deploying ${agentDisplayName}`}
        </p>
        <AboutAgent
          agentDisplayName={agentDisplayName}
          sourceRepoUrl={sourceRepoUrl}
          containerUri={deployment.container_uri ?? null}
        />
        <DeploymentStatus deploymentId={deployment.id} />
        {isTerminalFailure && (
          <Button variant="outline" size="sm" className="self-start" onClick={clearDeployment}>
            Try again
          </Button>
        )}
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-4 rounded-md border border-border p-4">
      <div className="flex items-center gap-2">
        <Rocket className="h-4 w-4 shrink-0 text-muted-foreground" />
        <p className="text-sm font-medium">Deploy {agentDisplayName}</p>
      </div>

      <AboutAgent
        agentDisplayName={agentDisplayName}
        sourceRepoUrl={sourceRepoUrl}
        containerUri={null}
      />

      <div className="flex flex-col gap-3">
        <DeployStepHeading number={1} title="Choose a runtime" />
        <RuntimeTargetPicker selected={runtime} onSelect={setRuntime} />
      </div>

      {runtime === "bedrock-agentcore" && (
        <div className="flex flex-col gap-3 border-t border-border pt-4">
          <DeployStepHeading number={2} title="Deploy into your own AWS account" />
          <ByocRoleSetup agentSlug={agentSlug} onDeployed={(dep) => selectDeployment(dep.id)} />
        </div>
      )}
    </div>
  );
}
