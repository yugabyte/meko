import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
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
  // Hoisted, so it is shared across tests and would otherwise carry the
  // previous test's navigation into an assertion that nothing navigated.
  mockNavigate.mockClear();
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

describe("LabRunnerPage", () => {
  it("renders the runner with no flag set", () => {
    renderPage();
    expect(screen.queryByText("404 Page Not Found")).toBeNull();
  });

  // Both exits discard the run, so neither acts on the first click.
  it("asks before exiting, then clears the run's saved state and leaves", () => {
    localStorage.setItem("meko:lab-progress:adlc-review-agent", '{"chapterIndex":2,"stageIndex":1}');
    renderPage();

    fireEvent.click(screen.getByRole("button", { name: /exit lab/i }));
    expect(mockNavigate).not.toHaveBeenCalled();

    const dialog = within(screen.getByRole("alertdialog"));
    fireEvent.click(dialog.getByRole("button", { name: /exit lab/i }));
    expect(localStorage.getItem("meko:lab-progress:adlc-review-agent")).toBeNull();
    expect(mockNavigate).toHaveBeenCalledWith(ROUTES.labs);
  });

  it("asks before restarting, then clears the run's saved state and stays put", () => {
    localStorage.setItem("meko:lab-progress:adlc-review-agent", '{"chapterIndex":2,"stageIndex":1}');
    localStorage.setItem("meko:lab-completed:adlc-review-agent", '["see-the-pr"]');
    renderPage();

    fireEvent.click(screen.getByRole("button", { name: /restart lab/i }));
    expect(localStorage.getItem("meko:lab-progress:adlc-review-agent")).not.toBeNull();

    const dialog = within(screen.getByRole("alertdialog"));
    fireEvent.click(dialog.getByRole("button", { name: /restart lab/i }));
    expect(localStorage.getItem("meko:lab-progress:adlc-review-agent")).toBeNull();
    expect(localStorage.getItem("meko:lab-completed:adlc-review-agent")).toBeNull();
    expect(mockNavigate).not.toHaveBeenCalled();
  });
});

describe("shouldShowProvisionStatusSpinner", () => {
  it("blocks every stage that invokes the agent while the provision-status lookup is loading", () => {
    expect(shouldShowProvisionStatusSpinner(true, "memory")).toBe(true);
    expect(shouldShowProvisionStatusSpinner(true, "conversation")).toBe(true);
    expect(shouldShowProvisionStatusSpinner(true, "run-review")).toBe(true);
    // make-it-yours hosts the bring-your-own-PR review, so it renders the
    // same PrReviewDemoApp and needs session.userToken in dedicated mode.
    // Without it the stage renders mid-provision and tells the user to deploy
    // an agent that is already deploying.
    expect(shouldShowProvisionStatusSpinner(true, "make-it-yours")).toBe(true);
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
