// @vitest-environment happy-dom
// Settings › Meko Cloud's sign-in: the button starts it and sends the browser to Meko Cloud, and the
// page Meko Cloud sends the browser back to (/settings/meko-cloud/callback) finishes it, against a
// faked /api/v1.

import { act, StrictMode } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, expect, test, vi } from "vitest";
import type { MekoCloud } from "../src/lib/types";
import { MekoCloudPage } from "../src/pages/MekoCloudPage";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

let root: Root;
let calls: { method: string; url: string; body?: string }[];

type Reply = { status?: number; data?: unknown; error?: { code: string; message: string } };

// Meko's replies, by "METHOD /path"; anything else is a 404.
function serve(routes: Record<string, () => Reply>) {
  calls = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init?: RequestInit) => {
      const method = init?.method ?? "GET";
      calls.push({ method, url, body: init?.body as string | undefined });
      const r = routes[`${method} ${url}`]?.() ?? { status: 404, error: { code: "NOT_FOUND", message: "not found" } };
      return Response.json({ data: r.data ?? null, error: r.error ?? null }, { status: r.status ?? 200 });
    }),
  );
}

function cloud(linked: boolean): MekoCloud {
  return {
    set: linked,
    linked,
    url: "",
    web_url: "",
    api_url: "",
    effective_url: "https://mcp.example.com/mcp",
    effective_web_url: "https://cloud.example.com",
    effective_api_url: "https://cloud.example.com/api/v1",
    key_hint: linked ? "abcd" : null,
    default_url: "https://mcp.example.com/mcp",
    default_web_url: "https://cloud.example.com",
    agents: { moved: [{ name: "claude", title: "Claude Code" }], left: [], failed: [] },
  };
}

async function render(strict = false) {
  const el = document.createElement("div");
  document.body.append(el);
  root = createRoot(el);
  await act(async () =>
    root.render(
      strict ? (
        <StrictMode>
          <MekoCloudPage />
        </StrictMode>
      ) : (
        <MekoCloudPage />
      ),
    ),
  );
}

function text(): string {
  return document.body.textContent ?? "";
}

afterEach(() => {
  act(() => root.unmount());
  document.body.innerHTML = "";
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  history.replaceState(null, "", "/");
});

test("Sign in with Meko Cloud sends the browser to Meko Cloud's sign-in", async () => {
  serve({
    "GET /api/v1/settings/meko-cloud": () => ({ data: cloud(false) }),
    "POST /api/v1/settings/meko-cloud/sign-in": () => ({
      data: { authorize_url: "https://mcp.example.com/authorize?state=s" },
    }),
  });
  const assign = vi.fn();
  vi.spyOn(window, "location", "get").mockReturnValue({ ...window.location, assign } as Location);
  await render();
  expect(text()).toContain("Meko keeps an API key for this computer, never your password.");
  expect(document.getElementById("meko-cloud-key")).toBeNull();
  const signIn = [...document.querySelectorAll("button")].find((b) => b.textContent === "Sign in with Meko Cloud")!;
  await act(async () => signIn.click());
  expect(calls.find((c) => c.method === "POST")).toMatchObject({ url: "/api/v1/settings/meko-cloud/sign-in", body: "{}" });
  expect(assign).toHaveBeenCalledWith("https://mcp.example.com/authorize?state=s");
});

test("the callback finishes the sign-in, once, and takes the code out of the address", async () => {
  serve({
    "POST /api/v1/settings/meko-cloud/sign-in/finish": () => ({ data: cloud(true) }),
  });
  history.replaceState(null, "", "/settings/meko-cloud/callback?code=the-code&state=the-state");
  await render();
  expect(window.location.pathname + window.location.search).toBe("/settings/meko-cloud");
  const finishes = calls.filter((c) => c.url === "/api/v1/settings/meko-cloud/sign-in/finish");
  expect(finishes).toHaveLength(1);
  expect(JSON.parse(finishes[0].body!)).toEqual({ code: "the-code", state: "the-state" });
  expect(text()).toContain("Signed in and linked.");
  expect(text()).toContain("Moved Claude Code back to this Meko");
  expect(text()).toContain("Linked");
});

test("a sign-in Meko Cloud refused says why, and finishes nothing", async () => {
  serve({ "GET /api/v1/settings/meko-cloud": () => ({ data: cloud(false) }) });
  history.replaceState(
    null,
    "",
    "/settings/meko-cloud/callback?error=access_denied&error_description=User+denied+access.",
  );
  await render();
  expect(text()).toContain("Meko Cloud didn't sign you in: User denied access. Nothing was saved.");
  expect(calls.some((c) => c.url.endsWith("/finish"))).toBe(false);
  expect(text()).toContain("Not linked");
});

test("in StrictMode, the callback's second run finishes the same sign-in and reads no older link", async () => {
  serve({
    "GET /api/v1/settings/meko-cloud": () => ({ data: cloud(false) }),
    "POST /api/v1/settings/meko-cloud/sign-in/finish": () => ({ data: cloud(true) }),
  });
  history.replaceState(null, "", "/settings/meko-cloud/callback?code=the-code&state=another-state");
  await render(true);
  expect(calls.filter((c) => c.url.endsWith("/finish"))).toHaveLength(1);
  expect(calls.some((c) => c.method === "GET")).toBe(false);
  expect(text()).toContain("Signed in and linked.");
  expect(text()).not.toContain("didn't sign you in");
});
