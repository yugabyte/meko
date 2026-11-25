// @vitest-environment happy-dom
// The Calls page against a faked /api/v1: the agent filter in the address,
// paging back with before (dropped when the filter changed meanwhile), and a call opening to what it
// sent and got back, or to a note that it isn't kept.

import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, expect, test, vi } from "vitest";
import App from "../src/App";
import type { AuthStatus, MekoCall } from "../src/lib/types";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const auth: AuthStatus = { access: "local", local_access: true, signed_in: true };
const AGENT = "claude_code:fix-foo-1a2b3c4d";

function call(id: number, tool: string, extra: Partial<MekoCall> = {}): MekoCall {
  return { id, at: new Date().toISOString(), tool, agent_id: AGENT, ok: true, error_code: "", ms: 12, memory_n: 0,
    kb_n: 0, conversation_n: 0, steps: [], client: "", has_contents: true, ...extra };
}

let root: Root;
let urls: string[];

function serve(routes: Record<string, unknown>) {
  urls = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string) => {
      urls.push(url);
      if (url in routes) return Response.json({ data: routes[url], error: null });
      if (url === "/api/v1/datapacks") return Response.json({ data: [], error: null });
      return Response.json({ data: null, error: { code: "NOT_FOUND", message: "not found" } }, { status: 404 });
    }),
  );
}

async function open(path: string) {
  history.replaceState(null, "", path);
  const el = document.createElement("div");
  document.body.append(el);
  root = createRoot(el);
  await act(async () => root.render(<App auth={auth} />));
}

const main = () => document.querySelector("main")?.textContent ?? "";
const button = (text: string) => [...document.querySelectorAll("button")].find((b) => b.textContent === text);

afterEach(() => {
  act(() => root.unmount());
  document.body.innerHTML = "";
  history.replaceState(null, "", "/");
  vi.unstubAllGlobals();
});

test("an agent in the address opens its calls, and Load older pages back from the last one", async () => {
  const q = `agent=${encodeURIComponent(AGENT)}`;
  serve({
    [`/api/v1/calls?${q}`]: { calls: [call(9, "context_search", { client: "kode" }), call(7, "memory_add")], more: true },
    [`/api/v1/calls?${q}&before=7`]: { calls: [call(3, "conversation_create")], more: false },
  });
  await open(`/calls?${q}`);
  expect(document.querySelector<HTMLInputElement>("#calls-agent")?.value).toBe(AGENT);
  expect(main()).toContain("context_search");
  expect(main()).toContain("Kode");
  await act(async () => button("Load older")!.click());
  expect(urls).toContain(`/api/v1/calls?${q}&before=7`);
  expect(main()).toContain("conversation_create");
  expect(button("Load older")).toBeUndefined();
});

test("a call opens to what it sent and got back, or says that wasn't kept", async () => {
  serve({
    "/api/v1/calls": { calls: [call(2, "context_search"), call(1, "memory_add", { has_contents: false })], more: false },
    "/api/v1/calls/2": { ...call(2, "context_search"), args: '{"query":"why"}', reply: '{"results":[]}', cut: false },
    "/api/v1/calls/1": call(1, "memory_add", { has_contents: false }),
  });
  await open("/calls");
  const row = (tool: string) => [...document.querySelectorAll("tr")].find((r) => r.textContent?.includes(tool))!;
  await act(async () => row("context_search").click());
  const dialog = () => document.querySelector('[role="dialog"]')?.textContent ?? "";
  expect(dialog()).toContain("Sent");
  expect(dialog()).toContain('"query": "why"');
  expect(dialog()).toContain("Got back");
  await act(async () => button("Close")!.click());
  await act(async () => row("memory_add").click());
  expect(dialog()).toContain("isn't kept");
});

test("the failed-only box filters and the address keeps it", async () => {
  serve({ "/api/v1/calls": { calls: [], more: false }, "/api/v1/calls?failed=true": { calls: [], more: false } });
  await open("/calls");
  expect(main()).toContain("No calls match");
  await act(async () => document.querySelector<HTMLInputElement>("#calls-failed")!.click());
  expect(urls).toContain("/api/v1/calls?failed=true");
  expect(window.location.search).toBe("?failed=true");
});

test("an older page that comes back after the filter changed isn't added to the new list", async () => {
  let release: (r: Response) => void = () => {};
  urls = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string) => {
      urls.push(url);
      if (url === "/api/v1/calls") return Response.json({ data: { calls: [call(5, "memory_add")], more: true }, error: null });
      if (url === "/api/v1/calls?before=5") return new Promise<Response>((r) => (release = r));
      if (url === "/api/v1/calls?failed=true") return Response.json({ data: { calls: [call(4, "memory_search")], more: false }, error: null });
      if (url === "/api/v1/datapacks") return Response.json({ data: [], error: null });
      return Response.json({ data: null, error: { code: "NOT_FOUND", message: "not found" } }, { status: 404 });
    }),
  );
  await open("/calls");
  await act(async () => button("Load older")!.click());
  await act(async () => document.querySelector<HTMLInputElement>("#calls-failed")!.click());
  await act(async () => release(Response.json({ data: { calls: [call(1, "conversation_create")], more: false }, error: null })));
  expect(main()).toContain("memory_search");
  expect(main()).not.toContain("conversation_create");
});
