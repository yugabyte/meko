import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { type ReactElement } from "react";
import { render, screen, cleanup, fireEvent, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ComputeInstance } from "./ComputeInstance";
import {
  ampDeleteCompute,
  ampResizeCompute,
  ampCoreLimitMessage,
  type AmpCompute,
} from "@/lib/api/ampApi";
import { toast } from "@/hooks/use-toast";

vi.mock("@/lib/api/ampApi", () => ({
  ampDeleteCompute: vi.fn().mockResolvedValue({ id: "e1", deleted: true }),
  ampResizeCompute: vi.fn(),
  // Real impl: returns the upgrade message for a 402 core-limit rejection, else
  // null. Overridden per-test to drive the resize-failure → CoreLimitNotice path.
  ampCoreLimitMessage: vi.fn().mockReturnValue(null),
}));
vi.mock("@/hooks/use-toast", () => ({ toast: vi.fn() }));

const mockDelete = vi.mocked(ampDeleteCompute);

const ep: AmpCompute = {
  id: "e1",
  cluster_id: "p",
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

  it("calls ampDeleteCompute with the endpoint id after the confirm dialog", async () => {
    renderInstance({ isDefaultBranch: false });

    fireEvent.click(screen.getByRole("button", { name: /unprovision/i })); // open confirm
    const dialog = await screen.findByRole("alertdialog");
    fireEvent.click(within(dialog).getByRole("button", { name: /^unprovision$/i })); // confirm

    await waitFor(() => expect(mockDelete).toHaveBeenCalledWith("dp", "cl", "br", "e1"));
  });

  it("surfaces the upgrade notice (not a raw error) when a resize hits the vCPU cap", async () => {
    vi.mocked(ampResizeCompute).mockRejectedValue(new Error("402"));
    vi.mocked(ampCoreLimitMessage).mockReturnValue("Upgrade to Standard to use more vCPU.");
    renderInstance({ isDefaultBranch: false });

    fireEvent.click(screen.getByRole("button", { name: /resize/i })); // open the resize panel
    fireEvent.change(await screen.findByLabelText("Resize CPU"), { target: { value: "8" } });
    fireEvent.click(screen.getByRole("button", { name: /apply resize/i }));

    await waitFor(() => expect(toast).toHaveBeenCalled());
    const last = vi.mocked(toast).mock.calls.at(-1)?.[0];
    expect(last?.variant).toBe("destructive");
    // Render the toast's description (the CoreLimitNotice) and assert the user
    // sees the upgrade message + a Settings link — not the raw error string the
    // non-402 branch would produce.
    const notice = render(<>{last?.description}</>);
    expect(notice.getByText(/Upgrade to Standard to use more vCPU\./)).toBeTruthy();
    expect(notice.getByRole("link", { name: /go to settings/i })).toBeTruthy();
  });
});
