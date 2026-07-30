import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { type ReactElement } from "react";
import { render, screen, cleanup, fireEvent, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ComputeInstance } from "./ComputeInstance";
import { deleteCompute, type AmpCompute } from "@/lib/api/ampApi";

vi.mock("@/lib/api/ampApi", () => ({
  deleteCompute: vi.fn().mockResolvedValue({ id: "e1", deleted: true }),
  resizeCompute: vi.fn(),
}));
vi.mock("@/hooks/use-toast", () => ({ toast: vi.fn() }));

const mockDelete = vi.mocked(deleteCompute);

const ep: AmpCompute = {
  id: "e1",
  project_id: "p",
  branch_id: "br",
  role: "read_write",
  address: "10.0.0.1:5432",
  resources: { cpu: "4", memory: "8Gi" },
  created_at: "",
};

function renderInstance(over: { isDefaultBranch?: boolean; ep?: AmpCompute } = {}) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const ui: ReactElement = (
    <ComputeInstance
      ep={over.ep ?? ep}
      datapackId="dp"
      clusterId="cl"
      branchId="br"
      branchName="main"
      isDefaultBranch={over.isDefaultBranch ?? false}
    />
  );
  return render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
}

afterEach(cleanup);
beforeEach(() => vi.clearAllMocks());

describe("ComputeInstance", () => {
  it("shows the cpu/memory reservation tag only when resources are present", () => {
    const withRes = renderInstance();
    expect(withRes.container.textContent).toContain("8Gi");
    cleanup();
    const bare = renderInstance({ ep: { ...ep, resources: undefined } });
    expect(bare.container.textContent).not.toContain("8Gi");
  });

  it("offers Unprovision on a forked branch but hides it on the default branch", () => {
    renderInstance({ isDefaultBranch: false });
    expect(screen.queryByRole("button", { name: /unprovision/i })).not.toBeNull();
    cleanup();
    renderInstance({ isDefaultBranch: true });
    expect(screen.queryByRole("button", { name: /unprovision/i })).toBeNull();
  });

  it("calls deleteCompute with the endpoint id after the confirm dialog", async () => {
    renderInstance({ isDefaultBranch: false });

    fireEvent.click(screen.getByRole("button", { name: /unprovision/i })); // open confirm
    const dialog = await screen.findByRole("alertdialog");
    fireEvent.click(within(dialog).getByRole("button", { name: /^unprovision$/i })); // confirm

    await waitFor(() => expect(mockDelete).toHaveBeenCalledWith("dp", "cl", "br", "e1"));
  });
});
