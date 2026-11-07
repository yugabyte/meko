// @vitest-environment happy-dom
// The gate in a browser: there's no login, so a page load shows the app; the old sign-in and account
// addresses lead into it; and a refused request shows its note in place. Against a faked /api/v1.

import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, expect, test, vi } from "vitest";
import { Gate } from "../src/Gate";
import type { AuthStatus } from "../src/lib/types";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const remote: AuthStatus = { access: "remote", local_access: false, signed_in: true };
const local: AuthStatus = { access: "local", local_access: true, signed_in: true };

let root: Root;

// Meko's replies: the status given, no datapacks, and refuse (when set) for every other route.
function serve(status: AuthStatus, refuse?: number) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string) => {
      if (url === "/api/v1/auth/status") return Response.json({ data: status, error: null });
      if (refuse) {
        const code = refuse === 401 ? "UNAUTHORIZED" : "FORBIDDEN";
        return Response.json({ data: null, error: { code, message: "refused" } }, { status: refuse });
      }
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
  await act(async () => root.render(<Gate />));
}

const here = () => window.location.pathname + window.location.search;

afterEach(() => {
  act(() => root.unmount());
  document.body.innerHTML = "";
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

test("remote access opens the app, with no sign-in", async () => {
  serve(remote);
  await open("/status");
  expect(document.body.textContent).not.toContain("Sign in");
  expect(here()).toBe("/status");
});

test("an old sign-in link goes on to where it was headed", async () => {
  serve(remote);
  await open("/signin?next=%2Fdatapacks");
  expect(here()).toBe("/datapacks");
});

test("an old sign-in link to an extension's page loads that page, not the app", async () => {
  serve(remote);
  const replace = vi.spyOn(window.location, "replace").mockImplementation(() => {});
  await open("/signin?next=%2Fext%2Fpresent%2Fdeck%2F1");
  expect(replace).toHaveBeenCalledWith("/ext/present/deck/1");
});

test("an old sign-in link can't lead to another site", async () => {
  serve(remote);
  await open("/signin?next=%2F%2Fevil.example.com%2F");
  expect(here()).toBe("/");
});

test("an old setup link goes to the start, and the old account page to Settings", async () => {
  serve(remote);
  await open("/setup?token=t0k3n");
  expect(here()).toBe("/");
  act(() => root.unmount());
  await open("/account");
  expect(here()).toBe("/settings");
});

test("local access that Meko refuses says the request wasn't local", async () => {
  serve(local, 401);
  await open("/");
  expect(document.body.textContent).toContain("This request wasn't recognized as local");
});
