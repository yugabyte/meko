import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, cleanup, fireEvent, waitFor as waitForRTL } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import type { StoragePool, Task } from "@/lib/api/types";

afterEach(cleanup);

// Radix's Select drives the DOM APIs jsdom does not implement. Without these the
// pool picker throws on open and every test in this file fails for a reason that
// has nothing to do with the wizard.
Element.prototype.scrollIntoView = vi.fn();
globalThis.ResizeObserver ??= class {
  observe() {}
  unobserve() {}
  disconnect() {}
} as unknown as typeof ResizeObserver;
Element.prototype.hasPointerCapture ??= () => false;
Element.prototype.setPointerCapture ??= () => {};
Element.prototype.releasePointerCapture ??= () => {};

const waitFor = (cb: () => void) => waitForRTL(cb, { timeout: 5000 });

const navigate = vi.fn();
vi.mock("react-router-dom", async (importActual) => {
  const actual = await importActual<typeof import("react-router-dom")>();
  return { ...actual, useNavigate: () => navigate };
});

// The composite create is the seam: its Task is the only carrier of a generated
// password, so faking it is how the one-time display can be exercised.
const createClusterWithWriter = vi.fn();
vi.mock("@/lib/api/resources", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api/resources")>();
  return {
    ...actual,
    createClusterWithWriter: (...args: unknown[]) => createClusterWithWriter(...args),
  };
});

const { CreateClusterWizard } = await import("./CreateClusterWizard");

const pool: StoragePool = {
  id: "pool-1",
  name: "payments",
  owner_user_id: "user-1",
  created_at: "2026-01-01T00:00:00Z",
};

const task = (): Task => ({
  id: "task-1",
  kind: "create_cluster",
  status: "pending",
  cluster_id: null,
  target_ref: null,
  error: null,
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
});

const PASSWORD = "hunter2hunter2";

function renderWizard() {
  const qc = new QueryClient();
  return render(
    <MemoryRouter>
      <QueryClientProvider client={qc}>
        <CreateClusterWizard open onOpenChange={() => {}} pools={[pool]} />
      </QueryClientProvider>
    </MemoryRouter>,
  );
}

// Walk Cluster -> Extensions -> Compute -> Review and submit. The Compute step
// will not advance without a password, so filling it is part of the walk.
async function fillAndCreate({
  password = PASSWORD,
  onCompute,
}: { password?: string; onCompute?: () => void } = {}) {
  fireEvent.change(screen.getByLabelText(/cluster name/i), { target: { value: "orders" } });
  // The pool Select is a Radix trigger; the wizard needs a pool id before Next.
  fireEvent.click(screen.getByText(/select a pool/i));
  await waitFor(() => expect(screen.getByText("payments")).toBeTruthy());
  fireEvent.click(screen.getByText("payments"));

  // Cluster -> Extensions, Extensions -> Compute.
  for (let i = 0; i < 2; i++) {
    await waitFor(() => {
      const b = screen.getByRole("button", { name: /next/i }) as HTMLButtonElement;
      expect(b.disabled).toBe(false);
    });
    fireEvent.click(screen.getByRole("button", { name: /next/i }));
  }
  // The Compute step carries the writer's password.
  if (password) {
    fireEvent.change(screen.getByLabelText(/^password$/i), { target: { value: password } });
  }
  onCompute?.();
  await waitFor(() => {
    const b = screen.getByRole("button", { name: /next/i }) as HTMLButtonElement;
    expect(b.disabled).toBe(false);
  });
  fireEvent.click(screen.getByRole("button", { name: /next/i }));
  fireEvent.click(screen.getByRole("button", { name: /create cluster/i }));
}

describe("CreateClusterWizard", () => {
  // The writer's password reaches the controller, and the wizard goes on to
  // Tasks: there is nothing to reveal, because nothing was generated.
  it("sends the writer's password and navigates to Tasks", async () => {
    navigate.mockClear();
    createClusterWithWriter.mockResolvedValueOnce(task());
    renderWizard();
    await fillAndCreate();

    await waitFor(() => expect(createClusterWithWriter).toHaveBeenCalled());
    expect(createClusterWithWriter.mock.calls.at(-1)![1].password).toBe(PASSWORD);
    await waitFor(() => expect(navigate).toHaveBeenCalledWith("/tasks"));
  });

  // The controller mints no password, so a with_writer create without one is a
  // 400. The Compute step is what stops that reaching it.
  it("will not leave the Compute step without a password", async () => {
    renderWizard();
    fireEvent.change(screen.getByLabelText(/cluster name/i), { target: { value: "orders" } });
    fireEvent.click(screen.getByText(/select a pool/i));
    await waitFor(() => expect(screen.getByText("payments")).toBeTruthy());
    fireEvent.click(screen.getByText("payments"));
    for (let i = 0; i < 2; i++) {
      fireEvent.click(screen.getByRole("button", { name: /next/i }));
    }
    // On Compute now, with the password blank.
    await waitFor(() => expect(screen.getByLabelText(/^password$/i)).toBeTruthy());
    expect((screen.getByRole("button", { name: /next/i }) as HTMLButtonElement).disabled).toBe(
      true,
    );

    fireEvent.change(screen.getByLabelText(/^password$/i), { target: { value: PASSWORD } });
    expect((screen.getByRole("button", { name: /next/i }) as HTMLButtonElement).disabled).toBe(
      false,
    );
  });

  // A blank username still means "take the controller's default"; "" is a 400.
  it("omits a blank username from the request", async () => {
    createClusterWithWriter.mockResolvedValueOnce(task());
    renderWizard();
    await fillAndCreate();

    await waitFor(() => expect(createClusterWithWriter).toHaveBeenCalled());
    expect(createClusterWithWriter.mock.calls.at(-1)![1].username).toBeUndefined();
  });

  it("sends no scheduling fields unless some were set", async () => {
    createClusterWithWriter.mockResolvedValueOnce(task());
    renderWizard();
    await fillAndCreate();

    await waitFor(() => expect(createClusterWithWriter).toHaveBeenCalled());
    const body = createClusterWithWriter.mock.calls.at(-1)![1];
    expect(body.tolerations ?? []).toEqual([]);
    expect(body.topologySpreadConstraints ?? []).toEqual([]);
  });

  // The cluster has no id before the create, so the prefilled selector is the pool's.
  it("sends a spread constraint added on the Compute step", async () => {
    createClusterWithWriter.mockResolvedValueOnce(task());
    renderWizard();
    await fillAndCreate({
      onCompute: () => {
        fireEvent.click(screen.getByRole("button", { name: /scheduling \(advanced\)/i }));
        fireEvent.click(screen.getByRole("button", { name: /add spread constraint/i }));
      },
    });

    await waitFor(() => expect(createClusterWithWriter).toHaveBeenCalled());
    expect(createClusterWithWriter.mock.calls.at(-1)![1].topologySpreadConstraints).toEqual([
      {
        maxSkew: 1,
        topologyKey: "topology.kubernetes.io/zone",
        whenUnsatisfiable: "ScheduleAnyway",
        labelSelector: { matchLabels: { "amp.yugabyte.com/storage-pool-id": "pool-1" } },
      },
    ]);
  });
});
