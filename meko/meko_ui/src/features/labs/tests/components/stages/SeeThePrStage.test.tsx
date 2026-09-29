// The cold-review screen's verdict is the baseline the whole lab argues
// against, so it may never appear before the replay has finished.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";

import { COLD_REVIEW_VERDICT } from "@/features/labs/data/adlcReviewAgentLab/coldReviewCapture";
import { SeeThePrStage } from "@/features/labs/components/stages/SeeThePrStage";

// ColdReviewTrace replays line by line on timers unless the reader has asked
// for reduced motion, in which case it renders whole and reports finished at
// once -- which is how these tests reach the result card.
beforeEach(() => {
  vi.stubGlobal("matchMedia", (query: string) => ({
    matches: query.includes("prefers-reduced-motion"),
    media: query,
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
  }));
});

afterEach(() => {
  vi.unstubAllGlobals();
  cleanup();
});

function runTheColdReview() {
  render(<SeeThePrStage agentDisplayName="AgentK" onComplete={vi.fn()} />);
  fireEvent.click(screen.getByRole("button", { name: /review with no context/i }));
}

describe("SeeThePrStage", () => {
  it("approves the diff once the replay finishes, which is the baseline", async () => {
    runTheColdReview();

    expect(await screen.findByText("approved")).toBeTruthy();
    expect(screen.getByText(new RegExp(COLD_REVIEW_VERDICT.slice(0, 20)))).toBeTruthy();
    expect(screen.getByRole("button", { name: /review complete/i })).toBeTruthy();
  });

  // The measured half of the capture (tokens, duration) is still unrecorded,
  // so the card states the verdict and nothing quantitative.
  it("quotes no measured numbers beside the verdict", async () => {
    runTheColdReview();
    await screen.findByText("approved");

    expect(screen.queryByText(/input tokens/i)).toBeNull();
    expect(screen.queryByText(/\d+ issues/)).toBeNull();
  });

  // The label used to key off `started`, which is set synchronously on click,
  // so it announced a finished review while the trace was still animating.
  it("does not claim the review is done while the trace is still running", () => {
    vi.stubGlobal("matchMedia", (query: string) => ({
      matches: false, // no reduced motion: the replay takes its time
      media: query,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    }));
    render(<SeeThePrStage agentDisplayName="AgentK" onComplete={vi.fn()} />);
    fireEvent.click(screen.getByRole("button", { name: /review with no context/i }));

    expect(screen.getByRole("button", { name: /reviewing/i })).toBeTruthy();
    expect(screen.queryByRole("button", { name: /review complete/i })).toBeNull();
  });

  it("shows the trace, which is the lesson regardless of the capture", async () => {
    runTheColdReview();
    expect(await screen.findByText(/context sources available: none/)).toBeTruthy();
  });
});
