import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, cleanup, fireEvent, waitFor as waitForRTL } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { Endpoint } from "@/lib/api/types";

// Provisioning with a unit-less memory reserves BYTES, and the endpoint it
// creates can never be resized in place afterwards (meko#492). The controller
// answers 400, and this form must not send one in the first place.
afterEach(cleanup);

// The whole suite runs files in parallel, so these async settles compete for a
// loaded event loop; RTL's 1s default is tight enough to flake there.
const waitFor = (cb: () => void) => waitForRTL(cb, { timeout: 5000 });

const createEndpoint = vi.fn();
vi.mock("@/lib/api/resources", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api/resources")>();
  return {
    ...actual,
    createEndpoint: (...args: unknown[]) => createEndpoint(...args),
  };
});

const { EndpointsSection } = await import("./EndpointsSection");

const endpoint: Endpoint = {
  id: "ep-1",
  branch_id: "branch-1",
  cluster_id: "cluster-1",
  role: "read_write",
  address: "ep-1.ns.svc:55433",
  tailnet_address: "",
  created_at: "2026-01-01T00:00:00Z",
};

function renderSection(
  endpoints: Endpoint[] = [],
  { passwordRequired = true, isDefaultBranch = true } = {},
) {
  const qc = new QueryClient();
  return render(
    <QueryClientProvider client={qc}>
      <EndpointsSection
        poolId="pool-1"
        clusterId="cluster-1"
        branchId="branch-1"
        endpoints={endpoints}
        loading={false}
        isDefaultBranch={isDefaultBranch}
        passwordRequired={passwordRequired}
      />
    </QueryClientProvider>,
  );
}

const provisionBtn = () =>
  screen.getByRole("button", { name: /provision compute/i }) as HTMLButtonElement;

describe("EndpointsSection", () => {
  it("blocks provisioning while the memory quantity has no unit", () => {
    renderSection();
    fireEvent.change(screen.getByLabelText(/^password$/i), { target: { value: "hunter2hunter2" } });
    const memory = screen.getByLabelText(/memory/i);

    expect(provisionBtn().disabled).toBe(false);

    fireEvent.change(memory, { target: { value: "4" } });
    expect(provisionBtn().disabled).toBe(true);
    expect(memory.getAttribute("aria-invalid")).toBe("true");
    expect(screen.getByText(/needs a unit/i)).toBeTruthy();

    fireEvent.change(memory, { target: { value: "16Gi" } });
    expect(provisionBtn().disabled).toBe(false);
    expect(screen.queryByText(/needs a unit/i)).toBeNull();
  });

  // The controller mints no password, so a writer create without one is a 400.
  // The form is what stops that reaching it.
  it("blocks provisioning a writer until a password is given", () => {
    renderSection();
    expect(provisionBtn().disabled).toBe(true);
    expect(screen.getByText(/password is required/i)).toBeTruthy();

    fireEvent.change(screen.getByLabelText(/^password$/i), { target: { value: "hunter2hunter2" } });
    expect(provisionBtn().disabled).toBe(false);
    expect(screen.queryByText(/password is required/i)).toBeNull();
  });

  it("sends the password the operator typed", async () => {
    createEndpoint.mockResolvedValueOnce(endpoint);
    renderSection();
    fireEvent.change(screen.getByLabelText(/^password$/i), { target: { value: "hunter2hunter2" } });
    fireEvent.click(provisionBtn());

    await waitFor(() => expect(createEndpoint).toHaveBeenCalled());
    expect(createEndpoint.mock.calls.at(-1)![3].password).toBe("hunter2hunter2");
  });

  // Scheduling is opt-in: an untouched section sends neither list, so the
  // controller's defaults (no tolerations, no spread) apply.
  it("sends no scheduling fields unless some were set", async () => {
    createEndpoint.mockResolvedValueOnce(endpoint);
    renderSection();
    fireEvent.change(screen.getByLabelText(/^password$/i), { target: { value: "hunter2hunter2" } });
    fireEvent.click(provisionBtn());

    await waitFor(() => expect(createEndpoint).toHaveBeenCalled());
    const body = createEndpoint.mock.calls.at(-1)![3];
    expect(body).not.toHaveProperty("tolerations");
    expect(body).not.toHaveProperty("topology_spread_constraints");
  });

  it("sends a spread constraint selecting this cluster", async () => {
    createEndpoint.mockResolvedValueOnce(endpoint);
    renderSection();
    fireEvent.change(screen.getByLabelText(/^password$/i), { target: { value: "hunter2hunter2" } });
    fireEvent.click(screen.getByRole("button", { name: /scheduling \(advanced\)/i }));
    fireEvent.click(screen.getByRole("button", { name: /add spread constraint/i }));
    fireEvent.click(provisionBtn());

    await waitFor(() => expect(createEndpoint).toHaveBeenCalled());
    const body = createEndpoint.mock.calls.at(-1)![3];
    expect(body.topology_spread_constraints).toEqual([
      {
        maxSkew: 1,
        topologyKey: "topology.kubernetes.io/zone",
        whenUnsatisfiable: "ScheduleAnyway",
        labelSelector: { matchLabels: { "amp.yugabyte.com/cluster-id": "cluster-1" } },
      },
    ]);
    expect(body).not.toHaveProperty("tolerations");
  });

  it("blocks provisioning while the scheduling rules are invalid", () => {
    renderSection();
    fireEvent.change(screen.getByLabelText(/^password$/i), { target: { value: "hunter2hunter2" } });
    fireEvent.click(screen.getByRole("button", { name: /scheduling \(advanced\)/i }));
    // A blank toleration has an empty key with operator Equal.
    fireEvent.click(screen.getByRole("button", { name: /add toleration/i }));
    expect(provisionBtn().disabled).toBe(true);
  });

  // The compute's user lives in the branch's catalog, so only the writer sets
  // its password and the controller rejects one on a replica. With a writer
  // already present the form forces read-only, so the field goes away and the
  // password stops being required.
  it("hides the password field for a read-only endpoint", () => {
    renderSection([endpoint]);
    expect(screen.queryByLabelText(/^password$/i)).toBeNull();
    expect(provisionBtn().disabled).toBe(false);
  });

  // A fork uses its parent's password, and a later writer the one already set,
  // so the branch says none is needed and the form sends none.
  it("asks no password of a writer on a fork", async () => {
    createEndpoint.mockResolvedValueOnce(endpoint);
    renderSection([], { passwordRequired: false, isDefaultBranch: false });
    expect(screen.queryByLabelText(/^password$/i)).toBeNull();
    expect(screen.getByText(/uses its parent branch's password/i)).toBeTruthy();
    expect(provisionBtn().disabled).toBe(false);

    fireEvent.click(provisionBtn());
    await waitFor(() => expect(createEndpoint).toHaveBeenCalled());
    expect(createEndpoint.mock.calls.at(-1)![3].password).toBeUndefined();
  });

  it("says a recreated writer keeps the cluster's password", async () => {
    createEndpoint.mockResolvedValueOnce(endpoint);
    renderSection([], { passwordRequired: false, isDefaultBranch: true });
    expect(screen.queryByLabelText(/^password$/i)).toBeNull();
    expect(screen.getByText(/cluster already has a password/i)).toBeTruthy();
    expect(provisionBtn().disabled).toBe(false);

    fireEvent.click(provisionBtn());
    await waitFor(() => expect(createEndpoint).toHaveBeenCalled());
    expect(createEndpoint.mock.calls.at(-1)![3].password).toBeUndefined();
  });
});
