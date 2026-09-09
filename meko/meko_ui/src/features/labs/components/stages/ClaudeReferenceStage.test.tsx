import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

import { ANTHROPIC_REFERENCE_MODEL_LABEL } from "../../data/adlcReviewAgentLab/anthropicReferenceReview";
import { ClaudeReferenceStage } from "./ClaudeReferenceStage";

afterEach(cleanup);

describe("ClaudeReferenceStage", () => {
  it("shows the Claude reference review before the user configures their own", () => {
    render(<ClaudeReferenceStage agentDisplayName="AgentK" />);

    expect(screen.getAllByText(new RegExp(ANTHROPIC_REFERENCE_MODEL_LABEL)).length).toBeGreaterThan(0);
  });
});
