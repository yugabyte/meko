// Typed shape for a runnable Lab. Adding lab #2 means adding one more
// LabDefinition object to data/labDefinitions.ts (+ a new demo-app component
// and a `case` in LabStageSlot if it needs a demoApp.kind that doesn't
// exist yet) — LabsListPage / LabDetailPage / LabRunnerPage never change.

import type { LucideIcon } from "lucide-react";

/** One of the fixed workflow stages a lab run walks through, grouped into
 *  chapters (see LabChapter below). A closed union (not an arbitrary string)
 *  because LabStageSlot switches on it to pick a concrete stage component —
 *  adding a stage means adding a case there too, same convention LabStageSlot
 *  uses elsewhere for demoApp.kind. */
export type LabStageId =
  // Chapter 1 — give your agent context
  | "see-the-pr"
  | "create-datapack"
  | "knowledge-base"
  | "memory"
  | "conversation"
  // Chapter 2 — share it with AgentK
  | "meet-agentk"
  | "share-with-agent"
  | "configure-agentk"
  | "retrieval-cost"
  | "guardrail"
  // Chapter 3 — read the review
  | "run-review"
  | "review-findings"
  | "review-cost"
  | "collaborate"
  // Chapter 4 — build a dashboard
  | "dashboard-intro"
  | "create-amp-cluster"
  | "run-review-again"
  | "review-dashboard"
  // Chapter 4 — close the loop
  | "save-findings"
  | "make-it-yours";

/** Blocks "Next" until the stage reports its own work done, by calling the
 *  `onComplete` LabStageSlot hands it. Completion is tracked per lab in
 *  useLabProgress (not derived from datapack counts, which would already be
 *  satisfied for a user reusing a datapack they filled in an earlier run).
 *  `message` is the reason shown beside the disabled button. Omit `gate`
 *  entirely for pure explainers -- those advance freely. */
export type LabStageGate = { message: string };

export type LabStage = {
  id: LabStageId;
  /** Heading shown on this stage's left-rail nav card and in LabDetailPage's
   *  overview list -- not rendered inside the stage's own content panel,
   *  which titles itself. */
  title: string;
  /** One or two sentences shown under the title on the left-rail nav card. */
  summary: string;
  /** Present when finishing this stage's work is required to move on. */
  gate?: LabStageGate;
};

/** Groups a lab's stages into named chapters -- e.g. "Setup your Context"
 *  containing several stages, "Review cycle" containing others. LabStageNav
 *  always shows the current chapter's header and only its own stages;
 *  moving between chapters is an explicit "Next Chapter" / "Previous
 *  chapter" action, not an implicit consequence of finishing a chapter's
 *  last stage. */
export type LabChapter = {
  id: string;
  title: string;
  /** One or two sentences shown on LabDetailPage's chapter overview --
   *  the landing page shows only chapters, not individual stages. */
  summary: string;
  stages: LabStage[];
  /** True if this chapter needs the AMP (Postgres databases) feature turned
   *  on to make sense -- getLabDefinition (data/labDefinitions.ts) drops the
   *  chapter entirely when isAmpEnabled() is false, rather than showing it
   *  and having every stage inside immediately degrade. */
  requiresAmp?: boolean;
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
  /** One-sentence hook shown under the title on LabDetailPage -- the
   *  landing page's flow diagram and chapter list carry the detail, so
   *  this should stay short. */
  description: string;
  tags: string[];
  icon: LucideIcon;
  estimatedMinutes: number;
  chapters: LabChapter[];
  demoApp: LabDemoAppConfig;
  observation: LabObservationConfig;
};
