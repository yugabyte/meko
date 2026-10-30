// @vitest-environment happy-dom
// The Settings page's switches for the features that run the user's Claude Code, against a faked
// /api/v1: turning one on sends only the harness, and a failed test call leaves it off with why.

import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, expect, test, vi } from "vitest";
import type { AuthStatus } from "../src/lib/types";
import { SettingsPage } from "../src/pages/SettingsPage";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const auth = { access: "local", local_access: true, signed_in: false, username: null } as unknown as AuthStatus;

let root: Root;
let sent: { method: string; url: string; body?: string }[];

type Reply = { status?: number; data?: unknown; error?: { code: string; message: string } };

function serve(routes: Record<string, (body?: string) => Reply>) {
  sent = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init?: RequestInit) => {
      const method = init?.method ?? "GET";
      const body = init?.body as string | undefined;
      sent.push({ method, url, body });
      const r = routes[`${method} ${url}`]?.(body) ?? { status: 404, error: { code: "NOT_FOUND", message: "no" } };
      return Response.json({ data: r.data ?? null, error: r.error ?? null }, { status: r.status ?? 200 });
    }),
  );
}

function toggle(label: string): HTMLButtonElement {
  const b = document.querySelector<HTMLButtonElement>(`button[role="switch"][aria-label="${label}"]`);
  if (!b) throw new Error(`no ${label} switch in: ${document.body.textContent}`);
  return b;
}

async function render() {
  const el = document.createElement("div");
  document.body.append(el);
  root = createRoot(el);
  await act(async () => root.render(<SettingsPage auth={auth} />));
}

afterEach(() => {
  act(() => root.unmount());
  document.body.innerHTML = "";
  vi.unstubAllGlobals();
});

const extractorOff = { enabled: false, harness: "", model: "", interval_seconds: 300, queued: 0, extracted: 0 };

test("turning Search answers on sends the harness alone, and shows it on", async () => {
  let answersOn = false;
  serve({
    "GET /api/v1/settings/memory-extractor": () => ({ data: extractorOff }),
    "GET /api/v1/settings/answers": () => ({
      data: { enabled: answersOn, harness: answersOn ? "claude" : "", model: answersOn ? "haiku" : "" },
    }),
    "PUT /api/v1/settings/answers": () => ((answersOn = true), { data: { enabled: true } }),
    "GET /api/v1/settings/usage-stats": () => ({ data: { enabled: true, sends: true, forced_off: false } }),
  });
  await render();
  expect(toggle("Search answers").getAttribute("aria-checked")).toBe("false");
  await act(async () => toggle("Search answers").click());
  expect(sent.find((c) => c.method === "PUT")).toMatchObject({
    url: "/api/v1/settings/answers",
    body: JSON.stringify({ harness: "claude" }),
  });
  expect(toggle("Search answers").getAttribute("aria-checked")).toBe("true");
  expect(document.body.textContent).toContain("claude (haiku)");
});

test("a failed test call leaves memory extraction off and says why", async () => {
  serve({
    "GET /api/v1/settings/memory-extractor": () => ({ data: extractorOff }),
    "GET /api/v1/settings/answers": () => ({ data: { enabled: false, harness: "", model: "" } }),
    "PUT /api/v1/settings/memory-extractor": () => ({
      status: 400,
      error: { code: "VALIDATION_ERROR", message: "a test call failed (is claude installed and signed in?)" },
    }),
    "GET /api/v1/settings/usage-stats": () => ({ data: { enabled: false, sends: true, forced_off: false } }),
  });
  await render();
  await act(async () => toggle("Memory extraction").click());
  expect(toggle("Memory extraction").getAttribute("aria-checked")).toBe("false");
  expect(document.body.textContent).toContain("is claude installed and signed in?");
});
