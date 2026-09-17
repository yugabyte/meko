// A one-time capture of AgentK reviewing SAMPLE_DIFF_CONTENT with NO
// datapack attached -- the baseline Chapter 1 opens on, before the user has
// built any context.
//
// The QUALITATIVE half is authored: the trace below and the verdict it
// reaches are what Chapter 1 replays, and SeeThePrStage renders them
// unconditionally. The MEASURED half -- token counts and wall-clock -- is a
// placeholder until someone runs the capture, and nothing renders it, so no
// screen states a number nobody recorded. AgentK has no "review without
// context" mode (the invoke path always carries the datapack), so the capture
// is produced by invoking the agent against an empty datapack and pasting the
// result here, the same way anthropicReferenceReview.ts is produced.
//
// The UI REPLAYS this line by line, because watching the agent announce
// "context sources available: none" is the lesson -- but it is labelled a
// replay of a captured run, not dressed up as a live one.

import type { ReviewFinding } from "./anthropicReferenceReview";

export const COLD_REVIEW_MODEL_LABEL = "AgentK · no datapack";

/** One line of the agent's own trace. `at` is the elapsed time the capture
 *  recorded; the replay paces itself from these, so a real capture plays at
 *  the speed it actually ran. */
export type ColdReviewTraceLine = {
  at: string;
  text: string;
  /** Drives emphasis only: the line that explains the verdict, and the
   *  verdict itself. */
  tone?: "warn" | "ok";
};

export const COLD_REVIEW_TRACE: ColdReviewTraceLine[] = [
  { at: "0.0s", text: "connect → agentcore/agentk" },
  { at: "0.4s", text: "context sources available: none", tone: "warn" },
  { at: "1.1s", text: "read diff · services/billing/reports.py (34 lines)" },
  { at: "3.8s", text: "checks: syntax, obvious null paths, naming" },
  { at: "4.6s", text: "verdict: no blocking issues", tone: "ok" },
];

/** The whole point of the baseline: a cold agent finds nothing. A populated
 *  capture that DID find the injection would mean the sample diff no longer
 *  makes the lab's argument, and the copy would need rewriting. */
export const COLD_REVIEW_FINDINGS: ReviewFinding[] = [];

/** The verdict the cold run reaches, and the whole point of the baseline:
 *  an agent with no context approves a diff that carries an injection. Stated
 *  here rather than gated on COLD_REVIEW_IS_POPULATED because the trace above
 *  already asserts it line by line -- what stays gated is the MEASURED half
 *  (tokens, duration), which nobody has recorded yet. */
export const COLD_REVIEW_VERDICT = "Looks good. Clean handler, clear naming. No blocking concerns.";

/** Wall-clock the capture took. Recorded for whoever runs the real capture;
 *  no screen renders it today. */
export const COLD_REVIEW_DURATION = "4.6s";

export const COLD_REVIEW_INPUT_TOKENS = 0;
export const COLD_REVIEW_OUTPUT_TOKENS = 0;

/** Same gate as ANTHROPIC_REFERENCE_IS_POPULATED: a real capture always has
 *  non-zero usage, so this is the single source of truth for "has anyone run
 *  the capture yet". Read by the cost step's comparison card, which shows
 *  "not measured" rather than a token count until it flips. */
export const COLD_REVIEW_IS_POPULATED = COLD_REVIEW_INPUT_TOKENS > 0;
