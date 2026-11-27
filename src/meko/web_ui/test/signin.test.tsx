// @vitest-environment happy-dom
// Signing in to remote access through Meko Cloud: the gate shows the sign-in page to a browser that
// isn't signed in, which goes to Meko Cloud and back to /signin/callback; the callback finishes
// once and loads the page asked for, or says why it couldn't. Against a faked /api/v1.

import { act, StrictMode } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, expect, test, vi } from "vitest";
import { Gate } from "../src/Gate";
import type { AuthStatus } from "../src/lib/types";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const signedOut: AuthStatus = { access: "remote", signin: "cloud", local_access: false, signed_in: false };
const signedIn: AuthStatus = { ...signedOut, signed_in: true };

let root: Root;
let calls: { url: string; body?: unknown }[] = [];

type Reply = { status?: number; data?: unknown; error?: { code: string; message: string } };

// Meko's replies, by route; any other route gets refuse when set, else an empty list.
function serve(status: AuthStatus, routes: Record<string, Reply> = {}, refuse?: Reply) {
  calls = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init?: RequestInit) => {
      calls.push({ url, body: init?.body ? JSON.parse(String(init.body)) : undefined });
      if (url === "/api/v1/auth/status") return Response.json({ data: status, error: null });
      const r = routes[url] ?? refuse;
      if (r) return Response.json({ data: r.data ?? null, error: r.error ?? null }, { status: r.status ?? 200 });
      return Response.json({ data: [], error: null });
    }),
  );
}

async function open(path: string, strict = false) {
  history.replaceState(null, "", path);
  const el = document.createElement("div");
  document.body.append(el);
  root = createRoot(el);
  await act(async () => root.render(strict ? <StrictMode><Gate /></StrictMode> : <Gate />));
}

function click(label: string) {
  const b = [...document.querySelectorAll("button")].find((x) => x.textContent?.includes(label));
  if (!b) throw new Error(`no button ${label}`);
  b.click();
}

const here = () => window.location.pathname + window.location.search;

afterEach(() => {
  act(() => root.unmount());
  document.body.innerHTML = "";
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

test("a browser that isn't signed in gets the sign-in page, which goes to Meko Cloud", async () => {
  serve(signedOut, { "/api/v1/auth/signin/start": { data: { url: "https://cloud.example/meko-signin?x=1" } } });
  const assign = vi.spyOn(window.location, "assign").mockImplementation(() => {});
  await open("/datapacks/abc/memories?q=1");
  expect(document.body.textContent).toContain("Sign in to Meko");
  expect(here()).toBe("/datapacks/abc/memories?q=1");
  await act(async () => click("Sign in with Meko Cloud"));
  expect(calls.find((c) => c.url === "/api/v1/auth/signin/start")?.body).toEqual({
    next: "/datapacks/abc/memories?q=1",
  });
  expect(assign).toHaveBeenCalledWith("https://cloud.example/meko-signin?x=1");
});

test("the sign-in page comes back to an extension's page without leaving for it first", async () => {
  serve(signedOut, { "/api/v1/auth/signin/start": { data: { url: "https://cloud.example/s" } } });
  const replace = vi.spyOn(window.location, "replace").mockImplementation(() => {});
  vi.spyOn(window.location, "assign").mockImplementation(() => {});
  await open("/signin?next=%2Fext%2Fkode%2F");
  expect(replace).not.toHaveBeenCalled();
  await act(async () => click("Sign in with Meko Cloud"));
  expect(calls.find((c) => c.url === "/api/v1/auth/signin/start")?.body).toEqual({ next: "/ext/kode/" });
});

test("a sign-in Meko can't start says why on the sign-in page", async () => {
  serve(signedOut, {
    "/api/v1/auth/signin/start": {
      status: 409,
      error: { code: "CLOUD_NOT_LINKED", message: "Meko isn't linked to Meko Cloud." },
    },
  });
  await open("/");
  await act(async () => click("Sign in with Meko Cloud"));
  expect(document.body.textContent).toContain("Meko isn't linked to Meko Cloud.");
});

test("a Meko Cloud that can't finish a sign-in says so on the callback page", async () => {
  serve(signedOut, {
    "/api/v1/auth/signin/finish": {
      status: 409,
      error: { code: "CLOUD_SIGNIN_UNSUPPORTED", message: "This Meko Cloud can't sign you in to Meko yet." },
    },
  });
  await open("/signin/callback?code=c4&state=s4");
  expect(document.body.textContent).toContain("This Meko Cloud can't sign you in to Meko yet.");
});

test("once signed in, /signin?next= an extension's page loads it without showing the app first", async () => {
  serve(signedIn);
  const replace = vi.spyOn(window.location, "replace").mockImplementation(() => {});
  await open("/signin?next=%2Fext%2Fkode%2F");
  expect(replace).toHaveBeenCalledWith("/ext/kode/");
  expect(document.body.textContent?.trim()).toBe("Loading…");
});

test("once signed in, /signin goes on to next", async () => {
  serve(signedIn);
  await open("/signin?next=%2Fsettings");
  expect(here()).toBe("/settings");
});

test("the callback finishes once, takes the code out of the address, and loads next", async () => {
  serve(signedOut, { "/api/v1/auth/signin/finish": { data: { next: "/ext/kode/" } } });
  const replace = vi.spyOn(window.location, "replace").mockImplementation(() => {});
  await open("/signin/callback?code=c1&state=s1", true);
  const finishes = calls.filter((c) => c.url === "/api/v1/auth/signin/finish");
  expect(finishes).toEqual([{ url: "/api/v1/auth/signin/finish", body: { code: "c1", state: "s1" } }]);
  expect(here()).toBe("/signin/callback");
  expect(replace).toHaveBeenCalledWith("/ext/kode/");
});

test("a cancelled sign-in says so and posts nothing", async () => {
  serve(signedOut);
  await open("/signin/callback?error=access_denied&state=s2");
  expect(document.body.textContent).toContain("Sign-in cancelled");
  expect(calls.some((c) => c.url === "/api/v1/auth/signin/finish")).toBe(false);
});

test("someone signed in to another account is told so", async () => {
  serve(signedOut, {
    "/api/v1/auth/signin/finish": {
      status: 403,
      error: { code: "NOT_THE_OWNER", message: "Signed in as someone else: sign in with the Meko Cloud account this Meko is linked to." },
    },
  });
  await open("/signin/callback?code=c3&state=s3");
  expect(document.body.textContent).toContain("Signed in as someone else");
  expect(document.body.textContent).toContain("Sign in again");
});

test("a session that ends while the app is open shows the sign-in page", async () => {
  serve(signedIn, {}, { status: 401, error: { code: "SIGNIN_REQUIRED", message: "Sign in first." } });
  await open("/status");
  expect(document.body.textContent).toContain("Sign in to Meko");
});
