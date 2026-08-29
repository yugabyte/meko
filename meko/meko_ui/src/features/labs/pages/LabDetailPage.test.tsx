import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";

import { labDetailPath } from "@/app/routes/paths";
import { ADLC_REVIEW_AGENT_LAB_STAGES } from "../data/adlcReviewAgentLab/stages";
import LabDetailPage from "./LabDetailPage";

afterEach(cleanup);

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={[labDetailPath("adlc-review-agent")]}>
        <LabDetailPage />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("LabDetailPage overview timeline", () => {
  it("renders every stage's title and both highlight bullets, in order", () => {
    renderPage();

    const titles = screen.getAllByText(
      (_, el) => el?.tagName === "P" && ADLC_REVIEW_AGENT_LAB_STAGES.some((s) => s.title === el.textContent),
    );
    expect(titles.map((el) => el.textContent)).toEqual(ADLC_REVIEW_AGENT_LAB_STAGES.map((s) => s.title));

    for (const stage of ADLC_REVIEW_AGENT_LAB_STAGES) {
      for (const highlight of stage.highlights) {
        expect(screen.getByText(highlight)).toBeTruthy();
      }
    }
  });

  it("numbers each stage 1 through 5", () => {
    renderPage();
    for (let i = 1; i <= ADLC_REVIEW_AGENT_LAB_STAGES.length; i++) {
      expect(screen.getByText(String(i))).toBeTruthy();
    }
  });
});
