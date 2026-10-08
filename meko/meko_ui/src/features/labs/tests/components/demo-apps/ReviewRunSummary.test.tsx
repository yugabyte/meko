import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

import { ReviewRunSummary } from "@/features/labs/components/demo-apps/ReviewRunSummary";

const USAGE = { input_tokens: 589, output_tokens: 372 };
const FINDING = {
  severity: "critical" as const,
  location: "api/spend.py:15",
  comment: "Interpolates team_id into SQL without parameterization.",
};

function renderSummary(findings: Parameters<typeof ReviewRunSummary>[0]["findings"]) {
  render(
    <ReviewRunSummary
      findings={findings}
      usage={USAGE}
      stats={null}
      durationMs={8300}
      modelLabel="Ministral 14B"
      enabledTools={["knowledgebase_search"]}
    />,
  );
}

afterEach(cleanup);

describe("ReviewRunSummary", () => {
  it("counts real findings", () => {
    renderSummary([FINDING]);

    expect(screen.getByText("1 finding")).toBeTruthy();
    expect(screen.getByText("1 critical")).toBeTruthy();
  });

  // This is the first screen of Chapter 3, so a confident "0 findings · 0
  // critical · 0 major" over a review that found six is the most damaging
  // place to report an empty list as zero.
  it("claims no count for an empty list", () => {
    renderSummary([]);

    expect(screen.queryByText(/0 findings/)).toBeNull();
    expect(screen.queryByText(/0 critical/)).toBeNull();
    expect(screen.getByText(/no countable findings/i)).toBeTruthy();
  });

  it("claims no count for a prose answer", () => {
    renderSummary("unstructured");

    expect(screen.queryByText(/0 findings/)).toBeNull();
    expect(screen.getByText(/no countable findings/i)).toBeTruthy();
  });

  it("does not describe the pipeline step as parsed comments when nothing counted", () => {
    renderSummary([]);

    expect(screen.getByText(/Nothing to count/i)).toBeTruthy();
    expect(screen.queryByText(/0 comments/)).toBeNull();
  });
});
