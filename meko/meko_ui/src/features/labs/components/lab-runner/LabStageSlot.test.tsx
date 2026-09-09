import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";

import type { useLabProgress } from "../../hooks/useLabProgress";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import type { LabDefinition, LabStage } from "../../types";
import { LabStageSlot } from "./LabStageSlot";

// Only PrReviewDemoApp needs a controllable mock here -- this test is about
// LabStageSlot's own reviewRun state surviving a switch to a different
// stage and back, not about the review flow itself (see
// PrReviewDemoApp.test.tsx for that).
vi.mock("../demo-apps/PrReviewDemoApp", () => ({
  PrReviewDemoApp: ({
    savedRun,
    onRunChange,
  }: {
    savedRun?: { reviewText: string | null } | null;
    onRunChange?: (run: unknown) => void;
  }) => (
    <div>
      <button onClick={() => onRunChange?.({ reviewText: "Looks good." })}>simulate review complete</button>
      <div data-testid="review-text">{savedRun?.reviewText ?? "(none)"}</div>
    </div>
  ),
}));

const RUN_REVIEW_STAGE: LabStage = {
  id: "run-review",
  title: "Configure & run your review",
  summary: "",
  highlights: ["a", "b"],
};
const CONTEXT_INTRO_STAGE: LabStage = {
  id: "context-intro",
  title: "What is context?",
  summary: "",
  highlights: ["a", "b"],
};

const LAB = {
  id: "adlc-review-agent",
  demoApp: { kind: "pr-review-chat", agentDisplayName: "AgentK" },
  chapters: [{ id: "c1", title: "Chapter", summary: "", stages: [CONTEXT_INTRO_STAGE, RUN_REVIEW_STAGE] }],
} as unknown as LabDefinition;

const SESSION = { datapackId: "dp-1", deploymentActive: true } as unknown as ReturnType<typeof useLabRunnerSession>;

function fakeProgress(stage: LabStage): ReturnType<typeof useLabProgress> {
  return {
    currentStage: stage,
    goToStageById: vi.fn(),
    goToNext: vi.fn(),
  } as unknown as ReturnType<typeof useLabProgress>;
}

afterEach(cleanup);

describe("LabStageSlot", () => {
  it("keeps a completed review's results after navigating to another stage and back", () => {
    const { rerender } = render(<LabStageSlot lab={LAB} session={SESSION} progress={fakeProgress(RUN_REVIEW_STAGE)} />);

    expect(screen.getByTestId("review-text").textContent).toBe("(none)");
    fireEvent.click(screen.getByText("simulate review complete"));
    expect(screen.getByTestId("review-text").textContent).toBe("Looks good.");

    // Navigate away -- PrReviewDemoApp's mock unmounts entirely.
    rerender(<LabStageSlot lab={LAB} session={SESSION} progress={fakeProgress(CONTEXT_INTRO_STAGE)} />);
    expect(screen.queryByTestId("review-text")).toBeNull();

    // Navigate back -- the same LabStageSlot instance re-passes its own
    // reviewRun state back in as savedRun, so the result is still there.
    rerender(<LabStageSlot lab={LAB} session={SESSION} progress={fakeProgress(RUN_REVIEW_STAGE)} />);
    expect(screen.getByTestId("review-text").textContent).toBe("Looks good.");
  });
});
