// Guards the identity contract LabStageSlot has with PrReviewDemoApp: that
// component lists onRunChange in the dependency array of the effect that
// CALLS it, and the effect hands back a freshly built object each time. A
// callback that changes identity per render therefore loops forever --
// effect fires, state changes, parent re-renders, new identity, effect fires.
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, fireEvent } from "@testing-library/react";
import { useEffect } from "react";

import type { LabDefinition, LabStage } from "@/features/labs/types";
import { LabStageSlot } from "@/features/labs/components/lab-runner/LabStageSlot";

const seen = new Set<unknown>();

// Stands in for PrReviewDemoApp, reproducing the one behaviour that matters:
// an effect that depends on onRunChange and calls it with a new object.
vi.mock("@/features/labs/components/demo-apps/PrReviewDemoApp", () => ({
  PrReviewDemoApp: ({ onRunChange }: { onRunChange?: (r: unknown) => void }) => {
    seen.add(onRunChange);
    useEffect(() => {
      onRunChange?.({ messages: [], reviewUsage: null, selectedModelId: null });
    }, [onRunChange]);
    return <div data-testid="demo" />;
  },
}));

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

afterEach(() => {
  cleanup();
  seen.clear();
  localStorage.clear();
});

const RUN_REVIEW: LabStage = {
  id: "run-review",
  title: "Run the review",
  summary: "",
};

const LAB = {
  id: "adlc-review-agent",
  demoApp: { kind: "pr-review-chat", agentDisplayName: "AgentK" },
  chapters: [{ id: "c", title: "C", summary: "", stages: [RUN_REVIEW] }],
} as unknown as LabDefinition;

const SESSION = {
  datapackId: "dp-1",
  deploymentActive: true,
  deployment: { status: "active" },
} as never;

// useLabProgress returns these from useCallback, so they hold across renders
// -- the fake has to match, or it tests its own instability instead of
// LabStageSlot's.
const goToStageById = () => {};
const markStageComplete = () => {};

function fakeProgress() {
  return { currentStage: RUN_REVIEW, goToStageById, markStageComplete } as never;
}

describe("LabStageSlot run-state callbacks", () => {
  it("hands PrReviewDemoApp the same onRunChange across re-renders", () => {
    const { rerender } = render(
      <LabStageSlot
        lab={LAB}
        session={SESSION}
        progress={fakeProgress()}
        contextFeed={EMPTY_FEED}
        onEndLab={vi.fn()}
      />,
    );
    expect(screen.getByTestId("demo")).toBeTruthy();
    rerender(
      <LabStageSlot
        lab={LAB}
        session={SESSION}
        progress={fakeProgress()}
        contextFeed={EMPTY_FEED}
        onEndLab={vi.fn()}
      />,
    );
    // One identity across both renders. Two means the effect above re-fires
    // on every parent render, which is the render loop.
    expect(seen.size).toBe(1);
  });
});
