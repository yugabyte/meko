import { describe, it, expect, vi, beforeEach } from "vitest";

// Lock the Meko-UI → api_server wire contract for the AMP surface: exact path,
// method, and body per call. If api_server renames a route or changes a field
// (project→cluster, flat cpu/memory, with_writer, the /resize sub-path), a test
// here fails instead of the mismatch only surfacing at runtime behind the
// SHOW_PG_AMP flag. Complements the Go client tests (api_server → controller).
vi.mock("@/lib/api/mekoApi", () => ({
  request: vi.fn(async () => ({})),
}));

import { request } from "@/lib/api/mekoApi";
import {
  ampCreateBranch,
  ampCreateCluster,
  ampCreateCompute,
  ampDeleteCluster,
  ampDeleteCompute,
  ampListBranches,
  ampListClusters,
  ampListCompute,
  ampResizeCompute,
} from "@/lib/api/ampApi";

const mockRequest = vi.mocked(request);

beforeEach(() => mockRequest.mockClear());

describe("ampApi wire contract", () => {
  it("ampListClusters → GET /datapacks/:id/amp/clusters", async () => {
    await ampListClusters("dp1");
    expect(mockRequest).toHaveBeenCalledWith("/datapacks/dp1/amp/clusters", "GET", undefined, {});
  });

  it("ampCreateCluster → POST composite (with_writer) with a trimmed name", async () => {
    await ampCreateCluster("dp1", { name: "  sales  " });
    expect(mockRequest).toHaveBeenCalledWith("/datapacks/dp1/amp/clusters", "POST", {
      name: "sales",
      with_writer: true,
    });
  });

  it("ampCreateCluster forwards cpu/memory (writer sizing), omitting blanks", async () => {
    await ampCreateCluster("dp1", { name: "sales", cpu: "4", memory: "  " });
    expect(mockRequest).toHaveBeenCalledWith("/datapacks/dp1/amp/clusters", "POST", {
      name: "sales",
      with_writer: true,
      cpu: "4",
    });
  });

  it("ampCreateCluster forwards a non-default engine + non-blank extensions", async () => {
    await ampCreateCluster("dp1", {
      name: "vec",
      engine: "aidb",
      extensions: ["pgvector", "  ", "age"],
    });
    expect(mockRequest).toHaveBeenCalledWith("/datapacks/dp1/amp/clusters", "POST", {
      name: "vec",
      with_writer: true,
      engine: "aidb",
      extensions: ["pgvector", "age"],
    });
  });

  it("ampCreateCluster omits the default postgres engine and an empty extension list", async () => {
    await ampCreateCluster("dp1", { name: "plain", engine: "postgres", extensions: [] });
    expect(mockRequest).toHaveBeenCalledWith("/datapacks/dp1/amp/clusters", "POST", {
      name: "plain",
      with_writer: true,
    });
  });

  it("ampDeleteCluster → DELETE /datapacks/:id/amp/clusters/:cid", async () => {
    await ampDeleteCluster("dp1", "cl1");
    expect(mockRequest).toHaveBeenCalledWith("/datapacks/dp1/amp/clusters/cl1", "DELETE");
  });

  it("ampListBranches → GET .../amp/clusters/:cid/branches", async () => {
    await ampListBranches("dp1", "cl1");
    expect(mockRequest).toHaveBeenCalledWith("/datapacks/dp1/amp/clusters/cl1/branches", "GET", undefined, {});
  });

  it("ampCreateBranch omits fork_point_lsn when blank", async () => {
    await ampCreateBranch("dp1", "cl1", { name: "feat", parent_branch_id: "b0", fork_point_lsn: "  " });
    expect(mockRequest).toHaveBeenCalledWith(
      "/datapacks/dp1/amp/clusters/cl1/branches",
      "POST",
      { name: "feat", parent_branch_id: "b0" },
    );
  });

  it("ampListCompute → GET .../branches/:bid/compute", async () => {
    await ampListCompute("dp1", "cl1", "br1");
    expect(mockRequest).toHaveBeenCalledWith(
      "/datapacks/dp1/amp/clusters/cl1/branches/br1/compute",
      "GET",
      undefined,
      {},
    );
  });

  it("ampCreateCompute nests role + cpu/memory, omitting empties", async () => {
    await ampCreateCompute("dp1", "cl1", "br1", "read_only", { cpu: "2", memory: "" });
    expect(mockRequest).toHaveBeenCalledWith(
      "/datapacks/dp1/amp/clusters/cl1/branches/br1/compute",
      "POST",
      { role: "read_only", cpu: "2" },
    );
  });

  it("ampResizeCompute → POST .../compute/:id/resize with only the set fields", async () => {
    await ampResizeCompute("dp1", "cl1", "br1", "e1", { cpu: "4", memory: "  " });
    expect(mockRequest).toHaveBeenCalledWith(
      "/datapacks/dp1/amp/clusters/cl1/branches/br1/compute/e1/resize",
      "POST",
      { cpu: "4" },
    );
  });

  it("ampDeleteCompute → DELETE .../compute/:id", async () => {
    await ampDeleteCompute("dp1", "cl1", "br1", "e1");
    expect(mockRequest).toHaveBeenCalledWith("/datapacks/dp1/amp/clusters/cl1/branches/br1/compute/e1", "DELETE");
  });
});
