// Typed shape for a runnable Lab. Adding lab #2 means adding one more
// LabDefinition object to data/labDefinitions.ts (+ a new demo-app component
// and a `case` in LabStageSlot if it needs a demoApp.kind that doesn't
// exist yet) — LabsListPage / LabDetailPage / LabRunnerPage never change.

import type { LucideIcon } from "lucide-react";

/** One of the 5 fixed workflow stages a lab run walks through. A closed
 *  union (not an arbitrary string) because LabStageSlot switches on it to
 *  pick a concrete stage component — adding a 6th stage means adding a case
 *  there too, same convention LabStageSlot uses elsewhere for demoApp.kind. */
export type LabStageId = "create-datapack" | "seed-context" | "deploy-agent" | "run-review" | "collaborate";

export type LabStage = {
  id: LabStageId;
  /** Heading shown on this stage's left-rail nav card and in LabDetailPage's
   *  overview list -- not rendered inside the stage's own content panel,
   *  which titles itself. */
  title: string;
  /** One or two sentences shown under the title on the left-rail nav card. */
  summary: string;
  /** Exactly two bullet points shown under this stage in LabDetailPage's
   *  overview timeline — a slightly more concrete "what this does" than
   *  `summary`, which is written for the in-progress runner instead. */
  highlights: [string, string];
};

export type LabDemoAppConfig = {
  kind: "pr-review-chat";
  /** User-facing name for this lab's agent (e.g. "AgentK") — every screen
   *  that used to say "this agent"/"the agent" interpolates this instead,
   *  so a second lab's demo app shows its own agent's name rather than
   *  another lab's. */
  agentDisplayName: string;
  /** Enables the standalone Deploy stage. Only agentSlugs api_server's
   *  DeploymentService recognizes can actually deploy — see
   *  service.SupportedDeploymentAgentSlugs. Its image is resolved by
   *  convention from MEKO_LABS_AGENT_IMAGES_REPO, so adding a new
   *  deployable lab needs no new env var. `sourceRepoUrl` links to the
   *  agent's own source (shown in the Deploy stage's "About" panel) so
   *  users can inspect what they're actually running. */
  deployable?: { agentSlug: string; sourceRepoUrl: string };
};

export type LabObservationConfig = {
  kind: "workbench-live-observation";
};

export type LabDefinition = {
  id: string;
  title: string;
  summary: string;
  description: string;
  tags: string[];
  icon: LucideIcon;
  estimatedMinutes: number;
  stages: LabStage[];
  demoApp: LabDemoAppConfig;
  observation: LabObservationConfig;
};
