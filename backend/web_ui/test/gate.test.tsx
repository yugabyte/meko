// @vitest-environment happy-dom
// The sign-in gate in a browser with remote access: which screen a page load shows, and the address
// it leaves in the bar, against a faked /api/v1.

import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { Gate } from "../src/Gate";
import type { AuthStatus } from "../src/lib/types";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const signedOut: AuthStatus = {
  access: "remote",
  local_access: false,
  signed_in: false,
  setup_required: false,
  reset: false,
  username: null,
} as AuthStatus;

let root: Root;
let posted: unknown[];

// Meko's replies: the status given, and a setup that takes any token.
function serve(status: AuthStatus) {
  posted = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init?: RequestInit) => {
      if (url === "/api/v1/auth/status") return Response.json({ data: status, error: null });
      if (url === "/api/v1/auth/setup") {
        posted.push(JSON.parse(String(init?.body)));
        return Response.json({ data: { signed_in: true }, error: null });
      }
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

// Types into a controlled input the way a browser does, so React sees the change.
async function type(id: string, value: string) {
  const input = document.getElementById(id) as HTMLInputElement;
  await act(async () => {
    Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!.set!.call(input, value);
    input.dispatchEvent(new Event("input", { bubbles: true }));
  });
}

beforeEach(() => {
  vi.spyOn(window.location, "assign").mockImplementation(() => {});
});

afterEach(() => {
  act(() => root.unmount());
  document.body.innerHTML = "";
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

test("a setup link on a fresh install keeps its token and creates the login", async () => {
  serve({ ...signedOut, setup_required: true });
  await open("/setup?token=t0k3n");

  expect(document.body.textContent).toContain("Create your login");
  expect(window.location.pathname + window.location.search).toBe("/setup?token=t0k3n");

  await type("setup-username", "owner");
  await type("setup-password", "correct horse battery");
  await type("setup-confirm", "correct horse battery");
  await act(async () => document.querySelector("form")!.requestSubmit());

  expect(posted).toEqual([{ token: "t0k3n", username: "owner", password: "correct horse battery" }]);
});

test("a setup link that resets the login keeps its token too", async () => {
  serve({ ...signedOut, setup_required: true, reset: true, username: "owner" });
  await open("/setup?token=r3s3t");

  expect(document.body.textContent).toContain("Reset your login");
  expect(window.location.pathname + window.location.search).toBe("/setup?token=r3s3t");
});

test("a signed-out browser goes to sign-in, coming back to the page after", async () => {
  serve(signedOut);
  await open("/datapacks");

  expect(document.body.textContent).toContain("Sign in to reach your datapacks.");
  expect(window.location.pathname + window.location.search).toBe("/signin?next=%2Fdatapacks");
});
