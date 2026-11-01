// @vitest-environment happy-dom
// The Context Gaps tab and the Overview's line about it, against a faked /api/v1.

import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, expect, test, vi } from "vitest";
import type { Datapack, Gap, GapPage } from "../src/lib/types";
import { href, parse as parseRoute } from "../src/lib/route";
import { CloudContextGapsPage, ContextGapsPage } from "../src/pages/ContextGapsPage";
import { GapsLine } from "../src/pages/OverviewPage";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const ID = "11111111-2222-3333-4444-555555555555";
const DATAPACK = { datapack_id: ID, datapack_name: "demo" } as Datapack;

function gap(key: string, over: Partial<Gap> = {}): Gap {
  return {
    key,
    query: key,
    quality: "empty",
    tool: "context_search",
    results: 0,
    top_score: null,
    top_raw: null,
    asks: 1,
    agents: ["claude_code:demo-repo"],
    conversations: ["0123456789abcdef0123456789abcdef"],
    asked_by_conversation: false,
    first_at: "2026-10-14T10:00:00Z",
    last_at: "2026-10-14T10:00:00Z",
    state: "open",
    marked_at: null,
    taught: null,
    dismissal: null,
    ...over,
  };
}

function page(questions: Gap[], over: Partial<GapPage> = {}): GapPage {
  return {
    questions,
    more: false,
    counts: { open: questions.filter((q) => q.state === "open").length, dismissed: 0, taught: 0 },
    open: { empty: 0, weak: 0 },
    searches: 12,
    since: "2026-10-01T00:00:00Z",
    ...over,
  };
}

let root: Root;
let calls: { method: string; url: string; body?: unknown }[];

type Reply = { status?: number; data?: unknown; error?: { code: string; message: string } };

// Meko's replies, by "METHOD /path?query"; anything else is a 404.
function serve(routes: Record<string, (body: unknown) => Reply>) {
  calls = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init?: RequestInit) => {
      const method = init?.method ?? "GET";
      const body = init?.body ? JSON.parse(init.body as string) : undefined;
      calls.push({ method, url, body });
      const r = routes[`${method} ${url}`]?.(body) ?? {
        status: 404,
        error: { code: "NOT_FOUND", message: "not found" },
      };
      if (r.status === 204) return new Response(null, { status: 204 });
      return Response.json({ data: r.data ?? null, error: r.error ?? null }, { status: r.status ?? 200 });
    }),
  );
}

async function render(node: React.ReactNode) {
  const el = document.createElement("div");
  document.body.append(el);
  root = createRoot(el);
  await act(async () => root.render(node));
}

function text(): string {
  return document.body.textContent ?? "";
}

function button(label: string, within: ParentNode = document): HTMLButtonElement {
  const b = [...within.querySelectorAll("button")].find((x) => x.textContent?.trim() === label);
  if (!b) throw new Error(`no ${label} button in: ${text()}`);
  return b;
}

async function click(label: string, within?: ParentNode) {
  await act(async () => button(label, within).click());
}

afterEach(() => {
  act(() => root.unmount());
  document.body.innerHTML = "";
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

const LIST = `GET /api/v1/datapacks/${ID}/questions?state=`;

test("lists the open questions with their counts, quality and where they were asked", async () => {
  serve({
    [`${LIST}open`]: () => ({
      data: page(
        [
          gap("where do release tarballs go", { asks: 3, agents: ["claude_code:demo-repo", "kiro"] }),
          gap("how does the sweep pick chunks", { quality: "weak", results: 4, top_score: 0.61, top_raw: 0.42 }),
        ],
        { counts: { open: 2, dismissed: 1, taught: 4 }, open: { empty: 1, weak: 1 } },
      ),
    }),
  });
  await render(<ContextGapsPage datapack={DATAPACK} />);

  expect(text()).toContain("Open 2");
  expect(text()).toContain("Taught 4");
  expect(text()).toContain("Dismissed 1");
  expect(text()).toContain("12 searches, 2 open questions");
  expect(text()).toContain("where do release tarballs go");
  expect(text()).toContain("Asked 3 times by claude_code:demo-repo, kiro");
  expect(text()).toContain("Found nothing");
  expect(text()).toContain("Nothing relevant");
  expect(text()).toContain("4 found · best score 0.61 · best cosine 0.42");
  const link = document.querySelector<HTMLAnchorElement>("a[title='0123456789abcdef0123456789abcdef']");
  expect(link?.getAttribute("href")).toBe(`/datapacks/${ID}/conversations/0123456789abcdef0123456789abcdef`);
});

test("an empty Open list says what will show up", async () => {
  serve({ [`${LIST}open`]: () => ({ data: page([]) }) });
  await render(<ContextGapsPage datapack={DATAPACK} />);
  expect(text()).toContain("Nothing Meko couldn't answer");
});

test("Dismiss marks the question and reloads; Restore sends null", async () => {
  let dismissed = false;
  serve({
    [`${LIST}open`]: () => ({ data: page(dismissed ? [] : [gap("q1")]) }),
    [`${LIST}dismissed`]: () => ({
      data: page([gap("q1", { state: dismissed ? "dismissed" : "open", marked_at: "2026-10-15T00:00:00Z" })]),
    }),
    [`POST /api/v1/datapacks/${ID}/questions/mark`]: (body) => {
      dismissed = (body as { state: string | null }).state === "dismissed";
      return { status: 204 };
    },
  });
  await render(<ContextGapsPage datapack={DATAPACK} />);
  await click("Dismiss");
  expect(calls.find((c) => c.method === "POST")?.body).toEqual({ key: "q1", state: "dismissed" });
  expect(text()).not.toContain("q1");

  await click("Dismissed 0");
  await click("Restore");
  expect(calls.filter((c) => c.method === "POST").at(-1)?.body).toEqual({ key: "q1", state: null });
});

test("Teach saves the text and the question shows it taught", async () => {
  let taught: Gap | undefined;
  serve({
    [`${LIST}open`]: () => ({ data: page(taught ? [] : [gap("q1")]) }),
    [`${LIST}taught`]: () => ({ data: page(taught ? [taught] : []) }),
    [`POST /api/v1/datapacks/${ID}/questions/teach`]: (body) => {
      const b = body as { key: string; text: string };
      taught = gap(b.key, {
        state: "taught",
        taught: { text: b.text, memory_id: "m1", at: "2026-10-16T00:00:00Z", conversation_id: null },
      });
      return { data: taught };
    },
  });
  await render(<ContextGapsPage datapack={DATAPACK} />);
  await click("Teach");
  expect(text()).toContain("Saved as a memory in demo");
  const area = document.querySelector("textarea")!;
  await act(async () => {
    const set = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, "value")!.set!;
    set.call(area, "  Release tarballs go to GitHub releases.  ");
    area.dispatchEvent(new Event("input", { bubbles: true }));
  });
  await click("Save as a memory");
  expect(calls.find((c) => c.url.endsWith("/teach"))?.body).toEqual({
    key: "q1",
    text: "Release tarballs go to GitHub releases.",
  });
  expect(document.querySelector("[role=dialog]")).toBeNull();

  await click("Taught 0");
  expect(text()).toContain("Taught on");
  expect(text()).toContain("Release tarballs go to GitHub releases.");
});

test("an agent's dismissal says which conversation it came from", async () => {
  serve({
    [`${LIST}open`]: () => ({
      data: page([
        gap("q1", {
          state: "dismissed",
          marked_at: "2026-10-15T00:00:00Z",
          dismissal: {
            conversation_id: "fedcba9876543210fedcba9876543210",
            memory_id: "abcdef12-0000",
            memory_text: null,
          },
        }),
        gap("q2", {
          state: "dismissed",
          marked_at: "2026-10-15T00:00:00Z",
          dismissal: {
            conversation_id: "fedcba9876543210fedcba9876543210",
            memory_id: "0123abcd-0000",
            memory_text: "The deploy key is in the team vault.",
          },
        }),
      ]),
    }),
  });
  await render(<ContextGapsPage datapack={DATAPACK} />);
  expect(text()).toContain("Dismissed by an agent in conversation fedcba98");
  expect(text()).toContain("Meko already had this in memory abcdef12");
  expect(text()).toContain("Meko already had this:The deploy key is in the team vault.");
});

test("the Overview shows the open questions' line only while there are some", async () => {
  serve({ [`${LIST}open`]: () => ({ data: page([gap("q1")], { open: { empty: 5, weak: 4 } }) }) });
  await render(<GapsLine datapackId={ID} />);
  expect(text()).toContain("9 questions Meko couldn't answer");
  expect(text()).toContain("5 found nothing · 4 nothing relevant");
  expect(document.querySelector("a")?.getAttribute("href")).toBe(`/datapacks/${ID}/gaps`);
  act(() => root.unmount());
  document.body.innerHTML = "";

  serve({ [`${LIST}open`]: () => ({ data: page([]) }) });
  await render(<GapsLine datapackId={ID} />);
  expect(text()).toBe("");
  act(() => root.unmount());
  document.body.innerHTML = "";

  // An older server has no such route.
  serve({});
  await render(<GapsLine datapackId={ID} />);
  expect(text()).toBe("");
});

test("a Meko Cloud datapack's Context Gaps has a page of its own, named from the datapacks' listing", async () => {
  serve({
    [`${LIST}open`]: () => ({ data: page([gap("Who owns the pager?")]) }),
    "GET /api/v1/datapacks": () => ({
      data: [{ datapack_id: ID, datapack_name: "team-cloud", description: "", grant: "owner" }],
    }),
  });
  await render(<CloudContextGapsPage id={ID} />);
  expect(text()).toContain("team-cloud, in Meko Cloud");
  expect(text()).toContain("Who owns the pager?");
  // Its conversations aren't on this Meko: no links to them.
  expect(document.querySelector('a[href*="/conversations/"]')).toBeNull();
  expect(parseRoute(`/gaps/${ID}`)).toEqual({ tab: "gaps", view: "cloud-gaps", datapackId: ID });
  expect(href({ tab: "gaps", view: "cloud-gaps", datapackId: ID })).toBe(`/gaps/${ID}`);
});

test("a taught question says it was asked again only when it was", async () => {
  const taught = { text: "Ops owns it.", memory_id: "m1", at: "2026-10-15T00:00:00Z", conversation_id: null };
  serve({
    [`${LIST}open`]: () => ({
      data: page([
        gap("asked again", { last_at: "2026-10-16T00:00:00Z", taught }),
        gap("restored", { last_at: "2026-10-14T00:00:00Z", taught }),
      ]),
    }),
  });
  await render(<ContextGapsPage datapack={DATAPACK} />);
  expect(text().match(/asked again since/g)).toHaveLength(1);
});
