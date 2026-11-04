// @vitest-environment happy-dom
// The Connect page's Agents card and the Meko Cloud page's line about the agents a link change
// moved, against a faked /api/v1.

import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, expect, test, vi } from "vitest";
import { AgentsCard } from "../src/components/AgentsCard";
import type { Agent, AgentMoves, Agents, MekoCloud } from "../src/lib/types";
import { MekoCloudPage } from "../src/pages/MekoCloudPage";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const LOCAL = "http://127.0.0.1:7778/mcp";

function agent(
  name: string,
  title: string,
  state: Agent["state"],
  url: string | null = null,
  notes: string[] = [],
): Agent {
  return { name, title, command: `~/.local/bin/${name}`, state, url, notes };
}

let root: Root;
let calls: { method: string; url: string }[];

type Reply = { status?: number; data?: unknown; error?: { code: string; message: string } };

// Meko's replies, by "METHOD /path"; anything else is a 404.
function serve(routes: Record<string, () => Reply>) {
  calls = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init?: RequestInit) => {
      const method = init?.method ?? "GET";
      calls.push({ method, url });
      const r = routes[`${method} ${url}`]?.() ?? {
        status: 404,
        error: { code: "NOT_FOUND", message: "not found" },
      };
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

function button(label: string): HTMLButtonElement {
  const b = [...document.querySelectorAll("button")].find((x) => x.textContent?.trim() === label);
  if (!b) throw new Error(`no ${label} button in: ${text()}`);
  return b;
}

async function click(label: string) {
  await act(async () => button(label).click());
}

// The row of the agent with this title.
function row(title: string): HTMLElement {
  const li = [...document.querySelectorAll("li")].find((x) => x.textContent?.startsWith(title));
  if (!li) throw new Error(`no ${title} row in: ${text()}`);
  return li;
}

function buttons(el: HTMLElement): string[] {
  return [...el.querySelectorAll("button")].map((b) => b.textContent?.trim() ?? "");
}

afterEach(() => {
  act(() => root.unmount());
  document.body.innerHTML = "";
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

test("the card shows each agent's state, its notes and the buttons that fit it", async () => {
  const list: Agents = {
    target: { kind: "local", url: LOCAL },
    agents: [
      agent("claude", "Claude Code", "connected", LOCAL, [
        "~/demo-repo has its own meko server, which Claude Code uses there",
      ]),
      agent("codex", "Codex", "stale", LOCAL),
      agent("kiro", "Kiro CLI", "none"),
      agent("opencode", "OpenCode", "elsewhere", "https://meko.example.com/mcp"),
    ],
  };
  serve({ "GET /api/v1/agents": () => ({ data: list }) });
  await render(<AgentsCard />);

  expect(text()).toContain("Agents connect to Meko on this computer");
  expect(row("Claude Code").textContent).toContain("Connected");
  expect(row("Claude Code").textContent).toContain("~/demo-repo has its own meko server");
  expect(buttons(row("Claude Code"))).toEqual(["Disconnect"]);
  expect(row("Codex").textContent).toContain("Needs reconnecting");
  expect(buttons(row("Codex"))).toEqual(["Connect", "Disconnect"]);
  expect(row("Kiro CLI").textContent).toContain("Not connected");
  expect(buttons(row("Kiro CLI"))).toEqual(["Connect"]);
  expect(row("OpenCode").textContent).toContain("Connected elsewhere");
  expect(row("OpenCode").textContent).toContain("https://meko.example.com/mcp");
  expect(buttons(row("OpenCode"))).toEqual(["Connect"]);
  expect(text()).not.toContain("coding agents");
});

test("the card says agents connect to this Meko, which reaches a linked Meko Cloud for them", async () => {
  serve({
    "GET /api/v1/agents": () => ({
      data: {
        target: { kind: "local", url: LOCAL },
        agents: [agent("kiro", "Kiro CLI", "none")],
      },
    }),
  });
  await render(<AgentsCard />);
  expect(text()).toContain("Agents connect to Meko on this computer, which reaches Meko Cloud for them while it's linked.");
});

test("with no agent installed, the card names the ones Meko works with", async () => {
  serve({ "GET /api/v1/agents": () => ({ data: { target: { kind: "local", url: LOCAL }, agents: [] } }) });
  await render(<AgentsCard />);
  expect(text()).toContain(
    "No agents found on this computer: Meko works with Claude Code, Codex, Kiro CLI, OpenCode and Cursor",
  );
});

test("Connect posts the agent's route, reads the list again and shows the reply's notes", async () => {
  let state: Agent["state"] = "elsewhere";
  serve({
    "GET /api/v1/agents": () => ({
      data: {
        target: { kind: "local", url: LOCAL },
        agents: [agent("kiro", "Kiro CLI", state, state === "connected" ? LOCAL : "https://meko.example.com/mcp")],
      },
    }),
    "POST /api/v1/agents/kiro/connect": () => {
      state = "connected";
      return {
        data: {
          agent: agent("kiro", "Kiro CLI", "connected", LOCAL),
          notes: ["It replaced the meko server at https://meko.example.com/mcp"],
        },
      };
    },
  });
  await render(<AgentsCard />);
  await click("Connect");

  expect(calls.map((c) => `${c.method} ${c.url}`)).toEqual([
    "GET /api/v1/agents",
    "POST /api/v1/agents/kiro/connect",
    "GET /api/v1/agents",
  ]);
  expect(row("Kiro CLI").textContent).toContain("Connected");
  expect(buttons(row("Kiro CLI"))).toEqual(["Disconnect"]);
  expect(text()).toContain("It replaced the meko server at https://meko.example.com/mcp");
  expect(text()).toContain("New sessions have Meko's tools; running ones need a restart");
});

test("Disconnect posts the agent's route and reads the list again", async () => {
  let state: Agent["state"] = "connected";
  serve({
    "GET /api/v1/agents": () => ({
      data: {
        target: { kind: "local", url: LOCAL },
        agents: [agent("claude", "Claude Code", state, state === "none" ? null : LOCAL)],
      },
    }),
    "POST /api/v1/agents/claude/disconnect": () => {
      state = "none";
      return { data: { agent: agent("claude", "Claude Code", "none"), notes: [] } };
    },
  });
  await render(<AgentsCard />);
  await click("Disconnect");

  expect(calls.map((c) => `${c.method} ${c.url}`)).toEqual([
    "GET /api/v1/agents",
    "POST /api/v1/agents/claude/disconnect",
    "GET /api/v1/agents",
  ]);
  expect(row("Claude Code").textContent).toContain("Not connected");
  expect(buttons(row("Claude Code"))).toEqual(["Connect"]);
});

test("a failed connect shows Meko's message under the agent", async () => {
  serve({
    "GET /api/v1/agents": () => ({
      data: { target: { kind: "local", url: LOCAL }, agents: [agent("codex", "Codex", "none")] },
    }),
    "POST /api/v1/agents/codex/connect": () => ({
      status: 502,
      error: { code: "AGENT_CLI_FAILED", message: "codex mcp add failed: exit status 1" },
    }),
  });
  await render(<AgentsCard />);
  await click("Connect");

  expect(row("Codex").textContent).toContain("codex mcp add failed: exit status 1");
  expect(row("Codex").textContent).toContain("Not connected");
});

test("a server without the agents route shows no card", async () => {
  serve({});
  await render(<AgentsCard />);
  expect(calls.map((c) => c.url)).toEqual(["/api/v1/agents"]);
  expect(text()).toBe("");
});

// Meko Cloud's settings, linked or not.
function cloud(linked: boolean, agents?: AgentMoves): MekoCloud {
  return {
    set: linked,
    linked,
    url: "",
    web_url: "",
    api_url: "",
    effective_url: "https://meko.example.com/mcp",
    effective_web_url: "https://meko.example.com",
    effective_api_url: "https://meko.example.com/api/v1",
    key_hint: linked ? "abcd" : null,
    default_url: "https://meko.example.com/mcp",
    default_web_url: "https://meko.example.com",
    agents,
  };
}

async function type(id: string, value: string) {
  const input = document.getElementById(id) as HTMLInputElement;
  await act(async () => {
    Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!.set!.call(input, value);
    input.dispatchEvent(new Event("input", { bubbles: true }));
  });
}

test("linking says which agents moved back to this Meko and which couldn't", async () => {
  serve({
    "GET /api/v1/settings/meko-cloud": () => ({ data: cloud(false) }),
    "PUT /api/v1/settings/meko-cloud": () => ({
      data: cloud(true, {
        moved: [
          { name: "claude", title: "Claude Code" },
          { name: "opencode", title: "OpenCode" },
        ],
        left: [],
        failed: [{ name: "kiro", title: "Kiro CLI", error: "kiro-cli exited 1" }],
      }),
    }),
  });
  await render(<MekoCloudPage />);
  await type("meko-cloud-key", "test-token");
  await act(async () => document.querySelector("form")!.requestSubmit());

  expect(text()).toContain("Moved Claude Code and OpenCode back to this Meko. Couldn't move Kiro CLI: kiro-cli exited 1");
});

test("unlinking says which agents moved back, and nothing when none moved", async () => {
  let moves: AgentMoves | undefined = { moved: [{ name: "kiro", title: "Kiro CLI" }], left: [], failed: [] };
  serve({
    "GET /api/v1/settings/meko-cloud": () => ({ data: cloud(true) }),
    "DELETE /api/v1/settings/meko-cloud": () => ({ data: cloud(false, moves) }),
  });
  await render(<MekoCloudPage />);
  await click("Unlink…");
  await click("Unlink");
  expect(text()).toContain("Moved Kiro CLI back to this Meko");

  // An older server's reply has no agents, and a link change that moved none says nothing about them.
  for (const m of [
    undefined,
    { moved: [], left: [{ name: "opencode", title: "OpenCode", url: "https://meko.example.com/mcp" }], failed: [] },
  ]) {
    act(() => root.unmount());
    document.body.innerHTML = "";
    moves = m;
    await render(<MekoCloudPage />);
    await click("Unlink…");
    await click("Unlink");
    expect(text()).toContain("Unlinked.");
    expect(text()).not.toContain("Moved");
    expect(text()).not.toContain("OpenCode");
  }
});
