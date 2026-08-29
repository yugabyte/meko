// Deploy-agent step: shown inside DeployAgentStage. Orchestrates three
// sub-steps: pick a runtime target (only AgentCore is live; Vercel/Lyzr are
// visible "coming soon"), pick managed vs BYOC, and either deploy
// immediately (managed) or walk the BYOC role-setup wizard.
// agentDisplayName is per-lab (e.g. "AgentK") -- this component is shared
// across labs, so it never hardcodes one lab's agent name.
//
// Deployment selection lives in `session` (useLabRunnerSession), not local
// state -- Run a Review (a different stage entirely) needs to know which
// deployment to invoke, so it has to survive navigating away from this
// stage, the same way session.datapackId already does.

import { useState } from "react";
import { ExternalLink, Loader2, Rocket } from "lucide-react";

import { Button } from "@/components/ui/button";

import { useCreateManagedDeployment } from "../../hooks/useDeployments";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import { AccountModePicker, type AccountMode } from "./deploy/AccountModePicker";
import { ByocRoleSetup } from "./deploy/ByocRoleSetup";
import { DeploymentStatus } from "./deploy/DeploymentStatus";
import { RuntimeTargetPicker, type RuntimeTargetId } from "./deploy/RuntimeTargetPicker";

// Numbered step heading, reused for each stage of the picker so the flow
// reads as one sequence instead of cards just appearing with no context —
// this is what was "disjointed" before: AccountModePicker would show up
// under RuntimeTargetPicker with nothing marking it as the next step.
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
        <span className="font-medium text-foreground">{agentDisplayName}</span> is open source —{" "}
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
  const [runtime, setRuntime] = useState<RuntimeTargetId | null>(null);
  const [accountMode, setAccountMode] = useState<AccountMode | null>(null);
  const createManaged = useCreateManagedDeployment();
  const { deployment, selectDeployment, clearDeployment } = session;

  if (deployment) {
    // failed/deleted are terminal (useDeploymentStatus stops polling) with
    // no automatic way forward -- offer a way back to the picker instead of
    // leaving the user stuck on a dead "Deploying" panel.
    const isTerminalFailure = deployment.status === "failed" || deployment.status === "deleted";
    return (
      <div className="flex flex-col gap-3 rounded-md border border-border p-4">
        <p className="text-sm font-medium">
          {isTerminalFailure ? `Couldn't deploy ${agentDisplayName}` : `Deploying ${agentDisplayName}`}
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

      <AboutAgent agentDisplayName={agentDisplayName} sourceRepoUrl={sourceRepoUrl} containerUri={null} />

      <div className="flex flex-col gap-3">
        <DeployStepHeading number={1} title="Choose a runtime" />
        <RuntimeTargetPicker selected={runtime} onSelect={setRuntime} />
      </div>

      {runtime === "bedrock-agentcore" && (
        <div className="flex flex-col gap-3 border-t border-border pt-4">
          <DeployStepHeading number={2} title="Choose an AWS account" />
          <AccountModePicker selected={accountMode} onSelect={setAccountMode} />
        </div>
      )}

      {accountMode !== null && (
        <div className="flex flex-col gap-3 border-t border-border pt-4">
          <DeployStepHeading number={3} title="Deploy" />

          {accountMode === "managed" && (
            <div className="flex flex-col gap-2">
              <Button
                onClick={() =>
                  createManaged.mutate(agentSlug, { onSuccess: (dep) => selectDeployment(dep.id) })
                }
                disabled={createManaged.isPending}
                className="self-start"
              >
                {createManaged.isPending && <Loader2 className="h-4 w-4 animate-spin" />}
                Deploy to Meko's managed AWS account
              </Button>
              {createManaged.isError && (
                <p className="text-sm text-destructive">
                  {createManaged.error instanceof Error ? createManaged.error.message : "Deploy failed."}
                </p>
              )}
            </div>
          )}

          {accountMode === "byoc" && (
            <ByocRoleSetup agentSlug={agentSlug} onDeployed={(dep) => selectDeployment(dep.id)} />
          )}
        </div>
      )}
    </div>
  );
}
