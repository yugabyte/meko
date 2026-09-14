import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";

import { labDetailPath } from "@/app/routes/paths";
import { ADLC_REVIEW_AGENT_LAB } from "../data/labDefinitions";
import { flattenLabStages } from "../lib/labChapters";
import LabDetailPage from "./LabDetailPage";

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
  it("shows every chapter at once, in order, so the shape of the run is visible before starting", () => {
    renderPage();
    for (let i = 0; i < chapters.length; i++) {
      expect(screen.getByText(`Ch ${i + 1}`)).toBeTruthy();
      expect(screen.getByText(chapters[i].title)).toBeTruthy();
      expect(screen.getByText(chapters[i].summary)).toBeTruthy();
    }
  });

  it("summarises chapters without listing individual steps", () => {
    renderPage();
    for (const chapter of chapters) {
      for (const stage of chapter.stages) {
        expect(screen.queryByText(stage.title)).toBeNull();
      }
    }
  });

  it("derives its chapter and step counts from the lab, so the meta line can't drift", () => {
    renderPage();
    const stageCount = flattenLabStages(chapters).length;
    expect(screen.getByText(`${chapters.length} chapters, ${stageCount} steps`)).toBeTruthy();
  });

  it("offers exactly one way to start the lab", () => {
    renderPage();
    expect(screen.getByRole("link", { name: /launch lab/i })).toBeTruthy();
  });
});
