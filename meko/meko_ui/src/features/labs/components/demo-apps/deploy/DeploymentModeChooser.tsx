// Which AgentK to use: the shared, pre-deployed instance (no wait, no AWS
// account needed) or a dedicated one the user deploys themselves. Same
// 2-card idiom as the (now-removed) account-mode picker, with the dedicated
// card gated behind a paid tier -- shown, not hidden, so the option is
// legible with an upsell rather than silently absent for free-tier users.
//
// Starts expanded (both cards visible, so the choice is informed) and
// collapses to a one-line "Approach: <chosen> · Change" summary the moment
// either card is picked -- comparing two options only matters until you've
// picked one; after that, showing both permanently is exactly the clutter
// this was simplified away from. "Change" re-expands it. Collapse state is
// local to this mount, not persisted -- returning to this stage later shows
// the full picker again, then re-collapses on the next pick.

import { useState } from "react";
import { ChevronDown } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "@/components/ui/tooltip";
import { cn } from "@/lib/utils";

import type { DeploymentMode } from "../../../hooks/useLabRunnerSession";

const MODE_LABELS: Record<DeploymentMode, string> = {
  shared: "Use the predeployed AgentK",
  dedicated: "Deploy your own dedicated AgentK",
};

export function DeploymentModeChooser({
  selected,
  onSelect,
  canDeployDedicated,
}: {
  selected: DeploymentMode;
  onSelect: (mode: DeploymentMode) => void;
  canDeployDedicated: boolean;
}) {
  const [collapsed, setCollapsed] = useState(false);

  const choose = (mode: DeploymentMode) => {
    onSelect(mode);
    setCollapsed(true);
  };

  if (collapsed) {
    return (
      <button
        type="button"
        onClick={() => setCollapsed(false)}
        className="flex w-full items-center justify-between gap-2 rounded-md border border-border px-3 py-2 text-left text-sm transition-colors hover:bg-muted/30"
      >
        <span className="text-muted-foreground">
          Approach: <span className="font-medium text-foreground">{MODE_LABELS[selected]}</span>
        </span>
        <span className="inline-flex items-center gap-1 text-xs text-primary">
          Change
          <ChevronDown className="h-3.5 w-3.5" aria-hidden />
        </span>
      </button>
    );
  }

  return (
    <TooltipProvider delayDuration={200}>
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
        <Card
          role="button"
          tabIndex={0}
          onClick={() => choose("shared")}
          onKeyDown={(e) => {
            if (e.key === "Enter" || e.key === " ") choose("shared");
          }}
          className={cn(
            "cursor-pointer p-4 transition-colors hover:bg-muted/30",
            selected === "shared" && "border-primary ring-2 ring-primary/30",
          )}
        >
          <div className="flex items-center justify-between gap-2">
            <p className="text-sm font-medium">Use the predeployed AgentK</p>
            <Badge variant="secondary">No wait</Badge>
          </div>
          <p className="mt-1 text-xs text-muted-foreground">
            A pre-deployed instance ready right now, with no AWS account or deploy needed.
          </p>
        </Card>

        {canDeployDedicated ? (
          <Card
            role="button"
            tabIndex={0}
            onClick={() => choose("dedicated")}
            onKeyDown={(e) => {
              if (e.key === "Enter" || e.key === " ") choose("dedicated");
            }}
            className={cn(
              "cursor-pointer p-4 transition-colors hover:bg-muted/30",
              selected === "dedicated" && "border-primary ring-2 ring-primary/30",
            )}
          >
            <p className="text-sm font-medium">Deploy your own dedicated AgentK</p>
            <p className="mt-1 text-xs text-muted-foreground">
              Stand up your own instance in your own AWS account, on AWS Bedrock AgentCore.
            </p>
          </Card>
        ) : (
          <Tooltip>
            <TooltipTrigger asChild>
              <span className="inline-flex">
                <Card className="pointer-events-none w-full select-none p-4 opacity-70" aria-disabled="true">
                  <div className="flex items-center justify-between gap-2">
                    <p className="text-sm font-medium">Deploy your own dedicated AgentK</p>
                    <Badge variant="outline">Standard tier</Badge>
                  </div>
                  <p className="mt-1 text-xs text-muted-foreground">
                    Stand up your own instance in your own AWS account, on AWS Bedrock AgentCore.
                  </p>
                </Card>
              </span>
            </TooltipTrigger>
            <TooltipContent side="top">Upgrade to Standard to deploy your own dedicated instance.</TooltipContent>
          </Tooltip>
        )}
      </div>
    </TooltipProvider>
  );
}
