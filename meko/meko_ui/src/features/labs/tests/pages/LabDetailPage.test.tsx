import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";

import { labDetailPath } from "@/app/routes/paths";
import { ADLC_REVIEW_AGENT_LAB } from "@/features/labs/data/labDefinitions";
import { flattenLabStages } from "@/features/labs/lib/labChapters";
import LabDetailPage from "@/features/labs/pages/LabDetailPage";

// This page reads the lab via getLabDefinition, which drops any requiresAmp
// chapter when AMP isn't enabled -- these tests are about the landing page,
// not AMP gating, so they exercise the full chapter list.
vi.mock("@/lib/amp", () => ({ isAmpEnabled: () => true }));

const { chapters } = ADLC_REVIEW_AGENT_LAB;

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

describe("LabDetailPage", () => {
  it("lists neither chapters nor steps — the commitment is the headline, not the outline", () => {
    renderPage();
    for (const chapter of chapters) {
      expect(screen.queryByText(chapter.title)).toBeNull();
      for (const stage of chapter.stages) {
        expect(screen.queryByText(stage.title)).toBeNull();
      }
    }
  });

  it("derives its chapter and step counts from the lab, so the meta line can't drift", () => {
    renderPage();
    const stageCount = flattenLabStages(chapters).length;
    expect(screen.getByText(`${chapters.length} chapters`)).toBeTruthy();
    expect(screen.getByText(`${stageCount} steps`)).toBeTruthy();
  });

  it("places the lab in the lifecycle, which is per-lab and so belongs here", () => {
    renderPage();
    expect(screen.getByText(/where this sits in the adlc/i)).toBeTruthy();
    expect(screen.getByText("Review")).toBeTruthy();
    expect(screen.getByText("Fix & merge")).toBeTruthy();
  });

  it("offers exactly one way to start the lab", () => {
    renderPage();
    expect(screen.getByRole("link", { name: /launch lab/i })).toBeTruthy();
  });
});
