import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";

import { ROUTES } from "@/app/routes/paths";
import LabsSection from "./LabsSection";

afterEach(cleanup);
beforeEach(() => {
  localStorage.clear();
});

function renderSection(path: string = ROUTES.labs) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={[path]}>
        <LabsSection />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("LabsSection pre-GA gating", () => {
  it("renders 404 on the labs list route when the showLabs flag is off", () => {
    renderSection(ROUTES.labs);
    expect(screen.getByText("404 Page Not Found")).toBeTruthy();
  });

  it("renders 404 on a lab detail route when the flag is off, not the detail page", () => {
    renderSection(`${ROUTES.labs}/adlc-review-agent`);
    expect(screen.getByText("404 Page Not Found")).toBeTruthy();
  });

  it("renders the labs list once the flag is latched", () => {
    localStorage.setItem("meko:show_labs_nav", "true");
    renderSection(ROUTES.labs);
    expect(screen.queryByText("404 Page Not Found")).toBeNull();
  });
});
