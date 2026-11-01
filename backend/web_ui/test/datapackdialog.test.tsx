// @vitest-environment happy-dom
// The New datapack dialog against a faked /api/v1: Meko decides where the datapack is made, and the
// dialog says it's Meko Cloud while that's linked.

import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, expect, test, vi } from "vitest";
import { DatapackDialog } from "../src/components/DatapackDialog";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

let root: Root;
let bodies: unknown[];

function serve() {
  bodies = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (_url: string, init?: RequestInit) => {
      bodies.push(JSON.parse(String(init?.body)));
      return Response.json({ data: { datapack_id: "11111111-1111-4111-8111-111111111111" }, error: null }, { status: 201 });
    }),
  );
}

async function create(inCloud: boolean): Promise<string[]> {
  serve();
  const saved: string[] = [];
  const el = document.createElement("div");
  document.body.append(el);
  root = createRoot(el);
  await act(async () =>
    root.render(<DatapackDialog inCloud={inCloud} onClose={() => {}} onSaved={(id) => saved.push(id)} />),
  );
  const input = document.querySelector("input")!;
  await act(async () => {
    Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!.set!.call(input, "research");
    input.dispatchEvent(new Event("input", { bubbles: true }));
  });
  const button = [...document.querySelectorAll("button")].find((b) => b.textContent?.trim() === "Create")!;
  await act(async () => button.click());
  return saved;
}

afterEach(() => {
  act(() => root.unmount());
  document.body.innerHTML = "";
  vi.unstubAllGlobals();
});

test("unlinked, the create names no location, and the dialog doesn't mention Meko Cloud", async () => {
  expect(await create(false)).toEqual(["11111111-1111-4111-8111-111111111111"]);
  expect(bodies).toEqual([{ name: "research", description: "" }]);
  expect(document.body.textContent).not.toContain("Meko Cloud");
});

test("linked, the create names no location either", async () => {
  const saved = await create(true);
  expect(bodies).toEqual([{ name: "research", description: "" }]);
  expect(saved).toEqual(["11111111-1111-4111-8111-111111111111"]);
});

test("linked, the dialog says the datapack is made in Meko Cloud", async () => {
  serve();
  const el = document.createElement("div");
  document.body.append(el);
  root = createRoot(el);
  await act(async () => root.render(<DatapackDialog inCloud onClose={() => {}} onSaved={() => {}} />));
  expect(document.body.textContent).toContain("New Meko Cloud datapack");
  expect(document.body.textContent).toContain("Meko is linked to Meko Cloud, so the datapack is made there.");
});
