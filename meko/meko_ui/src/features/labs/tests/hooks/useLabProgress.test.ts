import { afterEach, describe, expect, it } from "vitest";
import { act, renderHook } from "@testing-library/react";

import type { LabChapter, LabStage } from "@/features/labs/types";
import { useLabProgress } from "@/features/labs/hooks/useLabProgress";

afterEach(() => localStorage.clear());

const FIXTURE_STAGES: LabStage[] = [
  { id: "create-datapack", title: "Create a datapack", summary: "" },
  { id: "seed-context", title: "Seed context", summary: "" },
  { id: "run-review", title: "Run a review", summary: "" },
];
const ONE_CHAPTER_LAB = { id: "test-lab", chapters: [{ id: "chapter-1", title: "Chapter", stages: FIXTURE_STAGES }] } as never;

describe("useLabProgress — single chapter", () => {
  it("starts at stage 0", () => {
    const { result } = renderHook(() => useLabProgress(ONE_CHAPTER_LAB));
    expect(result.current.stageIndex).toBe(0);
    expect(result.current.currentStage.id).toBe("create-datapack");
  });

  it("goToStage clamps out-of-range indices", () => {
    const { result } = renderHook(() => useLabProgress(ONE_CHAPTER_LAB));
    act(() => result.current.goToStage(0, -5));
    expect(result.current.stageIndex).toBe(0);
    act(() => result.current.goToStage(0, 99));
    expect(result.current.stageIndex).toBe(2);
  });

  it("goToNext is a no-op at the last stage", () => {
    const { result } = renderHook(() => useLabProgress(ONE_CHAPTER_LAB));
    act(() => result.current.goToStage(0, 2));
    act(() => result.current.goToNext());
    expect(result.current.stageIndex).toBe(2);
    expect(result.current.isLastStage).toBe(true);
  });

  it("persists across remounts", () => {
    const { result, unmount } = renderHook(() => useLabProgress(ONE_CHAPTER_LAB));
    act(() => result.current.goToStage(0, 1));
    unmount();

    const { result: result2 } = renderHook(() => useLabProgress(ONE_CHAPTER_LAB));
    expect(result2.current.stageIndex).toBe(1);
  });

  it("falls back to the start reading a pre-restructuring {chapterIndex, stepIndex} blob", () => {
    localStorage.setItem("meko:lab-progress:test-lab", JSON.stringify({ chapterIndex: 2, stepIndex: 1 }));
    const { result } = renderHook(() => useLabProgress(ONE_CHAPTER_LAB));
    expect(result.current.chapterIndex).toBe(0);
    expect(result.current.stageIndex).toBe(0);
  });

  it("clamps a negative persisted stageIndex, instead of indexing off the front", () => {
    localStorage.setItem("meko:lab-progress:test-lab", JSON.stringify({ chapterIndex: 0, stageIndex: -1 }));
    const { result } = renderHook(() => useLabProgress(ONE_CHAPTER_LAB));
    expect(result.current.stageIndex).toBe(0);
    expect(result.current.currentStage.id).toBe("create-datapack");
  });

  it("clamps an out-of-range persisted chapterIndex", () => {
    localStorage.setItem("meko:lab-progress:test-lab", JSON.stringify({ chapterIndex: 5, stageIndex: 0 }));
    const { result } = renderHook(() => useLabProgress(ONE_CHAPTER_LAB));
    expect(result.current.chapterIndex).toBe(0);
  });

  it("goToStageById jumps to the stage with that id", () => {
    const { result } = renderHook(() => useLabProgress(ONE_CHAPTER_LAB));
    act(() => result.current.goToStageById("run-review"));
    expect(result.current.stageIndex).toBe(2);
    expect(result.current.currentStage.id).toBe("run-review");
  });

  it("goToStageById is a no-op for an id that doesn't exist in this lab", () => {
    const { result } = renderHook(() => useLabProgress(ONE_CHAPTER_LAB));
    act(() => result.current.goToStageById("deploy-agent")); // a real LabStageId, just absent from this fixture
    expect(result.current.stageIndex).toBe(0);
  });
});

const TWO_CHAPTERS: LabChapter[] = [
  { id: "setup", title: "Setup", summary: "", stages: [FIXTURE_STAGES[0], FIXTURE_STAGES[1]] },
  { id: "review", title: "Review", summary: "", stages: [FIXTURE_STAGES[2]] },
];
const TWO_CHAPTER_LAB = { id: "test-lab-2", chapters: TWO_CHAPTERS } as never;

describe("useLabProgress — multiple chapters", () => {
  it("goToNext crosses from the last stage of one chapter into the first stage of the next", () => {
    const { result } = renderHook(() => useLabProgress(TWO_CHAPTER_LAB));
    act(() => result.current.goToStage(0, 1)); // last stage of chapter 0
    expect(result.current.isLastStage).toBe(false);
    act(() => result.current.goToNext());
    expect(result.current.chapterIndex).toBe(1);
    expect(result.current.stageIndex).toBe(0);
    expect(result.current.currentStage.id).toBe("run-review");
    expect(result.current.isLastStage).toBe(true);
  });

  it("goToNext at the very last stage of the last chapter is a no-op", () => {
    const { result } = renderHook(() => useLabProgress(TWO_CHAPTER_LAB));
    act(() => result.current.goToStage(1, 0));
    act(() => result.current.goToNext());
    expect(result.current.chapterIndex).toBe(1);
    expect(result.current.stageIndex).toBe(0);
  });

  it("goToStage clamps the stage index to the target chapter's own stage count", () => {
    const { result } = renderHook(() => useLabProgress(TWO_CHAPTER_LAB));
    act(() => result.current.goToStage(1, 99)); // chapter 1 has only 1 stage
    expect(result.current.chapterIndex).toBe(1);
    expect(result.current.stageIndex).toBe(0);
  });
});

describe("useLabProgress — gating and completion", () => {
  const GATED_LAB = {
    id: "gated-lab",
    chapters: [
      {
        id: "chapter-1",
        title: "Chapter",
        stages: [
          {
            id: "knowledge-base",
            title: "Knowledge",
            summary: "",
            gate: { message: "Add a standards doc to continue" },
          },
          { id: "memory", title: "Memory", summary: "" },
          { id: "run-review", title: "Review", summary: "" },
        ],
      },
    ],
  } as never;

  it("reports a gated stage's message until that stage is marked complete", () => {
    const { result } = renderHook(() => useLabProgress(GATED_LAB));
    expect(result.current.gateMessage).toBe("Add a standards doc to continue");
    act(() => result.current.markStageComplete("knowledge-base"));
    expect(result.current.gateMessage).toBeNull();
  });

  it("leaves ungated stages free to advance", () => {
    const { result } = renderHook(() => useLabProgress(GATED_LAB));
    act(() => result.current.goToStage(0, 1));
    expect(result.current.gateMessage).toBeNull();
  });

  it("completing one stage does not satisfy another stage's gate", () => {
    const { result } = renderHook(() => useLabProgress(GATED_LAB));
    act(() => result.current.markStageComplete("memory"));
    expect(result.current.gateMessage).toBe("Add a standards doc to continue");
  });

  it("keeps completion across remounts, so a resumed run isn't re-gated", () => {
    const first = renderHook(() => useLabProgress(GATED_LAB));
    act(() => first.result.current.markStageComplete("knowledge-base"));
    first.unmount();
    const second = renderHook(() => useLabProgress(GATED_LAB));
    expect(second.result.current.gateMessage).toBeNull();
  });

  it("opens the next stage for navigation once the current one is complete, but no further", () => {
    const { result } = renderHook(() => useLabProgress(GATED_LAB));
    expect(result.current.furthestFlatIndex).toBe(0);
    act(() => result.current.markStageComplete("knowledge-base"));
    expect(result.current.furthestFlatIndex).toBe(1);
  });
});

describe("useLabProgress — advancing marks a stage done", () => {
  const LAB = {
    id: "advance-lab",
    chapters: [
      {
        id: "c1",
        title: "C",
        stages: [
          { id: "retrieval-cost", title: "Explainer", summary: "" },
          { id: "guardrail", title: "Another explainer", summary: "" },
          { id: "collaborate", title: "Last", summary: "" },
        ],
      },
    ],
  } as never;

  // The rail used to tick only gated lessons, so explainers the user read and
  // moved past stayed blank and the progress count under-reported.
  it("completes an ungated stage when the user moves past it", () => {
    const { result } = renderHook(() => useLabProgress(LAB));
    expect(result.current.completedStages.has("retrieval-cost")).toBe(false);
    act(() => result.current.goToNext());
    expect(result.current.completedStages.has("retrieval-cost")).toBe(true);
    expect(result.current.currentStage.id).toBe("guardrail");
  });

  it("doesn't complete the stage the user lands on, only the one they left", () => {
    const { result } = renderHook(() => useLabProgress(LAB));
    act(() => result.current.goToNext());
    expect(result.current.completedStages.has("guardrail")).toBe(false);
  });

  it("leaves the final stage alone, since there is nowhere to advance to", () => {
    const { result } = renderHook(() => useLabProgress(LAB));
    act(() => result.current.goToStage(0, 2));
    act(() => result.current.goToNext());
    expect(result.current.completedStages.has("collaborate")).toBe(false);
    expect(result.current.currentStage.id).toBe("collaborate");
  });

  it("going back doesn't un-complete what was already finished", () => {
    const { result } = renderHook(() => useLabProgress(LAB));
    act(() => result.current.goToNext());
    act(() => result.current.goToPrevious());
    expect(result.current.completedStages.has("retrieval-cost")).toBe(true);
  });
});
