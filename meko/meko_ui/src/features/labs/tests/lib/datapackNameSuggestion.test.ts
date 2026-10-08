import { describe, expect, it } from "vitest";

import { suggestDatapackName } from "@/features/labs/lib/datapackNameSuggestion";

const OCT_7 = new Date(2026, 9, 7);

describe("suggestDatapackName", () => {
  it("dates the name so runs on different days do not collide", () => {
    expect(suggestDatapackName([], OCT_7)).toBe("adlc-code-review-Oct7");
    expect(suggestDatapackName([], new Date(2026, 9, 8))).toBe("adlc-code-review-Oct8");
  });

  // Restarting the lab the same day is the common case: the date alone gives
  // back a name POST /datapacks rejects as a duplicate, stranding the user on
  // step 2.
  it("counts the run when today's name is already taken", () => {
    expect(suggestDatapackName(["adlc-code-review-Oct7"], OCT_7)).toBe("adlc-code-review-Oct7-2");
  });

  it("keeps counting past the first suffix", () => {
    expect(
      suggestDatapackName(
        ["adlc-code-review-Oct7", "adlc-code-review-Oct7-2", "adlc-code-review-Oct7-3"],
        OCT_7,
      ),
    ).toBe("adlc-code-review-Oct7-4");
  });

  it("fills a gap rather than always taking the highest", () => {
    expect(suggestDatapackName(["adlc-code-review-Oct7", "adlc-code-review-Oct7-3"], OCT_7)).toBe(
      "adlc-code-review-Oct7-2",
    );
  });

  // Readability, not a backend constraint: the uniqueness index is on a plain
  // VARCHAR matched with `=`, so these two would coexist. Suggesting a name
  // that differs only by case is just confusing.
  it("treats a name differing only in case or padding as taken", () => {
    expect(suggestDatapackName(["  ADLC-Code-Review-Oct7 "], OCT_7)).toBe(
      "adlc-code-review-Oct7-2",
    );
  });

  it("ignores unrelated datapacks", () => {
    expect(suggestDatapackName(["meko_issues", "adlc-code-review-Sep30"], OCT_7)).toBe(
      "adlc-code-review-Oct7",
    );
  });
});
