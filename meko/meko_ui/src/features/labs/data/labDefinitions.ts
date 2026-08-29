import { GitPullRequest } from "lucide-react";

import type { LabDefinition } from "../types";
import { ADLC_REVIEW_AGENT_LAB_STAGES } from "./adlcReviewAgentLab/stages";

export const ADLC_REVIEW_AGENT_LAB: LabDefinition = {
  id: "adlc-review-agent",
  title: "Review a PR with AgentK",
  summary: "AgentK reviews a pull request against your org's coding standards, live.",
  description:
    "AgentK reviews a pull request against your org's coding standards, maintained in a Meko datapack: it retrieves them from a knowledge base, reviews the PR against them, and every turn is captured as real memories and traces you can watch happen in the panel on the right.",
  tags: ["ADLC", "Code Review"],
  icon: GitPullRequest,
  estimatedMinutes: 5,
  stages: ADLC_REVIEW_AGENT_LAB_STAGES,
  demoApp: {
    kind: "pr-review-chat",
    agentDisplayName: "AgentK",
    deployable: {
      agentSlug: "adlc-review-agent",
      sourceRepoUrl: "https://github.com/yugabyte/canned-agents/tree/main/adlc-review-agent",
    },
  },
  observation: {
    kind: "workbench-live-observation",
  },
};

/** Add a new lab by appending its LabDefinition here. */
export const LAB_DEFINITIONS: LabDefinition[] = [ADLC_REVIEW_AGENT_LAB];

export function getLabDefinition(id: string): LabDefinition | undefined {
  return LAB_DEFINITIONS.find((lab) => lab.id === id);
}
