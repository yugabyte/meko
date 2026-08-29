import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";

import { labRunnerPath } from "@/app/routes/paths";
import { TooltipProvider } from "@/components/ui/tooltip";
import LabRunnerPage, { shouldShowProvisionStatusSpinner } from "./LabRunnerPage";

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
});

describe("shouldShowProvisionStatusSpinner", () => {
  it("blocks seed-context and run-review while the provision-status lookup is loading", () => {
    expect(shouldShowProvisionStatusSpinner(true, "seed-context")).toBe(true);
    expect(shouldShowProvisionStatusSpinner(true, "run-review")).toBe(true);
  });

  it("doesn't block create-datapack, deploy-agent, or collaborate -- none read session.userToken", () => {
    expect(shouldShowProvisionStatusSpinner(true, "create-datapack")).toBe(false);
    expect(shouldShowProvisionStatusSpinner(true, "deploy-agent")).toBe(false);
    expect(shouldShowProvisionStatusSpinner(true, "collaborate")).toBe(false);
  });

  it("never blocks once loading is false", () => {
    expect(shouldShowProvisionStatusSpinner(false, "seed-context")).toBe(false);
  });
});
