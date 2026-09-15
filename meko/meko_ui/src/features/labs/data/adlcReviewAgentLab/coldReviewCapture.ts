// A one-time capture of AgentK reviewing SAMPLE_DIFF_CONTENT with NO
// datapack attached -- the baseline Chapter 1 opens on, before the user has
// built any context.
//
// PLACEHOLDER until someone runs the capture: AgentK has no "review without
// context" mode (the invoke path always carries the datapack), so this is
// produced by invoking the agent against an empty datapack and pasting the
// result here, the same way anthropicReferenceReview.ts is produced. Until
// then COLD_REVIEW_IS_POPULATED is false and SeeThePrStage shows the diff
// alone rather than claiming a result nobody measured.
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

/** The agent's own summary verdict, verbatim from the capture. */
export const COLD_REVIEW_VERDICT = "PENDING_REAL_CAPTURE";

/** Wall-clock the capture took, shown beside the verdict. */
export const COLD_REVIEW_DURATION = "4.6s";

export const COLD_REVIEW_INPUT_TOKENS = 0;
export const COLD_REVIEW_OUTPUT_TOKENS = 0;

/** Same gate as ANTHROPIC_REFERENCE_IS_POPULATED: a real capture always has
 *  non-zero usage, so this is the single source of truth for "has anyone run
 *  the capture yet". */
export const COLD_REVIEW_IS_POPULATED = COLD_REVIEW_INPUT_TOKENS > 0;
