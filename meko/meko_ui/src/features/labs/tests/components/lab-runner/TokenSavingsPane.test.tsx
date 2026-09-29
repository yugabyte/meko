import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";

import type { TokenSavingsSummary } from "@/lib/api/mekoApi";
import { TokenSavingsPane } from "@/features/labs/components/lab-runner/TokenSavingsPane";

const mockUseDatapackTokenSavings = vi.fn();
vi.mock("@/features/datapacks/hooks/useTokenSavings", () => ({
  useDatapackTokenSavings: (...args: unknown[]) => mockUseDatapackTokenSavings(...args),
}));

// total_requests: 1 -> baseline 10,000 tokens (BASELINE_TOKENS_PER_REQUEST),
// so total_tokens_saved: 8000 works out to an even 80% for the headline.
function summary(overrides: Partial<TokenSavingsSummary> = {}): TokenSavingsSummary {
  return {
    total_requests: 1,
    total_tokens_saved: 8000,
    tokens_saved_by_source: { knowledge_base: 5000, memory: 3000 },
    by_datapack: [],
    ...overrides,
  };
}

afterEach(cleanup);

describe("TokenSavingsPane", () => {
  it("shows the empty state when there's no data yet", () => {
    mockUseDatapackTokenSavings.mockReturnValue({
      data: summary({ total_requests: 0, total_tokens_saved: 0, tokens_saved_by_source: {} }),
      isLoading: false,
      error: null,
    });
    render(<TokenSavingsPane datapackId="dp-1" />);
    fireEvent.click(screen.getByRole("button", { name: /token savings/i }));
    expect(screen.getByText("No contextual searches recorded yet.")).toBeTruthy();
  });

  it("shows the headline percentage even while collapsed", () => {
    mockUseDatapackTokenSavings.mockReturnValue({ data: summary(), isLoading: false, error: null });
    render(<TokenSavingsPane datapackId="dp-1" />);
    expect(screen.getByText("80% saved")).toBeTruthy();
  });

  it("shows the per-source breakdown once expanded", () => {
    mockUseDatapackTokenSavings.mockReturnValue({ data: summary(), isLoading: false, error: null });
    render(<TokenSavingsPane datapackId="dp-1" />);
    fireEvent.click(screen.getByRole("button", { name: /token savings/i }));
    expect(screen.getByText(/5\.0K tokens/)).toBeTruthy();
    expect(screen.getByText(/3\.0K tokens/)).toBeTruthy();
  });

  it("shows an error message when the fetch fails", () => {
    mockUseDatapackTokenSavings.mockReturnValue({
      data: undefined,
      isLoading: false,
      error: new Error("boom"),
    });
    render(<TokenSavingsPane datapackId="dp-1" />);
    fireEvent.click(screen.getByRole("button", { name: /token savings/i }));
    expect(screen.getByText("boom")).toBeTruthy();
  });
});
