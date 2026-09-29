// The claim this lesson makes is "go and check": every row has to reach the
// real thread, or the page is asking for trust it said it did not need.

import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

import {
  PUBLIC_REVIEWS_READ_ON,
  PUBLIC_RUNS,
  pullRequestUrl,
} from "@/features/labs/data/adlcReviewAgentLab/publicReviews";
import { PublicRunsStage } from "@/features/labs/components/stages/PublicRunsStage";

afterEach(cleanup);

describe("PublicRunsStage", () => {
  it("deep-links every row to the pull request it describes", () => {
    render(<PublicRunsStage />);
    for (const run of PUBLIC_RUNS) {
      const link = screen.getByRole("link", { name: new RegExp(run.title.slice(0, 30), "i") });
      expect((link as HTMLAnchorElement).href).toBe(pullRequestUrl(run.pr));
    }
  });

  // A list where every review found something reads as a demo reel. The
  // clean row is the one that makes the rest believable.
  it("shows a review that flagged nothing as its own outcome", () => {
    const clean = PUBLIC_RUNS.filter((run) => run.major === 0 && run.minor === 0);
    expect(clean.length).toBeGreaterThan(0);
    render(<PublicRunsStage />);
    expect(screen.getAllByText("nothing flagged")).toHaveLength(clean.length);
  });

  it("dates the capture rather than implying a live fetch", () => {
    render(<PublicRunsStage />);
    expect(screen.getByText(`read ${PUBLIC_REVIEWS_READ_ON}`)).toBeTruthy();
  });

  it("names the account the comments were posted as", () => {
    render(<PublicRunsStage />);
    expect(screen.getByText("yb-agentk-dev[bot]")).toBeTruthy();
  });
});
