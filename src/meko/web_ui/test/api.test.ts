// The UI's listing, create, search and Add memory: Meko's own REST routes, which name no location
// (Meko routes each datapack itself).

import { afterEach, expect, test, vi } from "vitest";
import { api } from "../src/lib/api";

const DP = "11111111-1111-4111-8111-111111111111";

let calls: { url: string; method: string; body: unknown }[];

function serve(data: unknown) {
  calls = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init?: RequestInit) => {
      calls.push({ url, method: init?.method ?? "GET", body: init?.body ? JSON.parse(String(init.body)) : undefined });
      return Response.json({ data, error: null });
    }),
  );
}

afterEach(() => vi.unstubAllGlobals());

test("search posts to the datapack's search route", async () => {
  serve({ results: { memory: [] } });
  expect(await api.search(DP, "deploys", "memory")).toEqual({ results: { memory: [] } });
  expect(calls).toEqual([{ url: `/api/v1/datapacks/${DP}/search`, method: "POST",
    body: { query: "deploys", force_source: "memory", agent_id: "meko_web_ui", conversation_id: "meko_web_ui" } }]);
});

test("Add memory posts to the datapack's memories route", async () => {
  serve({ results: [] });
  await api.addMemory(DP, "a fact");
  expect(calls).toEqual([{ url: `/api/v1/datapacks/${DP}/memories`, method: "POST",
    body: { agent_id: "meko_web_ui", conversation_id: "meko_web_ui", text: "a fact" } }]);
});

test("the datapacks are one listing, and a create names no location", async () => {
  serve([]);
  await api.datapacks();
  await api.createDatapack("research", "");
  expect(calls).toEqual([
    { url: "/api/v1/datapacks", method: "GET", body: undefined },
    { url: "/api/v1/datapacks", method: "POST", body: { name: "research", description: "" } },
  ]);
});
