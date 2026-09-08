import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { type ReactElement } from "react";
import { render, screen, fireEvent, waitFor, cleanup } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { CreateClusterWizard } from "./CreateClusterWizard";
import { ampCreateCluster } from "@/lib/api/ampApi";

// The wizard's contract: PostgreSQL/AI Database/Oracle are selectable engines
// (SQL Server + DocumentDB are "coming soon"), an Extensions step seeds the
// main branch, and finishing fires ONE composite create with engine + extras.
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
  it("greys out roadmap engines and keeps PostgreSQL / AI Database / Oracle selectable", () => {
    renderWizard(<CreateClusterWizard open onOpenChange={() => {}} datapackId="dp" />);
    expect((screen.getByRole("button", { name: /sql server/i }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: /documentdb/i }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: /postgresql/i }) as HTMLButtonElement).disabled).toBe(false);
    expect((screen.getByRole("button", { name: /oracle/i }) as HTMLButtonElement).disabled).toBe(false);
  });

  it("fires ONE composite create with the name + writer sizing (default engine, no extras)", async () => {
    renderWizard(<CreateClusterWizard open onOpenChange={() => {}} datapackId="dp" />);
    fireEvent.change(screen.getByLabelText("Cluster name"), { target: { value: "orders" } });
    fireEvent.click(screen.getByRole("button", { name: "Next" })); // Cluster → Extensions
    fireEvent.click(screen.getByRole("button", { name: "Next" })); // Extensions → Compute
    fireEvent.click(screen.getByRole("button", { name: "Next" })); // Compute → Review
    fireEvent.click(screen.getByRole("button", { name: "Create cluster" }));

    await waitFor(() => expect(mockCreate).toHaveBeenCalledTimes(1));
    expect(mockCreate).toHaveBeenCalledWith("dp", {
      name: "orders",
      cpu: "2",
      memory: "4Gi",
      engine: "postgres",
      extensions: [],
    });
  });

  it("sends picked extras, and never re-sends the engine's own bundle", async () => {
    renderWizard(<CreateClusterWizard open onOpenChange={() => {}} datapackId="dp" />);
    fireEvent.change(screen.getByLabelText("Cluster name"), { target: { value: "vec" } });
    // Switch to AI Database (seeds pgvector/age/pg_cron — those render locked).
    fireEvent.click(screen.getByRole("button", { name: /ai database/i }));
    fireEvent.click(screen.getByRole("button", { name: "Next" })); // Cluster → Extensions
    // hexabridge is NOT in the aidb bundle, so it's a togglable extra.
    fireEvent.click(screen.getByRole("checkbox", { name: /hexabridge/i }));
    fireEvent.click(screen.getByRole("button", { name: "Next" })); // Extensions → Compute
    fireEvent.click(screen.getByRole("button", { name: "Next" })); // Compute → Review
    fireEvent.click(screen.getByRole("button", { name: "Create cluster" }));

    await waitFor(() => expect(mockCreate).toHaveBeenCalledTimes(1));
    expect(mockCreate).toHaveBeenCalledWith("dp", {
      name: "vec",
      cpu: "2",
      memory: "4Gi",
      engine: "aidb",
      extensions: ["hexabridge"],
    });
  });
});
