import { describe, it, expect, afterEach, vi } from "vitest";
import {
  render,
  screen,
  cleanup,
  fireEvent,
  waitFor as waitForRTL,
} from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { StoragePool } from "@/lib/api/types";

// The whole suite runs files in parallel, so these async settles compete for a
// loaded event loop; RTL's 1s default is tight enough to flake there.
const waitFor = (cb: () => void) => waitForRTL(cb, { timeout: 5000 });

const pool: StoragePool = {
  id: "pool-1",
  name: "payments-bu",
  owner_user_id: "user-1",
  created_at: "2026-01-01T00:00:00Z",
};

// The page derives each pool's cluster count from the fleet fan-out, so the
// controller's list endpoints are the seam worth faking.
const listClusters = vi.fn();
vi.mock("@/lib/api/resources", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api/resources")>();
  return {
    ...actual,
    listUsers: async () => ({ users: [{ id: "user-1", is_admin: false }], total: 1, count: 1 }),
    listStoragePools: async () => ({ storage_pools: [pool], total: 1, count: 1 }),
    listClusters: (...args: unknown[]) => listClusters(...args),
  };
});

const { default: SharedPoolsPage, PoolCard, RenamePoolDialog } = await import(
  "./SharedPoolsPage"
);

afterEach(() => {
  cleanup();
  listClusters.mockReset();
});

function withQuery(ui: React.ReactElement) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter>{ui}</MemoryRouter>
    </QueryClientProvider>,
  );
}

async function openPageMenu() {
  withQuery(<SharedPoolsPage />);
  const trigger = await screen.findByRole(
    "button",
    { name: /actions for payments-bu/i },
    { timeout: 5000 },
  );
  // Wait for the fleet fan-out to settle — before it does, the count is
  // legitimately unknown and Delete is held back.
  await waitFor(() => expect(listClusters).toHaveBeenCalled());
  fireEvent.keyDown(trigger, { key: "Enter" });
  return screen.getByRole("menuitem", { name: /delete/i });
}

// A pool with no clusters contributes no fleet rows, so it has no entry in the
// count map at all. Reading that absence as "unknown" disabled Delete for
// precisely the pools the controller would let you delete — the whole feature.
describe("SharedPoolsPage cluster counts", () => {
  it("enables delete for a pool the fleet reports no clusters for", async () => {
    listClusters.mockResolvedValue({ clusters: [], total: 0, count: 0 });
    const item = await openPageMenu();
    await waitFor(() =>
      expect(item.getAttribute("aria-disabled")).not.toBe("true"),
    );
    expect(screen.getByText("0")).toBeTruthy();
  });

  it("disables delete while the pool still holds a cluster", async () => {
    listClusters.mockResolvedValue({
      clusters: [{ id: "c1", storage_pool_id: pool.id, name: "db" }],
      total: 1,
      count: 1,
    });
    const item = await openPageMenu();
    expect(item.getAttribute("aria-disabled")).toBe("true");
  });

  // Failed-creation clusters are still live rows the controller counts, so the
  // fan-out has to ask for them or Delete would 409.
  it("asks the fleet for failed-creation clusters too", async () => {
    listClusters.mockResolvedValue({ clusters: [], total: 0, count: 0 });
    await openPageMenu();
    expect(listClusters).toHaveBeenCalledWith(pool.id, { includeFailed: true });
  });
});

describe("PoolCard", () => {
  it("withholds delete while the cluster count is still unknown", () => {
    withQuery(
      <PoolCard
        pool={pool}
        dbCount={undefined}
        dbCountLoading
        onOpen={vi.fn()}
        onRename={vi.fn()}
        onDelete={vi.fn()}
      />,
    );
    fireEvent.keyDown(
      screen.getByRole("button", { name: /actions for payments-bu/i }),
      { key: "Enter" },
    );
    expect(
      screen.getByRole("menuitem", { name: /delete/i }).getAttribute("aria-disabled"),
    ).toBe("true");
  });
});

describe("RenamePoolDialog", () => {
  it("keeps the submit disabled until the name actually changes", () => {
    withQuery(<RenamePoolDialog pool={pool} onOpenChange={vi.fn()} />);
    const btn = screen.getByRole("button", { name: /rename pool/i }) as HTMLButtonElement;
    const input = screen.getByLabelText(/pool name/i);

    expect(btn.disabled).toBe(true); // seeded with the current name

    fireEvent.change(input, { target: { value: "   " } });
    expect(btn.disabled).toBe(true); // blank

    fireEvent.change(input, { target: { value: "  payments-bu  " } });
    expect(btn.disabled).toBe(true); // same name, just padded

    fireEvent.change(input, { target: { value: "payments-team" } });
    expect(btn.disabled).toBe(false);
  });
});
