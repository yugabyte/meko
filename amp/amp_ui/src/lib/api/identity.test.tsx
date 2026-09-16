import { describe, it, expect, afterEach, vi } from "vitest";
import { renderHook, waitFor, cleanup } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";

// Two identities, deliberately separate. useCurrentUser is the pool owner on
// create and the Home page's owner filter; useAdminUser is only the acting
// identity for the admin-only extension APIs. Folding the admin preference into
// useCurrentUser re-owned new pools and emptied the Home dashboard, because
// existing pools carry the bootstrap user's id and nothing re-owns them.
const users = [
  { id: "bootstrap", is_admin: false, created_at: "2026-01-01T00:00:00Z" },
  { id: "someone", is_admin: false, created_at: "2026-01-01T00:00:00Z" },
  { id: "the-admin", is_admin: true, created_at: "2026-01-01T00:00:00Z" },
];
let served = users;

vi.mock("@/lib/api/resources", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api/resources")>();
  return {
    ...actual,
    listUsers: async () => ({ users: served, total: served.length, count: served.length }),
  };
});

const { useCurrentUser, useAdminUser } = await import("./queries");

function wrapper({ children }: { children: ReactNode }) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
}

afterEach(() => {
  cleanup();
  served = users;
});

describe("identity", () => {
  it("useCurrentUser stays the bootstrap user even when an admin exists", async () => {
    const { result } = renderHook(() => useCurrentUser(), { wrapper });
    await waitFor(() => expect(result.current.data).toBeTruthy());
    // Not "the-admin": pool ownership and the Home owner filter hang off this.
    expect(result.current.data?.id).toBe("bootstrap");
  });

  it("useAdminUser finds the admin wherever it sits in the list", async () => {
    const { result } = renderHook(() => useAdminUser(), { wrapper });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data?.id).toBe("the-admin");
  });

  it("useAdminUser reports none rather than inventing one", async () => {
    served = users.filter((u) => !u.is_admin);
    const { result } = renderHook(() => useAdminUser(), { wrapper });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data).toBeNull();
  });
});
