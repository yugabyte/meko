// The batch Chapter 4 opens on: six pull requests going past the agent with
// nobody watching, and the one that comes back.
//
// FIXTURE. Nothing here is a real run, and the stage says so on the page.
// It exists because the lab's own review is a single pull request, and one
// review cannot show the thing this chapter is about: what happens on the
// second, third and sixth, when nobody is sitting there pressing Run.
//
// The shape is what matters and it is chosen, not random: five of six close
// without a human, and the one that stops is the destructive migration. An
// agent that auto-approved a dropped table would make the guardrail lesson
// in Chapter 2 a lie.

export type BatchOutcome =
  | { kind: "auto-approved" }
  | { kind: "posted"; comments: number }
  | { kind: "blocking" };

export type BatchRow = { pr: number; title: string; outcome: BatchOutcome };

export const BATCH_ROWS: BatchRow[] = [
  { pr: 419, title: "Add retry to webhook dispatcher", outcome: { kind: "auto-approved" } },
  { pr: 421, title: "Cache tenant plan lookups", outcome: { kind: "auto-approved" } },
  { pr: 422, title: "Bulk export endpoint for invoices", outcome: { kind: "posted", comments: 2 } },
  { pr: 424, title: "Drop legacy usage_rollup table", outcome: { kind: "blocking" } },
  { pr: 425, title: "Rate-limit the reports API", outcome: { kind: "posted", comments: 1 } },
  { pr: 427, title: "Fix timezone drift in period boundaries", outcome: { kind: "auto-approved" } },
];

/** Wall clock for the whole batch. */
export const BATCH_DURATION = "4m 12s";

/** What the batch cost, and on what. The point of the figure is its size
 *  next to six reviews, so the model is quoted with it. */
export const BATCH_COST_USD = 0.02;
export const BATCH_MODEL_LABEL = "8B model";

/** The pull request that stopped, named in the closing line so the reason is
 *  attached to a specific change rather than left as a general claim. */
export const BATCH_BLOCKING_ROW = BATCH_ROWS.find((row) => row.outcome.kind === "blocking");

/** Derived, so the stat row and the table can never disagree. */
export const BATCH_CLOSED_WITHOUT_YOU = BATCH_ROWS.filter(
  (row) => row.outcome.kind !== "blocking",
).length;

export const BATCH_CAME_BACK = BATCH_ROWS.length - BATCH_CLOSED_WITHOUT_YOU;
