import { describe, it, expect, vi, beforeEach } from "vitest";

// Lock the Meko-UI → api_server wire contract for the AMP surface: exact path,
// method, and body per call. If api_server renames a route or changes a field
// (project→cluster, flat cpu/memory, the /resize sub-path), a test here fails
// instead of the mismatch only surfacing at runtime behind the SHOW_PG_AMP flag.
// This complements the Go client tests, which lock api_server → AMP controller.
vi.mock("@/lib/api/mekoApi", () => ({
  request: vi.fn(async () => ({})),
}));

import { request } from "@/lib/api/mekoApi";
import {
  createBranch,
  createCluster,
  createCompute,
  deleteCompute,
  listBranches,
  listClusters,
  listCompute,
  resizeCompute,
} from "@/lib/api/ampApi";

const mockRequest = vi.mocked(request);

beforeEach(() => mockRequest.mockClear());

describe("ampApi wire contract", () => {
  it("listClusters → GET /datapacks/:id/clusters", async () => {
    await listClusters("dp1");
    expect(mockRequest).toHaveBeenCalledWith("/datapacks/dp1/clusters", "GET", undefined, {});
  });

  it("createCluster → POST with a trimmed name and NO pg_version (PG18 default)", async () => {
    await createCluster("dp1", { name: "  sales  " });
    expect(mockRequest).toHaveBeenCalledWith("/datapacks/dp1/clusters", "POST", { name: "sales" });
  });

  it("createCluster forwards pg_version only when explicitly set", async () => {
    await createCluster("dp1", { name: "sales", pg_version: 18 });
    expect(mockRequest).toHaveBeenCalledWith("/datapacks/dp1/clusters", "POST", { name: "sales", pg_version: 18 });
  });

  it("listBranches → GET .../clusters/:cid/branches", async () => {
    await listBranches("dp1", "cl1");
    expect(mockRequest).toHaveBeenCalledWith("/datapacks/dp1/clusters/cl1/branches", "GET", undefined, {});
  });

  it("createBranch omits fork_point_lsn when blank", async () => {
    await createBranch("dp1", "cl1", { name: "feat", parent_branch_id: "b0", fork_point_lsn: "  " });
    expect(mockRequest).toHaveBeenCalledWith(
      "/datapacks/dp1/clusters/cl1/branches",
      "POST",
      { name: "feat", parent_branch_id: "b0" },
    );
  });

  it("listCompute → GET .../branches/:bid/compute", async () => {
    await listCompute("dp1", "cl1", "br1");
    expect(mockRequest).toHaveBeenCalledWith("/datapacks/dp1/clusters/cl1/branches/br1/compute", "GET", undefined, {});
  });

  it("createCompute nests role + cpu/memory, omitting empties", async () => {
    await createCompute("dp1", "cl1", "br1", "read_only", { cpu: "2", memory: "" });
    expect(mockRequest).toHaveBeenCalledWith(
      "/datapacks/dp1/clusters/cl1/branches/br1/compute",
      "POST",
      { role: "read_only", cpu: "2" },
    );
  });

  it("resizeCompute → POST .../compute/:id/resize with only the set fields", async () => {
    await resizeCompute("dp1", "cl1", "br1", "e1", { cpu: "4", memory: "  " });
    expect(mockRequest).toHaveBeenCalledWith(
      "/datapacks/dp1/clusters/cl1/branches/br1/compute/e1/resize",
      "POST",
      { cpu: "4" },
    );
  });

  it("deleteCompute → DELETE .../compute/:id", async () => {
    await deleteCompute("dp1", "cl1", "br1", "e1");
    expect(mockRequest).toHaveBeenCalledWith("/datapacks/dp1/clusters/cl1/branches/br1/compute/e1", "DELETE");
  });
});
