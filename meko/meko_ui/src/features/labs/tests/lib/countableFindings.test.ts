import { describe, expect, it } from "vitest";

import { countableFindings } from "@/features/labs/lib/countableFindings";

const FINDING = { severity: "critical" as const, location: "a.py:1", comment: "SQL injection." };

describe("countableFindings", () => {
  it("returns a non-empty list, which is the only state that supports a count", () => {
    expect(countableFindings([FINDING])).toEqual([FINDING]);
  });

  // The whole point. review.py sends [] for a clean diff AND for a response it
  // could not parse, so a count built from it reads as "0 issues found" on a
  // review that actually found six.
  it("claims nothing for an empty list", () => {
    expect(countableFindings([])).toBeNull();
  });

  it("claims nothing for a prose answer or an unfinished run", () => {
    expect(countableFindings("unstructured")).toBeNull();
    expect(countableFindings(null)).toBeNull();
    expect(countableFindings(undefined)).toBeNull();
  });
});
