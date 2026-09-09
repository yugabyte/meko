import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

import { SAMPLE_DIFF_TITLE } from "../../data/adlcReviewAgentLab/sampleDiff";
import { ReviewSamplePrStage } from "./ReviewSamplePrStage";

afterEach(cleanup);

describe("ReviewSamplePrStage", () => {
  it("shows the sample diff, clearly marked as sample content", () => {
    render(<ReviewSamplePrStage agentDisplayName="AgentK" />);

    expect(screen.getByText(SAMPLE_DIFF_TITLE)).toBeTruthy();
    expect(screen.getByText("Sample PR")).toBeTruthy();
  });
});
