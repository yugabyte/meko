import { describe, it, expect, vi, beforeEach } from "vitest";

// Lock the Meko-UI → api_server wire contract for the deployments surface,
// same rationale as ampApi.test.ts: a route/body mismatch fails here instead
// of only surfacing at runtime inside the Labs deploy panel.
vi.mock("@/lib/api/mekoApi", () => ({
  request: vi.fn(async () => ({})),
}));

import { request } from "@/lib/api/mekoApi";
import {
  confirmByocDeployment,
  createManagedDeployment,
  getDeployment,
  initByocDeployment,
} from "@/lib/api/deploymentsApi";

const mockRequest = vi.mocked(request);

beforeEach(() => mockRequest.mockClear());

describe("deploymentsApi wire contract", () => {
  it("initByocDeployment → POST /deployments/byoc/init", async () => {
    await initByocDeployment("adlc-review-agent");
    expect(mockRequest).toHaveBeenCalledWith("/deployments/byoc/init", "POST", {
      agent_slug: "adlc-review-agent",
    });
  });

  it("confirmByocDeployment → POST /deployments/:id/byoc/confirm", async () => {
    await confirmByocDeployment("dep-1", "arn:aws:iam::123456789012:role/meko-agentcore-deploy");
    expect(mockRequest).toHaveBeenCalledWith("/deployments/dep-1/byoc/confirm", "POST", {
      role_arn: "arn:aws:iam::123456789012:role/meko-agentcore-deploy",
    });
  });

  it("createManagedDeployment → POST /deployments", async () => {
    await createManagedDeployment("adlc-review-agent");
    expect(mockRequest).toHaveBeenCalledWith("/deployments", "POST", {
      agent_slug: "adlc-review-agent",
    });
  });

  it("getDeployment → GET /deployments/:id", async () => {
    await getDeployment("dep-1");
    expect(mockRequest).toHaveBeenCalledWith("/deployments/dep-1", "GET", undefined, {});
  });
});
