import { GitPullRequest } from "lucide-react";

import { isAmpEnabled } from "@/lib/amp";

import type { LabDefinition } from "../types";
import { ADLC_REVIEW_AGENT_LAB_CHAPTERS } from "./adlcReviewAgentLab/chapters";

export const ADLC_REVIEW_AGENT_LAB: LabDefinition = {
  id: "adlc-review-agent",
  title: "Agentic coding review (ADLC)",
  summary: "AgentK reviews a pull request against your org's coding standards, live.",
  description:
    "Build a Meko datapack from your team's standards, then watch AgentK review a real pull request with it, and see exactly what that context changed.",
  tags: ["ADLC", "Code review"],
  icon: GitPullRequest,
  estimatedMinutes: 5,
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
/** Memoised because the AMP-filtered branch below BUILDS a lab object, and
 *  this is called during render: returning a fresh object (and a fresh
 *  `chapters` array) on every call gives every hook keyed on `lab` or
 *  `lab.chapters` a new identity each render, so their useMemo/useCallback
 *  never hold and any effect depending on one re-fires forever. Keyed on the
 *  AMP flag too, since that decides which shape is correct. */
const gatedLabCache = new Map<string, LabDefinition>();

export function getLabDefinition(id: string): LabDefinition | undefined {
  const lab = LAB_DEFINITIONS.find((l) => l.id === id);
  if (!lab) return undefined;
  if (isAmpEnabled()) return lab;
  const chapters = lab.chapters.filter((chapter) => !chapter.requiresAmp);
  if (chapters.length === lab.chapters.length) return lab;
  const cacheKey = `${id}:amp-off`;
  const cached = gatedLabCache.get(cacheKey);
  if (cached) return cached;
  const gated = { ...lab, chapters };
  gatedLabCache.set(cacheKey, gated);
  return gated;
}
