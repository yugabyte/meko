import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";

import { labRunnerPath, ROUTES } from "@/app/routes/paths";
import { TooltipProvider } from "@/components/ui/tooltip";
import LabRunnerPage, { shouldShowProvisionStatusSpinner } from "./LabRunnerPage";

const { mockNavigate } = vi.hoisted(() => ({ mockNavigate: vi.fn() }));
vi.mock("react-router-dom", async (importOriginal) => {
  const actual = await importOriginal<typeof import("react-router-dom")>();
  return { ...actual, useNavigate: () => mockNavigate };
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});
beforeEach(() => {
  localStorage.clear();
  // jsdom has no matchMedia -- LabObservationSlot's useIsMobile() needs it.
  vi.stubGlobal(
    "matchMedia",
    vi.fn().mockImplementation((query: string) => ({
      matches: false,
      media: query,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    })),
  );
});

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <TooltipProvider>
        <MemoryRouter initialEntries={[labRunnerPath("adlc-review-agent")]}>
          <LabRunnerPage />
        </MemoryRouter>
      </TooltipProvider>
    </QueryClientProvider>,
  );
}

describe("LabRunnerPage pre-GA gating", () => {
  it("renders 404 for a direct URL hit when the showLabs flag is off", () => {
    renderPage();
    expect(screen.getByText("404 Page Not Found")).toBeTruthy();
  });

  it("renders the runner once the flag is latched", () => {
    localStorage.setItem("meko:show_labs_nav", "true");
    renderPage();
    expect(screen.queryByText("404 Page Not Found")).toBeNull();
  });

  it("clicking End Lab navigates back to the labs list", () => {
    localStorage.setItem("meko:show_labs_nav", "true");
    renderPage();
    fireEvent.click(screen.getByRole("button", { name: /end lab/i }));
    expect(mockNavigate).toHaveBeenCalledWith(ROUTES.labs);
  });
});

describe("shouldShowProvisionStatusSpinner", () => {
  it("blocks memory, conversation, run-review, and run-review-again while the provision-status lookup is loading", () => {
    expect(shouldShowProvisionStatusSpinner(true, "memory")).toBe(true);
    expect(shouldShowProvisionStatusSpinner(true, "conversation")).toBe(true);
    expect(shouldShowProvisionStatusSpinner(true, "run-review")).toBe(true);
    // Regression: run-review-again renders the same PrReviewDemoApp, which
    // needs session.userToken in dedicated mode just like run-review does.
    expect(shouldShowProvisionStatusSpinner(true, "run-review-again")).toBe(true);
  });

  it("doesn't block see-the-pr, create-datapack, knowledge-base, meet-agentk, or collaborate -- none read session.userToken", () => {
    expect(shouldShowProvisionStatusSpinner(true, "see-the-pr")).toBe(false);
    expect(shouldShowProvisionStatusSpinner(true, "create-datapack")).toBe(false);
    expect(shouldShowProvisionStatusSpinner(true, "knowledge-base")).toBe(false);
    expect(shouldShowProvisionStatusSpinner(true, "meet-agentk")).toBe(false);
    expect(shouldShowProvisionStatusSpinner(true, "collaborate")).toBe(false);
  });

  it("never blocks once loading is false", () => {
    expect(shouldShowProvisionStatusSpinner(false, "memory")).toBe(false);
  });
});
