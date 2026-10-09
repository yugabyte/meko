// Every figure on the production steps is a dated capture, not a live poll.
// What is pinned here is that the page says so, and that the numbers come
// from the snapshot rather than being typed into the JSX.

import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

import {
  PRODUCTION_DASHBOARD_URL,
  PRODUCTION_RUNS_30D_TOTAL,
  PRODUCTION_SNAPSHOT,
  PRODUCTION_SNAPSHOT_READ_ON,
} from "@/features/labs/data/adlcReviewAgentLab/productionSnapshot";
import { ProductionReviewsStage } from "@/features/labs/components/stages/ProductionReviewsStage";

afterEach(cleanup);

describe("ProductionReviewsStage", () => {
  it("dates the snapshot rather than implying a live feed", () => {
    render(<ProductionReviewsStage />);
    expect(screen.getByText(`read ${PRODUCTION_SNAPSHOT_READ_ON}`)).toBeTruthy();
    expect(screen.getByText(/snapshot, not a live feed/i)).toBeTruthy();
  });

  it("quotes the snapshot's own figures", () => {
    render(<ProductionReviewsStage />);
    expect(screen.getByText(PRODUCTION_SNAPSHOT.issuesFound.toLocaleString())).toBeTruthy();
    expect(screen.getByText(PRODUCTION_SNAPSHOT.guidelinesInContext.toLocaleString())).toBeTruthy();
  });

  it("links out to the dashboard the figures came from", () => {
    render(<ProductionReviewsStage />);
    const link = screen.getByRole("link", { name: /open the dashboard/i }) as HTMLAnchorElement;
    expect(link.href).toBe(PRODUCTION_DASHBOARD_URL);
  });
});

// The 30-day chart used to be its own step. It now shares this one, so its
// coverage moves here rather than going away with the file.
describe("ProductionReviewsStage, thirty-day half", () => {
  // Derived from the series rather than restated, so the chart and the total
  // cannot drift apart when the snapshot is refreshed.
  it("totals the same series the chart draws", () => {
    render(<ProductionReviewsStage />);
    expect(screen.getByText(PRODUCTION_RUNS_30D_TOTAL.toLocaleString())).toBeTruthy();
  });

  it("describes the chart for a reader who cannot see it", () => {
    render(<ProductionReviewsStage />);
    expect(
      screen.getByRole("img", { name: /runs per day from .* peaking at \d+ runs/i }),
    ).toBeTruthy();
  });
});
