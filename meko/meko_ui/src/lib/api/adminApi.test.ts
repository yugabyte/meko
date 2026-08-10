import { describe, it, expect, vi, beforeEach } from "vitest";

// Lock the admin tier-audit wire contract: tier changes must forward the
// operator as X-Meko-Admin-Actor-* headers (so the backend can audit who), and
// the history call must hit the tier-audit route.
vi.mock("@/lib/api/mekoApi", () => ({
  request: vi.fn(async () => ({})),
  MekoApiRequestError: class extends Error {},
}));

import { request } from "@/lib/api/mekoApi";
import { updateUserTier, fetchUserTierAudit } from "@/lib/api/adminApi";

const mockRequest = vi.mocked(request);

beforeEach(() => mockRequest.mockClear());

describe("adminApi tier audit wire contract", () => {
  it("updateUserTier forwards the operator as actor headers", async () => {
    await updateUserTier("u1", "pro", { email: "ops@example.com", uuid: "sub-1" });
    expect(mockRequest).toHaveBeenCalledWith(
      "/admin/users/u1/tier",
      "PATCH",
      { tier: "pro" },
      { headers: { "X-Meko-Admin-Actor-Email": "ops@example.com", "X-Meko-Admin-Actor-Uuid": "sub-1" } },
    );
  });

  it("updateUserTier omits actor headers when no operator is given", async () => {
    await updateUserTier("u1", "free");
    expect(mockRequest).toHaveBeenCalledWith("/admin/users/u1/tier", "PATCH", { tier: "free" }, { headers: {} });
  });

  it("fetchUserTierAudit → GET /admin/users/:id/tier-audit", async () => {
    await fetchUserTierAudit("u1");
    expect(mockRequest).toHaveBeenCalledWith("/admin/users/u1/tier-audit", "GET", undefined, {});
  });
});
