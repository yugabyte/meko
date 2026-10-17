import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { type ReactElement } from "react";
import { render, screen, fireEvent, waitFor, cleanup, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { CreateClusterWizard } from "./CreateClusterWizard";
import { ampCreateCluster, type AmpCluster } from "@/lib/api/ampApi";

// The wizard's contract: PostgreSQL/AI Database/Oracle/SQL Server/DocumentDB are
// selectable engines, an Extensions step seeds the
// main branch, and finishing fires ONE composite create with engine + extras.
vi.mock("@/lib/api/ampApi", () => ({
  ampCreateCluster: vi.fn().mockResolvedValue({ id: "c1", name: "orders" }),
  // Unlimited (Enterprise) budget → no remaining-vCPU cap, so the wizard keeps
  // the default 0.5 vCPU / 4Gi sizing.
  ampCoreBudget: vi.fn().mockResolvedValue({
    tier: "enterprise",
    next_tier: "",
    used_cores: 0,
    max_cores: null,
    at_limit: false,
  }),
  ampCoreLimitMessage: vi.fn().mockReturnValue(null),
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
  it("keeps PostgreSQL / AI Database / Oracle / SQL Server / DocumentDB selectable", () => {
    renderWizard(<CreateClusterWizard open onOpenChange={() => {}} datapackId="dp" />);
    expect(
      (screen.getByRole("button", { name: /sql server/i }) as HTMLButtonElement).disabled,
    ).toBe(false);
    expect(
      (screen.getByRole("button", { name: /documentdb/i }) as HTMLButtonElement).disabled,
    ).toBe(false);
    expect(
      (screen.getByRole("button", { name: /postgresql/i }) as HTMLButtonElement).disabled,
    ).toBe(false);
    expect((screen.getByRole("button", { name: /oracle/i }) as HTMLButtonElement).disabled).toBe(
      false,
    );
  });

  it("fires ONE composite create with the name + writer sizing (default engine, no extras)", async () => {
    renderWizard(<CreateClusterWizard open onOpenChange={() => {}} datapackId="dp" />);
    fireEvent.change(screen.getByLabelText("Cluster name"), { target: { value: "orders" } });
    fireEvent.click(screen.getByRole("button", { name: "Next" })); // Cluster → Extensions
    fireEvent.click(screen.getByRole("button", { name: "Next" })); // Extensions → Compute
    fireEvent.click(screen.getByRole("button", { name: "Next" })); // Compute → Credentials
    enterCustomPassword("pw-abcdefgh");
    fireEvent.click(screen.getByRole("button", { name: "Next" })); // Credentials → Review
    // Create is held until the budget query resolves (loading-window guard).
    const createBtn = screen.getByRole("button", { name: "Create cluster" }) as HTMLButtonElement;
    await waitFor(() => expect(createBtn.disabled).toBe(false));
    fireEvent.click(createBtn);

    await waitFor(() => expect(mockCreate).toHaveBeenCalledTimes(1));
    expect(mockCreate).toHaveBeenCalledWith("dp", {
      name: "orders",
      cpu: "0.5",
      memory: "4Gi",
      password: "pw-abcdefgh",
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
    fireEvent.click(screen.getByRole("checkbox", { name: /hexabridge(?!_mssql)/i }));
    fireEvent.click(screen.getByRole("button", { name: "Next" })); // Extensions → Compute
    fireEvent.click(screen.getByRole("button", { name: "Next" })); // Compute → Credentials
    enterCustomPassword("pw-abcdefgh");
    fireEvent.click(screen.getByRole("button", { name: "Next" })); // Credentials → Review
    const createBtn = screen.getByRole("button", { name: "Create cluster" }) as HTMLButtonElement;
    await waitFor(() => expect(createBtn.disabled).toBe(false));
    fireEvent.click(createBtn);

    await waitFor(() => expect(mockCreate).toHaveBeenCalledTimes(1));
    expect(mockCreate).toHaveBeenCalledWith("dp", {
      name: "vec",
      cpu: "0.5",
      memory: "4Gi",
      password: "pw-abcdefgh",
      engine: "aidb",
      extensions: ["hexabridge"],
    });
  });

  // The Credentials step gates the create: the controller requires a cloud_admin
  // password, so the wizard can't advance past it until one is set.
  it("blocks advancing from Credentials until a password is set", () => {
    renderWizard(<CreateClusterWizard open onOpenChange={() => {}} datapackId="dp" />);
    fireEvent.change(screen.getByLabelText("Cluster name"), { target: { value: "orders" } });
    fireEvent.click(screen.getByRole("button", { name: "Next" })); // Cluster → Extensions
    fireEvent.click(screen.getByRole("button", { name: "Next" })); // Extensions → Compute
    fireEvent.click(screen.getByRole("button", { name: "Next" })); // Compute → Credentials
    // Default (generate) mode is unacknowledged, so Next is held.
    const next = () => screen.getByRole("button", { name: "Next" }) as HTMLButtonElement;
    expect(next().disabled).toBe(true);
    // A short/invalid typed password keeps it blocked; a valid, confirmed one frees it.
    enterCustomPassword("short");
    expect(next().disabled).toBe(true);
    enterCustomPassword("pw-abcdefgh");
    expect(next().disabled).toBe(false);
  });

  // While the composite create task is in flight, the wizard swaps the step body
  // for a provisioning panel (spinner, not a hung button) and LOCKS the dialog:
  // the close X is hidden and Esc can't dismiss it until the task resolves.
  it("locks the dialog and shows a provisioning state while the create task runs", async () => {
    let resolveCreate: (v: AmpCluster) => void = () => {};
    mockCreate.mockImplementationOnce(() => new Promise<AmpCluster>((r) => (resolveCreate = r)));
    const onOpenChange = vi.fn();
    renderWizard(<CreateClusterWizard open onOpenChange={onOpenChange} datapackId="dp" />);
    fireEvent.change(screen.getByLabelText("Cluster name"), { target: { value: "orders" } });
    fireEvent.click(screen.getByRole("button", { name: "Next" })); // Cluster → Extensions
    fireEvent.click(screen.getByRole("button", { name: "Next" })); // Extensions → Compute
    fireEvent.click(screen.getByRole("button", { name: "Next" })); // Compute → Credentials
    enterCustomPassword("pw-abcdefgh");
    fireEvent.click(screen.getByRole("button", { name: "Next" })); // Credentials → Review
    const createBtn = screen.getByRole("button", { name: "Create cluster" }) as HTMLButtonElement;
    await waitFor(() => expect(createBtn.disabled).toBe(false));
    fireEvent.click(createBtn);

    // Provisioning panel shown, review content gone, and the button really spins
    // (the Spinner carries aria-label "Loading").
    const creatingBtn = await screen.findByRole("button", { name: /creating/i });
    expect(screen.getByText(/Provisioning your cluster/i)).toBeTruthy();
    expect(within(creatingBtn).getByLabelText("Loading")).toBeTruthy();
    expect(screen.queryByText("Writer compute")).toBeNull();

    // Locked: Escape doesn't dismiss it (onOpenChange stays uncalled). (The X is
    // also hidden via a CSS class, which jsdom can't evaluate, so that's a visual
    // check — the functional guard is what's asserted here.)
    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" });
    expect(onOpenChange).not.toHaveBeenCalled();

    // Once the task resolves, the wizard closes itself.
    resolveCreate({ id: "c1", name: "orders" } as AmpCluster);
    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
  });
});

// Fill the Credentials step's custom path with a known password + confirmation.
function enterCustomPassword(pw: string) {
  fireEvent.click(screen.getByRole("button", { name: /use my own/i }));
  fireEvent.change(screen.getByLabelText("Password"), { target: { value: pw } });
  fireEvent.change(screen.getByLabelText("Confirm password"), { target: { value: pw } });
}
