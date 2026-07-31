import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { type ReactElement } from "react";
import { render, screen, fireEvent, waitFor, cleanup } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { CreateClusterWizard } from "./CreateClusterWizard";
import { ampCreateCluster } from "@/lib/api/ampApi";

// The wizard's contract: only PostgreSQL is selectable (the rest are "coming
// soon"), and finishing fires ONE composite create with the writer sizing.
vi.mock("@/lib/api/ampApi", () => ({
  ampCreateCluster: vi.fn().mockResolvedValue({ id: "c1", name: "orders" }),
}));
vi.mock("@/hooks/use-toast", () => ({ toast: vi.fn() }));

const mockCreate = vi.mocked(ampCreateCluster);

function renderWizard(ui: ReactElement) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
}

afterEach(cleanup);
beforeEach(() => vi.clearAllMocks());

describe("CreateClusterWizard", () => {
  it("greys out coming-soon engines and keeps PostgreSQL selectable", () => {
    renderWizard(<CreateClusterWizard open onOpenChange={() => {}} datapackId="dp" />);
    expect((screen.getByRole("button", { name: /oracle/i }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: /documentdb/i }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: /postgresql/i }) as HTMLButtonElement).disabled).toBe(false);
  });

  it("fires ONE composite create with the name + writer sizing", async () => {
    renderWizard(<CreateClusterWizard open onOpenChange={() => {}} datapackId="dp" />);
    fireEvent.change(screen.getByLabelText("Cluster name"), { target: { value: "orders" } });
    fireEvent.click(screen.getByRole("button", { name: "Next" })); // Cluster → Compute
    fireEvent.click(screen.getByRole("button", { name: "Next" })); // Compute → Review
    fireEvent.click(screen.getByRole("button", { name: "Create cluster" }));

    await waitFor(() => expect(mockCreate).toHaveBeenCalledTimes(1));
    expect(mockCreate).toHaveBeenCalledWith("dp", { name: "orders", cpu: "2", memory: "4Gi" });
  });
});
