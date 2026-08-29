// Typed shape for a runnable Lab. Adding lab #2 means adding one more
// LabDefinition object to data/labDefinitions.ts (+ a new demo-app component
// and a `case` in LabDemoAppSlot if it needs a demoApp.kind that doesn't
// exist yet) — LabsListPage / LabDetailPage / LabRunnerPage never change.

import type { LucideIcon } from "lucide-react";

export type LabChapter = {
  id: string;
  title: string;
  steps: LabStep[];
};

export type LabStep = {
  id: string;
  title: string;
  /** Plain-text/markdown-ish body rendered in the steps rail. */
  body: string;
  /** Fires a side effect when this step is first reached. Kept as a named
   *  enum (not an arbitrary callback) so LabDefinition stays serializable
   *  data, not code. */
  onEnterAction?: "seedKnowledge";
};

export type LabDemoAppConfig = {
  kind: "pr-review-chat";
  /** Persona key forwarded to the inference gateway's /chat `persona`
   *  field — see inference_gateway/chain.py's PERSONA_TEMPLATES. */
  persona: string;
  /** Name of the persistent datapack this lab's demo app is scoped to.
   *  Resolved-or-created once per account by useLabRunnerSession. */
  datapackName: string;
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
  chapters: LabChapter[];
  demoApp: LabDemoAppConfig;
  observation: LabObservationConfig;
};
