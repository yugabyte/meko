import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { type ReactElement } from "react";
import { render, screen, cleanup, fireEvent, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { BranchCompute } from "./BranchCompute";
import { ampListCompute, ampCreateCompute, ampCoreLimitMessage, type AmpCompute } from "@/lib/api/ampApi";

// BranchCompute enforces AMP's "one read-write compute per branch" rule in the
// provision form. That gate is the whole point of the component, so test it
// against real loaded data (via the useCompute query) rather than mocking the
// child out. ampApi is mocked so the query resolves synchronously in the test.
vi.mock("@/lib/api/ampApi", () => ({
  ampListCompute: vi.fn(),
  ampCreateCompute: vi.fn(),
  // The add-compute form now reads the tier budget; default to an unlimited
  // (Enterprise) budget so these one-read-write-rule tests keep the AMP default.
  ampCoreBudget: vi
    .fn()
    .mockResolvedValue({ tier: "enterprise", next_tier: "", used_cores: 0, max_cores: null, at_limit: false }),
  ampCoreLimitMessage: vi.fn().mockReturnValue(null),
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

describe("BranchCompute tier budget", () => {
  it("blocks provision when the CPU field is cleared (server would count '' as 2 cores)", async () => {
    mockList.mockResolvedValue([]);
    renderWithClient(<BranchCompute datapackId="dp" clusterId="cl" branchId="br" branchName="main" isDefaultBranch={false} />);

    const cpu = await screen.findByLabelText("Compute CPU");
    const btn = screen.getByRole("button", { name: /provision compute/i }) as HTMLButtonElement;
    await waitFor(() => expect(btn.disabled).toBe(false)); // budget loaded, default within budget
    fireEvent.change(cpu, { target: { value: "" } });
    await waitFor(() => expect(btn.disabled).toBe(true)); // blank CPU is not valid
  });

  it("renders the upgrade notice when provisioning hits the vCPU cap", async () => {
    mockList.mockResolvedValue([]);
    vi.mocked(ampCreateCompute).mockRejectedValue(new Error("402"));
    vi.mocked(ampCoreLimitMessage).mockReturnValue("Upgrade to Standard to use more vCPU.");
    renderWithClient(<BranchCompute datapackId="dp" clusterId="cl" branchId="br" branchName="main" isDefaultBranch={false} />);

    const btn = await screen.findByRole("button", { name: /provision compute/i });
    await waitFor(() => expect((btn as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(btn);

    // CoreLimitNotice renders the server message + a plain-anchor Settings link.
    expect(await screen.findByText(/Upgrade to Standard to use more vCPU\./)).toBeTruthy();
    expect(screen.getByRole("link", { name: /go to settings/i })).toBeTruthy();
  });
});
