import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";

import { ROUTES } from "@/app/routes/paths";
import LabsSection from "@/features/labs/pages/LabsSection";

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

// Labs shipped: no flag gates it any more, so both routes resolve for
// everyone. These pin that a stale latched flag can't resurrect the 404.
describe("LabsSection routing", () => {
  it("renders the labs list with no flag set", () => {
    renderSection(ROUTES.labs);
    expect(screen.queryByText("404 Page Not Found")).toBeNull();
  });

  it("renders a lab detail route with no flag set", () => {
    renderSection(`${ROUTES.labs}/adlc-review-agent`);
    expect(screen.queryByText("404 Page Not Found")).toBeNull();
  });

  // The old latch survives in the localStorage of anyone who ever unlocked
  // Labs, so nothing may read it back into a gate.
  it("ignores the retired showLabs latch, whichever value it holds", () => {
    localStorage.setItem("meko:show_labs_nav", "false");
    renderSection(ROUTES.labs);
    expect(screen.queryByText("404 Page Not Found")).toBeNull();
  });
});
