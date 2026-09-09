import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

import { PrDiffViewer } from "./PrDiffViewer";

const DIFF = `diff --git a/a.py b/a.py
index 1..2 100644
--- a/a.py
+++ b/a.py
@@ -1,1 +1,2 @@
 line one
+line two
`;

afterEach(cleanup);

describe("PrDiffViewer", () => {
  it("shows a Sample PR badge when isSample is set", () => {
    render(<PrDiffViewer title="Add a thing" diff={DIFF} isSample />);
    expect(screen.getByText("Sample PR")).toBeTruthy();
  });

  it("shows no Sample PR badge by default", () => {
    render(<PrDiffViewer title="Add a thing" diff={DIFF} />);
    expect(screen.queryByText("Sample PR")).toBeNull();
  });
});
