// @vitest-environment happy-dom
// The app shell against a faked /api/v1: which datapack it opens from the one list of both sides,
// and that a datapack in the address opens whatever the listing (which waits for Meko Cloud while
// it's linked) is doing.

import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, expect, test, vi } from "vitest";
import App from "../src/App";
import { notifyCloudChanged } from "../src/lib/cloud";
import type { AuthStatus, Datapack } from "../src/lib/types";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const auth: AuthStatus = { access: "local", local_access: true, signed_in: true };
const HERE = "11111111-1111-4111-8111-111111111111";
const CLOUD = "22222222-2222-4222-8222-222222222222";

function row(id: string, name: string, webURL: string | null): Datapack {
  return { datapack_id: id, datapack_name: name, description: "", grant: "owner", created_at: "", updated_at: "",
    web_url: webURL };
}

// Meko Cloud's datapack sorts first by name.
const BOTH = [row(CLOUD, "alpha", `https://cloud.example.com/datapacks/${CLOUD}`), row(HERE, "beta", `/datapacks/${HERE}`)];

let root: Root;
let urls: string[];

// Meko's replies: list answers GET /datapacks (a Response, or a promise that never settles), this
// Meko's datapack answers GET /datapacks/<HERE>, and anything else is a 404.
function serve(list: () => Response | Promise<Response>) {
  urls = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string) => {
      urls.push(url);
      if (url === "/api/v1/datapacks") return list();
      if (url === `/api/v1/datapacks/${HERE}`) return Response.json({ data: row(HERE, "beta", null), error: null });
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

afterEach(() => {
  act(() => root.unmount());
  document.body.innerHTML = "";
  history.replaceState(null, "", "/");
  vi.unstubAllGlobals();
});

test("with no datapack in the address, the first that opens here is shown, never a Meko Cloud one", async () => {
  serve(() => Response.json({ data: BOTH, error: null }));
  await open("/");
  expect(urls).toContain(`/api/v1/datapacks/${HERE}`);
  expect(urls).not.toContain(`/api/v1/datapacks/${CLOUD}`);
});

test("when every datapack opens in Meko Cloud, the page says so rather than that there are none", async () => {
  serve(() => Response.json({ data: [BOTH[0]], error: null }));
  await open("/");
  expect(main()).toContain("Your datapacks are in Meko Cloud");
  expect(main()).not.toContain("No datapacks");
  expect(urls).not.toContain(`/api/v1/datapacks/${CLOUD}`);
});

test("a datapack in the address opens when the listing failed", async () => {
  serve(() =>
    Response.json({ data: null, error: { code: "CLOUD_UNREACHABLE", message: "Meko Cloud can't be reached." } },
      { status: 502 }),
  );
  await open(`/datapacks/${HERE}/overview`);
  expect(urls).toContain(`/api/v1/datapacks/${HERE}`);
  expect(document.querySelector("main h1, main h2")?.textContent).toContain("beta");
});

test("a datapack in the address doesn't wait for a listing that hangs", async () => {
  serve(() => new Promise<Response>(() => {}));
  await open(`/datapacks/${HERE}/overview`);
  expect(urls).toContain(`/api/v1/datapacks/${HERE}`);
  expect(document.querySelector("main h1, main h2")?.textContent).toContain("beta");
});

test("linking or unlinking Meko Cloud loads the list again", async () => {
  serve(() => Response.json({ data: BOTH, error: null }));
  await open("/");
  const before = urls.filter((u) => u === "/api/v1/datapacks").length;
  await act(async () => notifyCloudChanged());
  expect(urls.filter((u) => u === "/api/v1/datapacks").length).toBe(before + 1);
});
