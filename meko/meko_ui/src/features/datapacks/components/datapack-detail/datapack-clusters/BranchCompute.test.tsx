import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { type ReactElement } from "react";
import { render, screen, cleanup } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { BranchCompute } from "./BranchCompute";
import { ampListCompute, type AmpCompute } from "@/lib/api/ampApi";

// BranchCompute enforces AMP's "one read-write compute per branch" rule in the
// provision form. That gate is the whole point of the component, so test it
// against real loaded data (via the useCompute query) rather than mocking the
// child out. ampApi is mocked so the query resolves synchronously in the test.
vi.mock("@/lib/api/ampApi", () => ({
  ampListCompute: vi.fn(),
  ampCreateCompute: vi.fn(),
}));

const mockList = vi.mocked(ampListCompute);

function renderWithClient(ui: ReactElement) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
}

const compute = (over: Partial<AmpCompute>): AmpCompute => ({
  id: "e1",
  cluster_id: "p",
  branch_id: "br",
  role: "read_only",
  address: "10.0.0.1:5432",
  created_at: "",
  ...over,
});

afterEach(cleanup);
beforeEach(() => vi.clearAllMocks());

describe("BranchCompute read-write gating", () => {
  it("disables read-write + shows the hint once a read-write compute exists", async () => {
    mockList.mockResolvedValue([compute({ id: "rw", role: "read_write" })]);
    renderWithClient(<BranchCompute datapackId="dp" clusterId="cl" branchId="br" branchName="main" isDefaultBranch={false} />);

    // Wait for compute to load (the endpoint address renders in the card).
    await screen.findByText("10.0.0.1:5432");
    expect(screen.getByText(/additional compute must be read-only/i)).toBeTruthy();
    expect((screen.getByRole("option", { name: /read-write/i }) as HTMLOptionElement).disabled).toBe(true);
  });

  it("keeps read-write available when only read-only compute exists", async () => {
    mockList.mockResolvedValue([compute({ id: "ro", role: "read_only" })]);
    renderWithClient(<BranchCompute datapackId="dp" clusterId="cl" branchId="br" branchName="main" isDefaultBranch={false} />);

    await screen.findByText("10.0.0.1:5432");
    expect((screen.getByRole("option", { name: /read-write/i }) as HTMLOptionElement).disabled).toBe(false);
    expect(screen.queryByText(/additional compute must be read-only/i)).toBeNull();
  });
});
