// Chapter 2's autonomy lesson: where the agent stops and asks a human.
// The three levels all behave identically today — Labs' AgentK doesn't post
// to a PR, so there is no autonomy to enforce yet. Stated on the page rather
// than implied, so the control doesn't read as wired up when it isn't.

import { useState } from "react";

import { Badge } from "@/components/ui/badge";

import { LabChoiceCard } from "../LabChoiceCard";
import { LabStageHeader } from "./LabStageHeader";

type Autonomy = "comment" | "draft";

const DEFAULT_AUTONOMY: Autonomy = "comment";

export function GuardrailStage({ agentDisplayName }: { agentDisplayName: string }) {
  const [autonomy, setAutonomy] = useState<Autonomy>(DEFAULT_AUTONOMY);

  return (
    <div className="flex flex-1 flex-col gap-6 overflow-y-auto p-6">
      <LabStageHeader
        eyebrow="Chapter 2 · Step 4"
        title="Decide where it needs you"
        lede="An autonomous agent is only useful if you know where it stops. This is the human-in-the-loop line for the review stage."
        tags={["Guardrail"]}
      />

      <div className="flex flex-col gap-3">
        <LabChoiceCard
          title="Post comments, ask before blocking"
          badge={
            <Badge variant="secondary" className="text-[10px]">
              recommended
            </Badge>
          }
          body={`${agentDisplayName} writes the review on its own. Anything that would block the merge waits for your approval.`}
          selected={autonomy === "comment"}
          onSelect={() => setAutonomy("comment")}
        />
        <LabChoiceCard
          title="Draft only"
          body="Nothing reaches the pull request until you approve every comment."
          selected={autonomy === "draft"}
          onSelect={() => setAutonomy("draft")}
        />
      </div>

      <p className="text-xs text-muted-foreground">
        Whichever you pick, the full trace is kept: the prompt, the rules retrieved, the reasoning, and who
        approved what. In this lab both behave the same, because {agentDisplayName} reviews here rather than
        posting to a pull request, so there's nothing yet for the guardrail to hold back.
      </p>
    </div>
  );
}
