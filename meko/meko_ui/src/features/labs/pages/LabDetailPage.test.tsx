import { afterEach, describe, expect, it } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";

import { labDetailPath } from "@/app/routes/paths";
import { ADLC_REVIEW_AGENT_LAB } from "../data/labDefinitions";
import LabDetailPage from "./LabDetailPage";

const { chapters, startingPoint } = ADLC_REVIEW_AGENT_LAB;

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

describe("LabDetailPage starting-point flow", () => {
  it("renders all three steps' titles and subtitles", () => {
    renderPage();
    for (const step of startingPoint) {
      expect(screen.getByText(step.title)).toBeTruthy();
      expect(screen.getByText(step.subtitle)).toBeTruthy();
    }
  });
});

describe("LabDetailPage chapter tabs", () => {
  it("shows a tab per chapter, numbered in order, with no individual stages listed", () => {
    renderPage();
    for (let i = 0; i < chapters.length; i++) {
      expect(screen.getByRole("tab", { name: `${i + 1}. ${chapters[i].title}` })).toBeTruthy();
    }
    for (const chapter of chapters) {
      for (const stage of chapter.stages) {
        expect(screen.queryByText(stage.title)).toBeNull();
      }
    }
  });

  it("shows only the first chapter's summary initially", () => {
    renderPage();
    expect(screen.getByText(chapters[0].summary)).toBeTruthy();
    expect(screen.queryByText(chapters[1].summary)).toBeNull();
  });

  it("switches to the second chapter's summary on click, hiding the first", () => {
    renderPage();
    // Radix's TabsTrigger switches on `mousedown`, not `click` -- fireEvent.click
    // alone never dispatches a mousedown in jsdom.
    fireEvent.mouseDown(screen.getByRole("tab", { name: `2. ${chapters[1].title}` }));
    expect(screen.getByText(chapters[1].summary)).toBeTruthy();
    expect(screen.queryByText(chapters[0].summary)).toBeNull();
  });
});
