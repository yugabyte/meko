// @vitest-environment happy-dom
// A new datapack's Get started card: connecting an agent opens the Agents page, since agents
// connect to Meko, not to a datapack, and the other steps open the datapack's own tabs.

import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, expect, test } from "vitest";
import { GetStarted } from "../src/components/GetStarted";
import type { Datapack, Metrics } from "../src/lib/types";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const ID = "11111111-1111-4111-8111-111111111111";
const datapack: Datapack = { datapack_id: ID, datapack_name: "alpha", description: "", grant: "owner", created_at: "",
  updated_at: "", web_url: `/datapacks/${ID}` };
// Nothing done yet.
const metrics = { agent_count: 0, kb_file_count: 0, total_requests: 0 } as Metrics;

let root: Root;

async function render() {
  const el = document.createElement("div");
  document.body.append(el);
  root = createRoot(el);
  await act(async () => root.render(<GetStarted datapack={datapack} metrics={metrics} />));
}

async function click(label: string) {
  const b = [...document.querySelectorAll("button")].find((x) => x.textContent === label);
  if (!b) throw new Error(`no ${label} step`);
  await act(async () => b.click());
}

afterEach(() => {
  act(() => root.unmount());
  document.body.innerHTML = "";
  history.replaceState(null, "", "/");
});

test("Connect an agent opens the Agents page", async () => {
  await render();
  await click("Connect an agent");
  expect(window.location.pathname).toBe("/agents");
});

test("the other steps open the datapack's own tabs", async () => {
  await render();
  await click("Add a memory");
  expect(window.location.pathname).toBe(`/datapacks/${ID}/memories`);
  await click("Upload a file");
  expect(window.location.pathname).toBe(`/datapacks/${ID}/knowledge`);
  await click("Run a search");
  expect(window.location.pathname).toBe(`/datapacks/${ID}/search`);
});
