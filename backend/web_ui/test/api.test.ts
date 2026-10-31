// The UI's search and Add memory go to this computer's datapack through Meko's own REST routes, not
// /mcp, which sends every call to Meko Cloud while it's linked.

import { afterEach, expect, test, vi } from "vitest";
import { api } from "../src/lib/api";

const DP = "11111111-1111-4111-8111-111111111111";

let calls: { url: string; method: string; body: unknown }[];

function serve(data: unknown) {
  calls = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init?: RequestInit) => {
      calls.push({ url, method: init?.method ?? "GET", body: JSON.parse(String(init?.body)) });
      return Response.json({ data, error: null });
    }),
  );
}

afterEach(() => vi.unstubAllGlobals());

test("search posts to the datapack's search route on this computer", async () => {
  serve({ results: { memory: [] } });
  expect(await api.search(DP, "deploys", "memory")).toEqual({ results: { memory: [] } });
  expect(calls).toEqual([{ url: `/api/v1/datapacks/${DP}/search?location=local`, method: "POST",
    body: { query: "deploys", force_source: "memory", agent_id: "meko_web_ui", conversation_id: "meko_web_ui" } }]);
});

test("Add memory posts to the datapack's memories route on this computer", async () => {
  serve({ results: [] });
  await api.addMemory(DP, "a fact");
  expect(calls).toEqual([{ url: `/api/v1/datapacks/${DP}/memories?location=local`, method: "POST",
    body: { agent_id: "meko_web_ui", conversation_id: "meko_web_ui", text: "a fact" } }]);
});
