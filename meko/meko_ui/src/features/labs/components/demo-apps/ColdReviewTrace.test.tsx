import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen } from "@testing-library/react";

import type { ColdReviewTraceLine } from "../../data/adlcReviewAgentLab/coldReviewCapture";
import { ColdReviewTrace } from "./ColdReviewTrace";

const LINES: ColdReviewTraceLine[] = [
  { at: "0.0s", text: "connect" },
  { at: "0.4s", text: "context sources available: none", tone: "warn" },
  { at: "4.6s", text: "verdict: no blocking issues", tone: "ok" },
];

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

describe("ColdReviewTrace", () => {
  it("reveals lines one at a time rather than all at once", () => {
    vi.useFakeTimers();
    render(<ColdReviewTrace lines={LINES} />);

    // The pacing is the lesson: the user must see "no context" land before
    // the verdict does, or the verdict means nothing.
    expect(screen.queryByText(/verdict/)).toBeNull();
    act(() => void vi.advanceTimersByTime(600));
    expect(screen.getByText("connect")).toBeTruthy();
    act(() => void vi.advanceTimersByTime(600));
    expect(screen.getByText("context sources available: none")).toBeTruthy();
    expect(screen.queryByText(/verdict/)).toBeNull();
  });

  it("reports completion once, after the last line lands", () => {
    vi.useFakeTimers();
    const onFinished = vi.fn();
    render(<ColdReviewTrace lines={LINES} onFinished={onFinished} />);

    // One advance per line: each line's timer is only scheduled after the
    // previous one re-renders, so a single long advance doesn't chain them.
    for (let i = 0; i <= LINES.length; i++) {
      act(() => void vi.advanceTimersByTime(600));
    }
    expect(screen.getByText("verdict: no blocking issues")).toBeTruthy();
    expect(onFinished).toHaveBeenCalled();
  });

  it("shows everything immediately when the user prefers reduced motion", () => {
    vi.stubGlobal("matchMedia", (q: string) => ({
      matches: q.includes("reduce"),
      addEventListener: () => {},
      removeEventListener: () => {},
    }));
    render(<ColdReviewTrace lines={LINES} />);

    expect(screen.getByText("connect")).toBeTruthy();
    expect(screen.getByText("verdict: no blocking issues")).toBeTruthy();
    vi.unstubAllGlobals();
  });
});
