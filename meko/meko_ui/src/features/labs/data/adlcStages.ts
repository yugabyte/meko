// The four stages of the agentic development lifecycle, shown as a strip
// above the lab list so a visitor can see which part of the loop Labs
// covers today and what is still coming.

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

export const ADLC_STAGES_HINT = "Review is live today. The rest land next.";

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
    title: "Write a functional spec",
    tags: ["Spec"],
    summary:
      "Turn a Slack thread and a bug report into a spec the coding agent can build from, with the decisions traced back to who made them.",
  },
  {
    title: "Ship the fix",
    tags: ["Build"],
    summary:
      "Hand review findings to a coding agent, watch it patch against the same standards, and promote what it learned back to the team.",
  },
];
