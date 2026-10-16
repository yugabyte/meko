import { describe, it, expect, afterEach, vi } from "vitest";
import { createClusterWithWriter, updateEndpointScheduling } from "./resources";

// The wire bodies for endpoint scheduling. On create an empty list is omitted
// rather than sent; on update the two differ — omitted keeps, `[]` clears.

type Call = { url: string; method: string; body: unknown };
let calls: Call[] = [];

function stubFetch(replies: Record<string, unknown>) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init?: RequestInit) => {
      const method = init?.method ?? "GET";
      calls.push({ url, method, body: init?.body ? JSON.parse(String(init.body)) : undefined });
      const key = Object.keys(replies).find((k) => url.endsWith(k));
      return new Response(JSON.stringify(key ? replies[key] : {}), {
        status: method === "POST" ? 202 : 200,
      });
    }),
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
  calls = [];
});

const task = { id: "t1", kind: "k", status: "succeeded", cluster_id: null, target_ref: null };
const spread = [
  {
    maxSkew: 1,
    topologyKey: "topology.kubernetes.io/zone",
    whenUnsatisfiable: "ScheduleAnyway" as const,
    labelSelector: { matchLabels: { "amp.yugabyte.com/storage-pool-id": "p" } },
  },
];

describe("createClusterWithWriter", () => {
  it("omits empty scheduling lists", async () => {
    stubFetch({ "/clusters": task });
    await createClusterWithWriter("p", {
      name: "c",
      password: "pw",
      tolerations: [],
      topologySpreadConstraints: [],
    });
    expect(calls[0].body).not.toHaveProperty("tolerations");
    expect(calls[0].body).not.toHaveProperty("topology_spread_constraints");
  });

  it("sends the lists it is given", async () => {
    stubFetch({ "/clusters": task });
    await createClusterWithWriter("p", {
      name: "c",
      password: "pw",
      tolerations: [{ key: "dedicated", operator: "Exists" }],
      topologySpreadConstraints: spread,
    });
    expect(calls[0].body).toMatchObject({
      with_writer: true,
      tolerations: [{ key: "dedicated", operator: "Exists" }],
      topology_spread_constraints: spread,
    });
  });
});

describe("updateEndpointScheduling", () => {
  it("posts only the lists named, polls the task, and reads the endpoint back", async () => {
    stubFetch({ "/scheduling": task, "/tasks/t1": task, "/endpoints/e": { id: "e" } });
    const ep = await updateEndpointScheduling("p", "c", "b", "e", { tolerations: [] });

    expect(calls[0]).toEqual({
      url: expect.stringMatching(
        /\/storage-pools\/p\/clusters\/c\/branches\/b\/endpoints\/e\/scheduling$/,
      ),
      method: "POST",
      body: { tolerations: [] },
    });
    expect(calls.some((c) => c.url.endsWith("/tasks/t1"))).toBe(true);
    expect(ep.id).toBe("e");
  });
});
