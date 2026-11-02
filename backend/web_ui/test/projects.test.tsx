// @vitest-environment happy-dom
// Projects against a faked /api/v1: the addresses, the list's cards and its finder, and a project's
// folders, where taking a datapack out unfiles it rather than deleting it.

import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, expect, test, vi } from "vitest";
import { href, parse } from "../src/lib/route";
import type { Project, ProjectDetail } from "../src/lib/types";
import { ProjectPage } from "../src/pages/ProjectPage";
import { ProjectsPage } from "../src/pages/ProjectsPage";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const P = "11111111-1111-4111-8111-111111111111";
const DP = "22222222-2222-4222-8222-222222222222";

function proj(id: string, name: string, folders: string[]): Project {
  return { project_id: id, project_name: name, theme_key: "custom", description: `${name} things`,
    folder_names: folders, folder_count: folders.length, datapack_count: 1, created_at: "2026-10-01T00:00:00Z",
    updated_at: "2026-10-01T00:00:00Z" };
}

const DETAIL: ProjectDetail = {
  project: proj(P, "web", ["design", "tests"]),
  folders: [
    { folder_id: "f1", project_id: P, folder_name: "design", folder_description: "how it looks", datapack_count: 1,
      created_at: "" },
    { folder_id: "f2", project_id: P, folder_name: "tests", folder_description: "", datapack_count: 0, created_at: "" },
  ],
  datapacks: [
    { datapack_id: DP, datapack_name: "figma notes", folder_id: "f1", updated_at: "", placed_at: "", memory_count: 4,
      awaiting_review_count: 1, promoted_count: 2, knowledge_count: 3 },
  ],
};

let root: Root | undefined;
let sent: string[];

function serve(routes: Record<string, unknown>) {
  sent = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init?: RequestInit) => {
      const key = `${init?.method ?? "GET"} ${url}`;
      sent.push(key);
      if (key in routes) return Response.json({ data: routes[key], error: null });
      return Response.json({ data: null, error: { code: "NOT_FOUND", message: "no" } }, { status: 404 });
    }),
  );
}

async function render(node: React.ReactNode) {
  const el = document.createElement("div");
  document.body.append(el);
  const r = createRoot(el);
  root = r;
  await act(async () => r.render(node));
}

afterEach(() => {
  if (root) act(() => root!.unmount());
  root = undefined;
  document.body.innerHTML = "";
  vi.unstubAllGlobals();
});

test("the addresses of the list and of one project", () => {
  expect(parse("/projects")).toEqual({ tab: "overview", view: "projects" });
  expect(parse(`/projects/${P}`)).toEqual({ tab: "overview", view: "project", projectId: P });
  expect(href({ tab: "overview", view: "project", projectId: P })).toBe(`/projects/${P}`);
});

test("the list has a card per project, and finds one by a folder's name", async () => {
  serve({ "GET /api/v1/projects": { owned: [proj(P, "web", ["design"]), proj("p2", "ops", ["runbooks"])], shared: [] } });
  await render(<ProjectsPage />);
  expect(document.body.textContent).toContain("web");
  expect(document.body.textContent).toContain("runbooks");
  const box = document.querySelector<HTMLInputElement>('input[aria-label="Find a project or folder"]')!;
  await act(async () => {
    const set = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!.set!;
    set.call(box, "runb");
    box.dispatchEvent(new Event("input", { bubbles: true }));
  });
  const cards = [...document.querySelectorAll("h3")].map((h) => h.textContent);
  expect(cards).toEqual(["ops"]);
});

test("a project opens on its first folder, and Remove unfiles the datapack", async () => {
  serve({ [`GET /api/v1/projects/${P}`]: DETAIL, [`DELETE /api/v1/datapacks/${DP}/placement`]: { datapack_id: DP } });
  await render(<ProjectPage id={P} />);
  expect(document.body.textContent).toContain("how it looks");
  expect(document.body.textContent).toContain("figma notes");
  expect(document.body.textContent).toContain("1 learnings");
  const remove = [...document.querySelectorAll("button")].find((b) => b.textContent === "Remove")!;
  await act(async () => remove.click());
  expect(sent).toContain(`DELETE /api/v1/datapacks/${DP}/placement`);
  expect(sent.some((s) => s === `DELETE /api/v1/datapacks/${DP}`)).toBe(false);
});
