// Yugabyte's own AgentK deployment, as read from its dashboard on
// PRODUCTION_SNAPSHOT_READ_ON. A CAPTURE, not a live poll: nothing in the lab
// queries that dashboard, so every surface renders these behind the date they
// were read rather than as current numbers. Same contract as
// anthropicReferenceReview.ts and coldReviewCapture.ts.
//
// Refreshing it means opening PRODUCTION_DASHBOARD_URL, reading the figures
// off it, and moving the date with them. Leave a figure out rather than
// guessing it.

/** The day these figures were read. Rendered next to them, always. */
export const PRODUCTION_SNAPSHOT_READ_ON = "17 September 2026";

export const PRODUCTION_DASHBOARD_URL = "https://agentk.dev.yugabyte.com:3000/code-review";

export const PRODUCTION_SNAPSHOT = {
  reposWatched: 104,
  issuesFound: 7_363,
  runs: 1_282,
  prs: 1_032,
  /** Against a reviewer-hour, which is how the dashboard computes it. */
  costSavingsUsd: 13_553,
  costPerPrUsd: 10.61,
  totalSpendUsd: 10_947,
  /** Review rules and gotchas in the Meko corpus AgentK reads. */
  guidelinesInContext: 1_833,
  /** Promoted learnings credited in a posted comment — collective memory
   *  doing in production what Chapter 3 does once. */
  learningsApplied: 14,
  tokens30d: "596.33M",
  tokensIn30d: "276.57M",
  tokensOut30d: "319.76M",
  verdictsUp: 1_677,
  verdictsDown: 104,
  /** Share of labelled comments the engineer who received them judged right. */
  verdictsUpPct: 94,
} as const;

/** Runs per day over the 30 days ending on the snapshot date. The last point
 *  is a partial day, which every reader of the chart is told. */
export const PRODUCTION_RUNS_30D: { day: string; runs: number }[] = [
  { day: "08-19", runs: 14 },
  { day: "08-20", runs: 22 },
  { day: "08-21", runs: 31 },
  { day: "08-22", runs: 19 },
  { day: "08-23", runs: 6 },
  { day: "08-24", runs: 4 },
  { day: "08-25", runs: 41 },
  { day: "08-26", runs: 52 },
  { day: "08-27", runs: 29 },
  { day: "08-28", runs: 33 },
  { day: "08-29", runs: 12 },
  { day: "08-30", runs: 5 },
  { day: "08-31", runs: 48 },
  { day: "09-01", runs: 40 },
  { day: "09-02", runs: 39 },
  { day: "09-03", runs: 38 },
  { day: "09-04", runs: 36 },
  { day: "09-05", runs: 9 },
  { day: "09-06", runs: 7 },
  { day: "09-07", runs: 55 },
  { day: "09-08", runs: 61 },
  { day: "09-09", runs: 48 },
  { day: "09-10", runs: 58 },
  { day: "09-11", runs: 62 },
  { day: "09-12", runs: 55 },
  { day: "09-13", runs: 18 },
  { day: "09-14", runs: 66 },
  { day: "09-15", runs: 88 },
  { day: "09-16", runs: 101 },
  { day: "09-17", runs: 14 },
];

/** Averaged over the charted series itself, rather than copied from the
 *  dashboard's own all-time average: the dashboard computes that over a wider
 *  window, so quoting it beside this 30-day chart put two visibly different
 *  numbers next to each other. */
export const PRODUCTION_AVG_RUNS_PER_DAY = Math.round(
  PRODUCTION_RUNS_30D.reduce((sum, d) => sum + d.runs, 0) / PRODUCTION_RUNS_30D.length,
);

/** Derived rather than restated, so the chart and the total cannot disagree. */
export const PRODUCTION_RUNS_30D_TOTAL = PRODUCTION_RUNS_30D.reduce((sum, d) => sum + d.runs, 0);

export const PRODUCTION_BUSIEST_DAY = PRODUCTION_RUNS_30D.reduce((best, d) =>
  d.runs > best.runs ? d : best,
);
