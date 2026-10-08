// What a review's findings can honestly be reported as.
//
// Shared by every Chapter 3 surface that quotes a number, so they cannot
// answer the same run differently.

import type { ReviewFinding } from "../data/adlcReviewAgentLab/anthropicReferenceReview";

/**
 * The findings, or null when no count can honestly be claimed.
 *
 * `reviewFindings` has three states (see PrReviewRunState): an array,
 * `"unstructured"`, or null for a review that has not finished. Only a
 * NON-EMPTY array supports a count.
 *
 * An empty array does not. review.py sends `findings: []` both for a clean
 * diff and for a response it could not parse, and recoverReviewFindings has
 * already tried to tell them apart — what reaches here is genuinely unknown.
 * Reporting it as "0 issues found" is how a diff carrying a SQL injection was
 * shown as clean on three screens at once.
 */
export function countableFindings(
  reviewFindings: ReviewFinding[] | "unstructured" | null | undefined,
): ReviewFinding[] | null {
  if (!Array.isArray(reviewFindings) || reviewFindings.length === 0) return null;
  return reviewFindings;
}
