// @vitest-environment happy-dom
// The Sync to Meko Cloud dialog against a faked /api/v1: a datapack that doesn't fit the plan can't
// be synced and says which limit it's over; one that fits is synced, then offers Meko Cloud's link.

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

function serve(pc: SyncPrecheck) {
  sent = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init?: RequestInit) => {
      const key = `${init?.method ?? "GET"} ${url}`;
      sent.push(key);
      if (key === `POST /api/v1/datapacks/${ID}/cloud-sync/precheck`) return Response.json({ data: pc, error: null });
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
  await act(async () => button("Sync to Meko Cloud")!.click());
  expect(sent).toContain(`POST /api/v1/datapacks/${ID}/cloud-sync`);
  expect(document.body.textContent).toContain("invoices is in Meko Cloud");
  expect(document.querySelector<HTMLAnchorElement>('a[href="https://cloud.example/datapacks/x"]')).not.toBeNull();
  await act(async () => button("Done")!.click());
  expect(onSynced).toHaveBeenCalled();
});
