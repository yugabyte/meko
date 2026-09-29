import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";

import type { useLabProgress } from "@/features/labs/hooks/useLabProgress";
import type { useLabRunnerSession } from "@/features/labs/hooks/useLabRunnerSession";
import type { LabDefinition, LabStage } from "@/features/labs/types";
import { LabStageSlot } from "@/features/labs/components/lab-runner/LabStageSlot";

// Only PrReviewDemoApp/BringYourOwnPrStage need a controllable mock here --
// these tests are about LabStageSlot's own reviewRun/byocReviewRun state
// (surviving a stage switch, clearing on a datapack switch), not about
// either stage's own internal review flow (see PrReviewDemoApp.test.tsx and
// BringYourOwnPrStage.test.tsx for that).
function stubReviewStage({
  savedRun,
  onRunChange,
}: {
  savedRun?: { reviewText: string | null } | null;
  onRunChange?: (run: unknown) => void;
}) {
  return (
    <div>
      <button onClick={() => onRunChange?.({ reviewText: "Looks good." })}>simulate review complete</button>
      <div data-testid="review-text">{savedRun?.reviewText ?? "(none)"}</div>
    </div>
  );
}
vi.mock("@/features/labs/components/demo-apps/PrReviewDemoApp", () => ({ PrReviewDemoApp: stubReviewStage }));
// Chapter 4's review now lives inside MakeItYoursStage, which also reads a
// datapack rollup over the network -- stubbed to the same review shim so
// these stay tests of LabStageSlot's own state.
vi.mock("@/features/labs/components/stages/MakeItYoursStage", () => ({
  MakeItYoursStage: ({
    byocRun,
    onByocRunChange,
  }: {
    byocRun?: { reviewText: string | null } | null;
    onByocRunChange?: (run: unknown) => void;
  }) => stubReviewStage({ savedRun: byocRun, onRunChange: onByocRunChange }),
}));

const RUN_REVIEW_STAGE: LabStage = {
  id: "run-review",
  title: "Configure & run your review",
  summary: "",
};
const GUARDRAIL_STAGE: LabStage = {
  id: "guardrail",
  title: "Decide where it needs you",
  summary: "",
};
const MAKE_IT_YOURS_STAGE: LabStage = {
  id: "make-it-yours",
  title: "Make it yours",
  summary: "",
};

const LAB = {
  id: "adlc-review-agent",
  demoApp: { kind: "pr-review-chat", agentDisplayName: "AgentK" },
  chapters: [{ id: "c1", title: "Chapter", summary: "", stages: [GUARDRAIL_STAGE, RUN_REVIEW_STAGE] }],
} as unknown as LabDefinition;

const SESSION = { datapackId: "dp-1", deploymentActive: true } as unknown as ReturnType<typeof useLabRunnerSession>;

function fakeSession(datapackId: string): ReturnType<typeof useLabRunnerSession> {
  return { datapackId, deploymentActive: true } as unknown as ReturnType<typeof useLabRunnerSession>;
}

function fakeProgress(stage: LabStage): ReturnType<typeof useLabProgress> {
  return {
    currentStage: stage,
    goToStageById: vi.fn(),
    goToNext: vi.fn(),
  } as unknown as ReturnType<typeof useLabProgress>;
}

const EMPTY_FEED = {
  knowledge: [],
  knowledgeTotal: 0,
  memories: [],
  memoriesTotal: 0,
  collective: [],
  collectiveTotal: 0,
  conversationsTotal: 0,
  chunkSize: null,
  loading: false,
} as never;

afterEach(cleanup);

describe("LabStageSlot", () => {
  it("keeps a completed review's results after navigating to another stage and back", () => {
    const { rerender } = render(<LabStageSlot lab={LAB} session={SESSION} progress={fakeProgress(RUN_REVIEW_STAGE)} contextFeed={EMPTY_FEED} onEndLab={vi.fn()} />);

    expect(screen.getByTestId("review-text").textContent).toBe("(none)");
    fireEvent.click(screen.getByText("simulate review complete"));
    expect(screen.getByTestId("review-text").textContent).toBe("Looks good.");

    // Navigate away -- PrReviewDemoApp's mock unmounts entirely.
    rerender(<LabStageSlot lab={LAB} session={SESSION} progress={fakeProgress(GUARDRAIL_STAGE)} contextFeed={EMPTY_FEED} onEndLab={vi.fn()} />);
    expect(screen.queryByTestId("review-text")).toBeNull();

    // Navigate back -- the same LabStageSlot instance re-passes its own
    // reviewRun state back in as savedRun, so the result is still there.
    rerender(<LabStageSlot lab={LAB} session={SESSION} progress={fakeProgress(RUN_REVIEW_STAGE)} contextFeed={EMPTY_FEED} onEndLab={vi.fn()} />);
    expect(screen.getByTestId("review-text").textContent).toBe("Looks good.");
  });

  it("clears Chapter 4's byocReviewRun once session.datapackId actually changes", () => {
    const { rerender } = render(
      <LabStageSlot lab={LAB} session={fakeSession("dp-1")} progress={fakeProgress(MAKE_IT_YOURS_STAGE)} contextFeed={EMPTY_FEED} onEndLab={vi.fn()} />,
    );
    fireEvent.click(screen.getByText("simulate review complete"));
    expect(screen.getByTestId("review-text").textContent).toBe("Looks good.");

    // Chapter 4's own choose-datapack-again stage can change session.datapackId
    // mid-chapter -- a review run against the OLD datapack must not keep
    // showing (and blocking the fetch form) once the datapack switches.
    rerender(<LabStageSlot lab={LAB} session={fakeSession("dp-2")} progress={fakeProgress(MAKE_IT_YOURS_STAGE)} contextFeed={EMPTY_FEED} onEndLab={vi.fn()} />);
    expect(screen.getByTestId("review-text").textContent).toBe("(none)");
  });
});
