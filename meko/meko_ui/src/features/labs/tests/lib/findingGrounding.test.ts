import { describe, expect, it } from "vitest";

import {
  groundFinding,
  overlapScore,
  type RetrievedSource,
} from "@/features/labs/lib/findingGrounding";

const SOURCES: RetrievedSource[] = [
  {
    kind: "knowledge",
    source: "org-coding-standards.md",
    quote: "Never build a SQL string by concatenation. Use parameterized queries.",
  },
  {
    kind: "knowledge",
    source: "org-coding-standards.md",
    quote: "Every query on a multi-tenant table must be scoped by the caller's tenant.",
  },
  {
    kind: "memory",
    source: "private memory",
    quote: "We got burned by a SQL injection in the search endpoint last quarter.",
  },
];

describe("overlapScore", () => {
  it("stems, so a comment's verb reaches the rule's noun", () => {
    expect(
      overlapScore("string-concatenating raw input", "built by concatenation"),
    ).toBeGreaterThan(0);
  });

  it("ignores stopwords, so two unrelated sentences do not pair", () => {
    expect(overlapScore("that should always be here", "this must never be there")).toBe(0);
  });

  it("weights a distinctive word above a vague one", () => {
    expect(overlapScore("parameterized queries", "use parameterized queries")).toBeGreaterThan(
      overlapScore("rows returned", "rows counted"),
    );
  });
});

describe("groundFinding", () => {
  it("puts the rule the comment actually echoes first", () => {
    const grounded = groundFinding(
      "SQL injection vulnerability by string-concatenating raw team_id without parameterization.",
      SOURCES,
    );
    expect(grounded[0].quote).toContain("concatenation");
    expect(grounded.map((g) => g.kind)).toContain("memory");
  });

  // One shared word is nearly always the language or the file, which would
  // pair a finding with a rule it has nothing to do with.
  it("returns nothing when only a single word is shared", () => {
    expect(groundFinding("Rename this variable for clarity.", SOURCES)).toEqual([]);
  });

  it("caps how many sources one finding claims", () => {
    expect(
      groundFinding("SQL injection query concatenation tenant", SOURCES, { limit: 1 }),
    ).toHaveLength(1);
  });
});
