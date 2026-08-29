import { GitPullRequest } from "lucide-react";

import type { LabDefinition } from "../types";
import { ADLC_REVIEW_AGENT_LAB_CHAPTERS } from "./adlcReviewAgentLab/steps";

export const ADLC_REVIEW_AGENT_LAB: LabDefinition = {
  id: "adlc-review-agent",
  title: "Review a PR with an agent that remembers",
  summary: "An agent reviews a pull request against your org's coding standards, live.",
  description:
    "This lab walks through a code-review agent built on Meko: it retrieves your org's coding standards from a knowledge base, reviews a pull request against them, and every turn is captured as real memories and traces you can watch happen in the panel on the right.",
  tags: ["ADLC", "Code Review"],
  icon: GitPullRequest,
  estimatedMinutes: 5,
  chapters: ADLC_REVIEW_AGENT_LAB_CHAPTERS,
  demoApp: {
    kind: "pr-review-chat",
    persona: "review_agent",
    datapackName: "ADLC Review Agent Lab",
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
