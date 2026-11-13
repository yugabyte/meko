// @vitest-environment happy-dom
// The Sync to Meko Cloud dialog against a faked /api/v1: a datapack that doesn't fit the plan can't
// be synced and says which limit it's over; one that fits syncs only once the box is ticked and its
// name typed, then offers Meko Cloud's link; with no Meko Cloud link, it asks for the API key instead.

import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, expect, test, vi } from "vitest";
import { CloudSyncDialog } from "../src/components/CloudSyncDialog";
import type { Datapack, SyncPrecheck } from "../src/lib/types";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const ID = "11111111-1111-4111-8111-111111111111";
const DP: Datapack = { datapack_id: ID, datapack_name: "invoices", description: "", grant: "owner", created_at: "",
  updated_at: "" };

let root: Root | undefined;
let sent: string[];

function serve(pc: SyncPrecheck | { code: string; message: string }) {
  sent = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init?: RequestInit) => {
      const key = `${init?.method ?? "GET"} ${url}`;
      sent.push(key);
      if (key === `POST /api/v1/datapacks/${ID}/cloud-sync/precheck`) {
        if ("code" in pc) return Response.json({ data: null, error: pc }, { status: 409 });
        return Response.json({ data: pc, error: null });
      }
      if (key === `POST /api/v1/datapacks/${ID}/cloud-sync`) {
        return Response.json({ data: { datapack_id: ID, datapack_name: "invoices", state: "done", step: "",
          web_url: "https://cloud.example/datapacks/x" }, error: null }, { status: 202 });
      }
      return Response.json({ data: null, error: { code: "NOT_FOUND", message: "no" } }, { status: 404 });
    }),
  );
}

async function render(onSynced = () => {}) {
  const el = document.createElement("div");
  document.body.append(el);
  const r = createRoot(el);
  root = r;
  await act(async () => r.render(<CloudSyncDialog datapack={DP} onClose={() => {}} onSynced={onSynced} />));
}

const button = (text: string) => [...document.querySelectorAll("button")].find((b) => b.textContent?.includes(text));

// Ticks "I understand" and types name, as a person confirms the sync.
async function confirm({ tick = true, name = "invoices" } = {}) {
  const box = document.querySelector<HTMLInputElement>('input[type="checkbox"]')!;
  if (tick && !box.checked) await act(async () => box.click());
  const input = document.querySelector<HTMLInputElement>('input[aria-label="Type the name to confirm"]')!;
  const setValue = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!.set!;
  await act(async () => {
    setValue.call(input, name);
    input.dispatchEvent(new Event("input", { bubbles: true }));
  });
}

afterEach(() => {
  if (root) act(() => root!.unmount());
  root = undefined;
  document.body.innerHTML = "";
  vi.unstubAllGlobals();
});

test("a datapack over a limit can't be synced, and says which", async () => {
  serve({ ok: false, plan: "free", checks: [
    { check: "datapacks", used: 10, adding: 1, limit: 10, ok: false, message: "Your Free plan allows 10 datapacks." },
    { check: "memory_adds", used: 0, adding: 40, limit: 10000, ok: true },
  ] });
  await render();
  expect(document.body.textContent).toContain("Your Free plan allows 10 datapacks.");
  expect(document.body.textContent).toContain("Upgrade your plan");
  expect(button("Sync to Meko Cloud")?.disabled).toBe(true);
});

test("one that fits is synced, then offers Meko Cloud's link", async () => {
  serve({ ok: true, plan: "pro", checks: [{ check: "datapacks", used: 3, adding: 1, limit: -1, ok: true }] });
  const onSynced = vi.fn();
  await render(onSynced);
  expect(document.body.textContent).toContain("no limit");
  await confirm();
  await act(async () => button("Sync to Meko Cloud")!.click());
  expect(sent).toContain(`POST /api/v1/datapacks/${ID}/cloud-sync`);
  expect(document.body.textContent).toContain("invoices is in Meko Cloud");
  expect(document.querySelector<HTMLAnchorElement>('a[href="https://cloud.example/datapacks/x"]')).not.toBeNull();
  await act(async () => button("Done")!.click());
  expect(onSynced).toHaveBeenCalled();
});

test("it syncs only once the box is ticked and the datapack's exact name typed", async () => {
  serve({ ok: true, plan: "pro", checks: [{ check: "datapacks", used: 3, adding: 1, limit: -1, ok: true }] });
  await render();
  expect(document.body.textContent).toContain("is deleted from this computer");
  const sync = () => button("Sync to Meko Cloud")!;
  expect(sync().disabled).toBe(true);
  await confirm({ tick: true, name: "" });
  expect(sync().disabled).toBe(true); // ticked, nothing typed
  await confirm({ tick: true, name: "Invoices" });
  expect(sync().disabled).toBe(true); // a name that isn't exactly its own
  await act(async () => document.querySelector<HTMLInputElement>('input[type="checkbox"]')!.click());
  await confirm({ tick: false, name: "invoices" });
  expect(sync().disabled).toBe(true); // typed, box unticked
  await confirm();
  expect(sync().disabled).toBe(false);
  expect(sent).not.toContain(`POST /api/v1/datapacks/${ID}/cloud-sync`);
});

test("with no Meko Cloud link, it asks for the API key and syncs nothing", async () => {
  serve({ code: "CLOUD_NOT_LINKED", message: "Meko Cloud isn't linked" });
  await render();
  expect(document.body.textContent).toContain("link Meko Cloud with your API key");
  expect(button("Add API key")).toBeTruthy();
  expect(button("Sync to Meko Cloud")).toBeUndefined();
  await act(async () => button("Add API key")!.click());
  expect(window.location.pathname).toBe("/settings/meko-cloud");
  expect(sent).not.toContain(`POST /api/v1/datapacks/${ID}/cloud-sync`);
});

test("a sync under way shows its steps, the upload's progress, and can be closed", async () => {
  serve({ ok: true, plan: "pro", checks: [{ check: "datapacks", used: 3, adding: 1, limit: -1, ok: true }] });
  const now = new Date().toISOString();
  const running = { datapack_id: ID, datapack_name: "invoices", state: "syncing", step: "uploading",
    started_at: now, upload: { done: 1048576, total: 4194304 }, phases: [
      { name: "exporting", started_at: now, ended_at: now }, { name: "uploading", started_at: now }] };
  const inner = fetch as unknown as (url: string, init?: RequestInit) => Promise<Response>;
  vi.stubGlobal("fetch", vi.fn(async (url: string, init?: RequestInit) =>
    init?.method === "POST" && url.endsWith("/cloud-sync")
      ? Response.json({ data: running, error: null }, { status: 202 })
      : inner(url, init)));
  const onClose = vi.fn();
  const el = document.createElement("div");
  document.body.append(el);
  root = createRoot(el);
  await act(async () => root!.render(<CloudSyncDialog datapack={DP} onClose={onClose} onSynced={() => {}} />));
  await confirm();
  await act(async () => button("Sync to Meko Cloud")!.click());
  const steps = document.querySelector('ol[aria-label="Sync steps"]')!;
  expect(steps.querySelector('[data-status="done"]')?.textContent).toContain("Pack it up");
  expect(steps.querySelector('[aria-current="step"]')?.textContent).toContain("Upload it to Meko Cloud");
  expect(steps.textContent).toContain("1.0 MB of 4.0 MB");
  expect(document.body.textContent).toContain("Step 2 of 9");
  await act(async () => button("Close")!.click());
  expect(onClose).toHaveBeenCalled();
});
