import { describe, expect, it } from "vitest";

import { recoverReviewFindings } from "@/features/labs/lib/recoverReviewFindings";

describe("recoverReviewFindings", () => {
  // An EXCERPT of a real Ministral 14B run (datapack adlc-code-review-Oct7,
  // 2026-10-07): the first two of its six findings, kept verbatim. The whole
  // response was discarded by the agent because one location pasted the
  // offending query and left its quotes raw, and the lab then reported "No
  // blocking issues found" on a diff built to carry a SQL injection.
  const REAL_RUN = [
    "```json",
    '{"findings": [',
    '  {"severity": "critical", "location": "request.args.get(\\"team_id\\") (new endpoint)",',
    '   "comment": "No authorization check exists for the new endpoint."},',
    '  {"severity": "critical",',
    '   "location": "SELECT user_id, SUM(tokens) FROM spend WHERE team_id = " + team_id + " GROUP BY user_id",',
    '   "comment": "Interpolates team_id into SQL without parameterization."}',
    "]}",
    "```",
  ].join("\n");

  it("recovers findings the agent dropped over an unescaped quote", () => {
    const findings = recoverReviewFindings(REAL_RUN);

    expect(findings).toHaveLength(2);
    expect(findings?.[1].severity).toBe("critical");
    expect(findings?.[1].comment).toMatch(/without parameterization/);
    // The quoted SQL survives intact rather than being truncated at the
    // stray quote.
    expect(findings?.[1].location).toBe(
      'SELECT user_id, SUM(tokens) FROM spend WHERE team_id = " + team_id + " GROUP BY user_id',
    );
  });

  it("reads a well-formed response without mangling escaped quotes", () => {
    const findings = recoverReviewFindings(
      '{"findings": [{"severity": "major", "location": "a.py:1", "comment": "Use \\"x\\" here."}]}',
    );

    expect(findings).toEqual([{ severity: "major", location: "a.py:1", comment: 'Use "x" here.' }]);
  });

  it("returns null for a genuinely clean review, so [] is left alone", () => {
    expect(recoverReviewFindings('{"findings": []}')).toBeNull();
  });

  it("returns null for prose, which is not a findings response at all", () => {
    expect(recoverReviewFindings("Looks fine to me, nothing to flag.")).toBeNull();
  });

  it("returns null for an absent or empty response", () => {
    expect(recoverReviewFindings(null)).toBeNull();
    expect(recoverReviewFindings("   ")).toBeNull();
  });

  it("falls back to minor for a severity it does not recognise", () => {
    const findings = recoverReviewFindings(
      '{"findings": [{"severity": "blocker", "location": "", "comment": "Hmm."}]}',
    );

    expect(findings?.[0].severity).toBe("minor");
  });
});
