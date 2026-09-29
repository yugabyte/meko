import { describe, expect, it } from "vitest";

import { SAMPLE_DIFF_CONTENT } from "@/features/labs/data/adlcReviewAgentLab/sampleDiff";
import { excerptForFinding, parseFindingLocation, parseUnifiedDiff } from "@/features/labs/lib/diffExcerpt";

describe("parseUnifiedDiff", () => {
  it("numbers lines from the hunk header, in the new file's frame", () => {
    const [file] = parseUnifiedDiff(SAMPLE_DIFF_CONTENT);
    expect(file.path).toBe("api/spend.py");
    // `@@ -10,6 +10,24 @@`, so the first body line is the new file's line 10.
    expect(file.lines[0].lineNumber).toBe(10);
    const concat = file.lines.find((l) => l.text.includes("+ team_id"));
    expect(concat?.kind).toBe("add");
    expect(concat?.lineNumber).toBe(19);
  });

  it("gives a removed line no new-file number, and does not advance the count", () => {
    const diff = [
      "diff --git a/x.py b/x.py",
      "@@ -1,3 +1,3 @@",
      " keep",
      "-gone",
      "+added",
    ].join("\n");
    const [file] = parseUnifiedDiff(diff);
    expect(file.lines.map((l) => [l.kind, l.lineNumber])).toEqual([
      ["context", 1],
      ["del", null],
      ["add", 2],
    ]);
  });
});

describe("parseFindingLocation", () => {
  it("reads a single line, a hyphen range and an en dash range alike", () => {
    expect(parseFindingLocation("/api/spend.py:23")).toEqual({
      path: "api/spend.py",
      start: 23,
      end: 23,
    });
    expect(parseFindingLocation("api/spend.py:14-15")?.end).toBe(15);
    expect(parseFindingLocation("api/spend.py:14–15")?.end).toBe(15);
  });

  // The agent routinely annotates the location. Rejecting these meant most
  // findings rendered no code at all.
  it("ignores a note tacked on after the line number", () => {
    expect(parseFindingLocation("spend.py:15 (string concatenation in SQL)")).toEqual({
      path: "spend.py",
      start: 15,
      end: 15,
    });
    expect(parseFindingLocation("spend.py:13-15 (missing `team_id` validation)")?.end).toBe(15);
  });

  it("returns null for anything it cannot read, rather than a guess", () => {
    expect(parseFindingLocation("api/spend.py")).toBeNull();
    expect(parseFindingLocation("")).toBeNull();
    // A backwards range would render an empty highlight.
    expect(parseFindingLocation("api/spend.py:20-10")).toBeNull();
  });
});

describe("excerptForFinding", () => {
  const files = parseUnifiedDiff(SAMPLE_DIFF_CONTENT);

  it("matches on basename, so a leading slash does not lose the file", () => {
    const excerpt = excerptForFinding(files, "/api/spend.py:19");
    expect(excerpt?.path).toBe("api/spend.py");
    expect(excerpt?.highlightStart).toBe(19);
    expect(excerpt?.lines.some((l) => l.text.includes("+ team_id"))).toBe(true);
  });

  it("clamps a range that runs past the hunk instead of dropping it", () => {
    const excerpt = excerptForFinding(files, "api/spend.py:24-900");
    expect(excerpt?.highlightStart).toBe(24);
    expect(excerpt?.highlightEnd).toBe(31);
  });

  // Highlighting unrelated code is worse than showing none: the reader would
  // take the marked line as the agent's claim.
  it("returns null when the location falls outside every hunk", () => {
    expect(excerptForFinding(files, "api/spend.py:900")).toBeNull();
  });

  it("returns null for a file this diff does not touch", () => {
    const twoFiles = parseUnifiedDiff(
      SAMPLE_DIFF_CONTENT + "\ndiff --git a/b.py b/b.py\n@@ -1,1 +1,1 @@\n+ok\n",
    );
    expect(excerptForFinding(twoFiles, "api/other.py:12")).toBeNull();
  });
});
