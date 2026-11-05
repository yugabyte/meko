// @vitest-environment happy-dom
// The sidebar's datapacks against a faked /api/v1: one list, each row opened where its web_url says
// (this UI's pages, Meko Cloud's web app in a new tab, or nowhere), with no location in any request.

import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, expect, test, vi } from "vitest";
import { Sidebar } from "../src/components/Sidebar";
import { ApiError } from "../src/lib/api";
import type { Datapack } from "../src/lib/types";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const HERE = "11111111-1111-4111-8111-111111111111";
const CLOUD = "22222222-2222-4222-8222-222222222222";
const UNKNOWN = "33333333-3333-4333-8333-333333333333";

function row(id: string, name: string, webURL: string | null, grant = "owner"): Datapack {
  return { datapack_id: id, datapack_name: name, description: "", grant, created_at: "", updated_at: "",
    web_url: webURL };
}

const ROWS = [
  row(HERE, "alpha", `/datapacks/${HERE}`),
  row(CLOUD, "beta", `https://cloud.example.com/datapacks/${CLOUD}`, "viewer"),
  row(UNKNOWN, "gamma", null),
];

let root: Root;
let urls: string[];

// Meko's replies: the link's state, a create's (made where createdURL says), and a 404 for anything
// else the sidebar asks.
function serve(linked: boolean, createdURL: string | null = null) {
  urls = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init?: RequestInit) => {
      urls.push(url);
      if (url === "/api/v1/settings/meko-cloud") return Response.json({ data: { linked }, error: null });
      if (url === "/api/v1/datapacks" && init?.method === "POST") {
        return Response.json({ data: { datapack_id: CLOUD, name: "research", web_url: createdURL }, error: null },
          { status: 201 });
      }
      return Response.json({ data: null, error: { code: "NOT_FOUND", message: "not found" } }, { status: 404 });
    }),
  );
}

async function render(
  props: {
    datapacks?: Datapack[];
    error?: Error;
    linked?: boolean;
    createdURL?: string | null;
    onCreated?: (id: string, open: boolean) => void;
  } = {},
) {
  serve(props.linked ?? true, props.createdURL ?? null);
  const el = document.createElement("div");
  document.body.append(el);
  root = createRoot(el);
  await act(async () =>
    root.render(
      <Sidebar
        open
        onClose={() => {}}
        datapacks={props.datapacks ?? ROWS}
        loading={false}
        error={props.error}
        onRefresh={() => {}}
        view={undefined}
        theme="light"
        onToggleTheme={() => {}}
        onCreated={props.onCreated ?? (() => {})}
      />,
    ),
  );
}

function link(name: string): HTMLElement {
  const el = [...document.querySelectorAll("nav a, nav div")].find((x) => x.textContent?.startsWith(name));
  if (!el) throw new Error(`no row ${name}`);
  return el as HTMLElement;
}

afterEach(() => {
  act(() => root.unmount());
  document.body.innerHTML = "";
  history.replaceState(null, "", "/");
  vi.unstubAllGlobals();
});

test("a row served here opens this UI's page", async () => {
  await render();
  const a = link("alpha") as HTMLAnchorElement;
  expect(a.tagName).toBe("A");
  expect(a.getAttribute("href")).toBe(`/datapacks/${HERE}/overview`);
  expect(a.target).toBe("");
  await act(async () => a.click());
  expect(location.pathname).toBe(`/datapacks/${HERE}/overview`);
});

test("a row with a full URL opens it in a new tab, and says so", async () => {
  await render();
  const a = link("beta") as HTMLAnchorElement;
  expect(a.tagName).toBe("A");
  expect(a.getAttribute("href")).toBe(`https://cloud.example.com/datapacks/${CLOUD}`);
  expect(a.target).toBe("_blank");
  expect(a.rel).toBe("noopener noreferrer");
  expect(a.textContent).toContain("opens in a new tab");
  expect(a.textContent).toContain("shared with you");
});

test("a row with no web_url opens nothing", async () => {
  await render();
  const d = link("gamma");
  expect(d.tagName).toBe("DIV");
  expect(d.getAttribute("aria-disabled")).toBe("true");
  expect(d.title).toContain("Settings › Meko Cloud");
});

test("a row from an older server, with no web_url at all, opens here", async () => {
  const old = { ...row(HERE, "alpha", null) };
  delete old.web_url;
  await render({ datapacks: [old] });
  expect(link("alpha").getAttribute("href")).toBe(`/datapacks/${HERE}/overview`);
});

test("only a Meko Cloud row says where it lives, with its cloud icon, and no request names a location", async () => {
  await render();
  const nav = document.querySelector("nav")!.textContent ?? "";
  expect(nav).not.toContain("this computer");
  const rows = [...document.querySelectorAll("nav a, nav [aria-disabled]")];
  expect(rows.filter((r) => r.textContent?.includes("in Meko Cloud")).map((r) => r.textContent)).toEqual([
    "beta, shared with you, in Meko Cloud: opens in a new tab",
  ]);
  expect(rows.find((r) => r.textContent?.startsWith("beta"))?.getAttribute("title")).toBe("Opens in Meko Cloud");
  expect(urls).toContain("/api/v1/settings/meko-cloud");
  expect(urls.filter((u) => u.includes("location"))).toEqual([]);
});

test("+ says where a new datapack is made: Meko Cloud while linked", async () => {
  await render({ linked: true });
  expect(document.querySelector('button[title="New datapack in Meko Cloud"]')).not.toBeNull();
  act(() => root.unmount());
  document.body.innerHTML = "";
  await render({ linked: false });
  expect(document.querySelector('button[title="New datapack"]')).not.toBeNull();
  expect(document.querySelector('button[title="Refresh the datapacks"]')).toBeNull();
});

test("a listing that failed is one note, linking to Settings when the fix is there", async () => {
  await render({ datapacks: [], error: new ApiError("Meko Cloud refused the key.", 502, "CLOUD_KEY_REFUSED") });
  const nav = document.querySelector("nav")!;
  expect(nav.textContent).toContain("Meko Cloud refused the key.");
  expect(nav.querySelector('a[href="/settings/meko-cloud"]')?.textContent).toBe("Open Meko Cloud settings");
});

// The server, not the UI's view of the link, says where a new datapack was made.
async function createOne(props: { linked: boolean; createdURL: string | null }): Promise<boolean[]> {
  const opened: boolean[] = [];
  await render({ ...props, onCreated: (_id, open) => opened.push(open) });
  const plus = document.querySelector<HTMLButtonElement>('button[title^="New datapack"]')!;
  await act(async () => plus.click());
  // The dialog's name: the first input that isn't the sidebar's Find a datapack box.
  const input = [...document.querySelectorAll<HTMLInputElement>("input")].find(
    (i) => i.placeholder !== "Find a datapack…",
  )!;
  await act(async () => {
    Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!.set!.call(input, "research");
    input.dispatchEvent(new Event("input", { bubbles: true }));
  });
  const button = [...document.querySelectorAll("button")].find((b) => b.textContent?.trim() === "Create")!;
  await act(async () => button.click());
  return opened;
}

test("a datapack the server made in Meko Cloud doesn't open here, even when the UI thought it unlinked", async () => {
  expect(await createOne({ linked: false, createdURL: `https://cloud.example.com/datapacks/${CLOUD}` })).toEqual([
    false,
  ]);
});

test("a datapack the server made here opens here", async () => {
  expect(await createOne({ linked: true, createdURL: `/datapacks/${CLOUD}` })).toEqual([true]);
});

test("Settings is a gear at the foot of the sidebar, not a row in the top links", async () => {
  await render();
  const gear = document.querySelector<HTMLAnchorElement>('a[aria-label="Settings"]')!;
  expect(gear.getAttribute("href")).toBe("/settings");
  expect(gear.closest(".border-t")).not.toBeNull();
  expect(gear.textContent).toBe("");
  expect(document.querySelectorAll('a[href="/settings"]')).toHaveLength(1);
});
