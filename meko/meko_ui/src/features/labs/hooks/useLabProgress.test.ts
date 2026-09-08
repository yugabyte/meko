import { afterEach, describe, expect, it } from "vitest";
import { act, renderHook } from "@testing-library/react";

import type { LabChapter, LabStage } from "../types";
import { useLabProgress } from "./useLabProgress";

afterEach(() => localStorage.clear());

const FIXTURE_STAGES: LabStage[] = [
  { id: "create-datapack", title: "Create a datapack", summary: "", highlights: ["a", "b"] },
  { id: "seed-context", title: "Seed context", summary: "", highlights: ["a", "b"] },
  { id: "run-review", title: "Run a review", summary: "", highlights: ["a", "b"] },
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

  it("goToNextChapter jumps straight to the next chapter's first stage regardless of current stageIndex", () => {
    const { result } = renderHook(() => useLabProgress(TWO_CHAPTER_LAB));
    // Not on the last stage of chapter 0 -- goToNextChapter still moves on,
    // unlike goToNext which would only advance within the chapter.
    expect(result.current.stageIndex).toBe(0);
    act(() => result.current.goToNextChapter());
    expect(result.current.chapterIndex).toBe(1);
    expect(result.current.stageIndex).toBe(0);
  });

  it("goToNextChapter is a no-op on the last chapter", () => {
    const { result } = renderHook(() => useLabProgress(TWO_CHAPTER_LAB));
    act(() => result.current.goToStage(1, 0));
    act(() => result.current.goToNextChapter());
    expect(result.current.chapterIndex).toBe(1);
  });

  it("goToPreviousChapter jumps back to the previous chapter's first stage", () => {
    const { result } = renderHook(() => useLabProgress(TWO_CHAPTER_LAB));
    act(() => result.current.goToStage(1, 0));
    act(() => result.current.goToPreviousChapter());
    expect(result.current.chapterIndex).toBe(0);
    expect(result.current.stageIndex).toBe(0);
  });

  it("goToPreviousChapter is a no-op on the first chapter", () => {
    const { result } = renderHook(() => useLabProgress(TWO_CHAPTER_LAB));
    act(() => result.current.goToPreviousChapter());
    expect(result.current.chapterIndex).toBe(0);
  });

  it("isLastStageInChapter and isLastChapter are independent of the overall isLastStage", () => {
    const { result } = renderHook(() => useLabProgress(TWO_CHAPTER_LAB));
    act(() => result.current.goToStage(0, 1)); // last stage of chapter 0, not the last chapter
    expect(result.current.isLastStageInChapter).toBe(true);
    expect(result.current.isLastChapter).toBe(false);
    expect(result.current.isLastStage).toBe(false);
  });
});
