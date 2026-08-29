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
  /** Heading shown atop the stage's content panel and in LabDetailPage's overview list. */
  title: string;
  /** Terse label for the full-width top stepper strip. */
  shortLabel: string;
  /** One or two sentences shown under the title in the stage panel. */
  summary: string;
  /** Exactly two bullet points shown under this stage in LabDetailPage's
   *  overview timeline — a slightly more concrete "what this does" than
   *  `summary`, which is written for the in-progress runner instead. */
  highlights: [string, string];
};

export type LabDemoAppConfig = {
  kind: "pr-review-chat";
  /** Persona key forwarded to the inference gateway's /chat `persona`
   *  field — see inference_gateway/chain.py's PERSONA_TEMPLATES. */
  persona: string;
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
