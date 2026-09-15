// The cold-review screen is the one place in Labs that shows a result nobody
// has measured yet. These pin the gate that keeps it honest.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";

import { COLD_REVIEW_IS_POPULATED } from "../../data/adlcReviewAgentLab/coldReviewCapture";
import { SeeThePrStage } from "./SeeThePrStage";

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
  // COLD_REVIEW_IS_POPULATED is derived from the capture's token counts, so it
  // flips on its own the moment someone records a real run. Until then no part
  // of this screen may state an outcome: not the verdict, not the badge, not
  // the button label.
  it("states no outcome while the baseline capture is unrecorded", async () => {
    expect(COLD_REVIEW_IS_POPULATED).toBe(false);
    runTheColdReview();

    expect(await screen.findByText(/hasn't been recorded yet/)).toBeTruthy();
    expect(screen.queryByText("approved")).toBeNull();
    expect(screen.queryByText(/Reviewed, \d+ issues/)).toBeNull();
    expect(screen.getByRole("button", { name: /review complete/i })).toBeTruthy();
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
