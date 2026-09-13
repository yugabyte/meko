import { GitPullRequest } from "lucide-react";

import { isAmpEnabled } from "@/lib/amp";

import type { LabDefinition } from "../types";
import { ADLC_REVIEW_AGENT_LAB_CHAPTERS } from "./adlcReviewAgentLab/chapters";

export const ADLC_REVIEW_AGENT_LAB: LabDefinition = {
  id: "adlc-review-agent",
  title: "Review a PR with AgentK",
  summary: "AgentK reviews a pull request against your org's coding standards, live.",
  description:
    "A sample project with no coding standards yet — you'll build context with a Meko datapack, then watch AgentK use it for reviewing a PR.",
  tags: ["ADLC", "Code review"],
  icon: GitPullRequest,
  estimatedMinutes: 5,
  startingPoint: [
    { title: "Sample project", subtitle: "No standards yet, ungoverned" },
    { title: "Build context", subtitle: "Knowledge, memory, conversation" },
    { title: "AgentK reviews", subtitle: "Against what you built" },
  ],
  chapters: ADLC_REVIEW_AGENT_LAB_CHAPTERS,
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

/** Looks up a lab and drops any chapter whose `requiresAmp` isn't met yet --
 *  LAB_DEFINITIONS itself stays the full, ungated list (labDefinitions.test.ts
 *  and anything else that wants every chapter reads that directly); this is
 *  the one funnel (LabRunnerPage, LabDetailPage) that actually renders a
 *  lab's chapters to a user. */
export function getLabDefinition(id: string): LabDefinition | undefined {
  const lab = LAB_DEFINITIONS.find((l) => l.id === id);
  if (!lab) return undefined;
  if (isAmpEnabled()) return lab;
  const chapters = lab.chapters.filter((chapter) => !chapter.requiresAmp);
  if (chapters.length === lab.chapters.length) return lab;
  return { ...lab, chapters };
}
