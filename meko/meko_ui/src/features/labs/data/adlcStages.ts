// The four stages of the agentic development lifecycle, shown as a strip on a
// lab's landing page so a visitor can see which part of the loop that lab
// covers and what is still coming.

export type AdlcStage = {
  label: string;
  /** False for the stages that have no lab yet — rendered dimmed. */
  live: boolean;
};

export const ADLC_STAGES: AdlcStage[] = [
  { label: "Spec", live: false },
  { label: "Build", live: false },
  { label: "Review", live: true },
  { label: "Fix & merge", live: false },
];

/** The two lifecycles a visitor is likely to be conflating, one tile each.
 *  `context` is split out of `body` rather than being another sentence in it
 *  because it is the point: both lifecycles are capped by what their agents
 *  can read, which is the whole reason Labs exists. Keeping it a separate
 *  field lets the tile give it its own emphasis instead of burying it as the
 *  tail of a paragraph. Global framing, not per-lab copy, which is why it
 *  sits beside ADLC_STAGES rather than in a LabDefinition. */
export type LifecycleDefinition = {
  term: string;
  expansion: string;
  body: string;
  context: string;
};

export const LIFECYCLE_DEFINITIONS: LifecycleDefinition[] = [
  {
    term: "ADLC",
    expansion: "Agentic Development Life Cycle",
    body: "Agents run your software lifecycle: spec, build, review, fix.",
    context:
      "An agent with no context guesses at your conventions. Give it your standards, your past decisions and the reasons behind them, and it reviews the way your team would.",
  },
  {
    term: "AIDLC",
    expansion: "AI Development Life Cycle",
    body: "You build and ship the agents themselves.",
    context:
      "The model is the commodity; your context is not. What your agent knows about your business is the only part a competitor cannot buy off the shelf.",
  },
];

/** Labs that don't exist yet. Deliberately NOT LabDefinitions — they have no
 *  chapters to run, and putting them in LAB_DEFINITIONS would mean every
 *  invariant that registry enforces (unique stage ids, non-empty summaries)
 *  had to be faked for a card nobody can open. */
export type UpcomingLab = {
  title: string;
  tags: string[];
  summary: string;
};

export const UPCOMING_LABS: UpcomingLab[] = [
  {
    title: "Financial Services / fraud detection",
    tags: ["Financial services"],
    summary:
      "Ground a fraud-detection agent in your own policies, thresholds and past analyst decisions, so it escalates what your team would.",
  },
];
