import { afterEach, describe, expect, it } from "vitest";
import { act, renderHook } from "@testing-library/react";

import type { LabStage } from "../types";
import { useLabProgress } from "./useLabProgress";

afterEach(() => localStorage.clear());

const FIXTURE_STAGES: LabStage[] = [
  { id: "create-datapack", title: "Create a datapack", shortLabel: "Datapack", summary: "" },
  { id: "seed-context", title: "Seed context", shortLabel: "Seed context", summary: "" },
  { id: "run-review", title: "Run a review", shortLabel: "Run review", summary: "" },
];
const FIXTURE_LAB = { id: "test-lab", stages: FIXTURE_STAGES } as never;

describe("useLabProgress", () => {
  it("starts at stage 0", () => {
    const { result } = renderHook(() => useLabProgress(FIXTURE_LAB));
    expect(result.current.stageIndex).toBe(0);
    expect(result.current.currentStage.id).toBe("create-datapack");
  });

  it("goToStage clamps out-of-range indices", () => {
    const { result } = renderHook(() => useLabProgress(FIXTURE_LAB));
    act(() => result.current.goToStage(-5));
    expect(result.current.stageIndex).toBe(0);
    act(() => result.current.goToStage(99));
    expect(result.current.stageIndex).toBe(2);
  });

  it("goToNext is a no-op at the last stage", () => {
    const { result } = renderHook(() => useLabProgress(FIXTURE_LAB));
    act(() => result.current.goToStage(2));
    act(() => result.current.goToNext());
    expect(result.current.stageIndex).toBe(2);
    expect(result.current.isLastStage).toBe(true);
  });

  it("persists across remounts", () => {
    const { result, unmount } = renderHook(() => useLabProgress(FIXTURE_LAB));
    act(() => result.current.goToStage(1));
    unmount();

    const { result: result2 } = renderHook(() => useLabProgress(FIXTURE_LAB));
    expect(result2.current.stageIndex).toBe(1);
  });

  it("falls back to stage 0 reading a pre-restructuring {chapterIndex, stepIndex} blob", () => {
    localStorage.setItem("meko:lab-progress:test-lab", JSON.stringify({ chapterIndex: 2, stepIndex: 1 }));
    const { result } = renderHook(() => useLabProgress(FIXTURE_LAB));
    expect(result.current.stageIndex).toBe(0);
  });
});
