import { describe, expect, it } from 'vitest';
import { DEFAULT_CONFIG, type Config } from '../../src/server/config.js';
import { silentLogger } from '../../src/server/log.js';
import type { TokenTry } from '../../src/server/meko.js';
import { CLAUDE_MEKO_TTL_MS, MekoClaudeWatch, claudeTokenGaps } from '../../src/server/mekoclaude.js';
import type { ClaudeMeko } from '../../src/server/mekosetup.js';

const CLOUD = 'https://mcp.mekodata.ai/mcp';

/** Claude Code set up on Meko Cloud with the plugin, holding these tokens. */
function claude(o: { settings?: string | null; server?: string | null; settingsUrl?: string | null; serverUrl?: string } = {}): ClaudeMeko {
  return {
    plugin: 'ok',
    settings: { url: o.settingsUrl ?? null, token: o.settings === undefined ? 'agentk' : o.settings },
    server: { url: o.serverUrl ?? CLOUD, token: o.server === undefined ? 'agentk' : o.server },
  };
}

const OK = (datapacks: string[]): TokenTry => ({ ok: true, datapacks });

/** A watch over stand-ins: AgentK on Meko Cloud with key `agentk`, two projects' datapacks. */
function setup(o: { claude?: ClaudeMeko; tokens?: Record<string, TokenTry>; own?: string[] | Error; key?: string; available?: boolean } = {}) {
  const cfg: Config = structuredClone(DEFAULT_CONFIG);
  cfg.meko.apiKey = o.key ?? 'agentk';
  const tried: string[] = [];
  let now = 1_000_000;
  let changes = 0;
  let ownCalls = 0;
  let registeredCalls = 0;
  const t = {
    cfg,
    claude: o.claude ?? claude(),
    tokens: o.tokens ?? {},
    own: o.own ?? ['dp1', 'dp2'],
    registered: true as boolean | null,
    available: o.available ?? true,
  };
  const watch = new MekoClaudeWatch({
    config: () => t.cfg,
    available: () => t.available,
    read: () => t.claude,
    mcpRegistered: async () => (registeredCalls++, t.registered),
    projectDatapacks: () => [{ id: 'dp1', name: 'adlc/proj/repo/one' }, { id: 'dp2', name: 'adlc/proj/repo/two' }, { id: 'gone', name: 'adlc/proj/repo/gone' }],
    ownDatapacks: async () => {
      ownCalls++;
      if (t.own instanceof Error) throw t.own;
      return t.own;
    },
    tryToken: async (_url, token) => (tried.push(token), t.tokens[token] ?? { ok: false, rejected: true, error: 'invalid_token' }),
    changed: () => changes++,
    log: silentLogger,
    now: () => now,
  });
  return {
    t, watch, tried,
    later: (ms: number) => (now += ms),
    changes: () => changes,
    ownCalls: () => ownCalls,
    registeredCalls: () => registeredCalls,
  };
}

describe("Claude Code's Meko: the watch", () => {
  it.each([
    ['Meko is off', { key: '' }],
    ["Claude Code isn't available", { available: false }],
  ])('reports nothing and calls nothing while %s', async (_, o) => {
    const s = setup({ ...o, claude: claude({ settings: 'x', server: 'y' }) });
    await s.watch.refresh();
    expect(s.watch.status).toEqual({ gaps: [], unchecked: [], checking: false });
    expect(s.tried).toEqual([]);
    expect(s.registeredCalls()).toBe(0);
  });

  it("doesn't try tokens that are AgentK's own", async () => {
    const s = setup();
    await s.watch.refresh();
    expect(s.watch.status).toEqual({ gaps: [], unchecked: [], checking: false });
    expect(s.tried).toEqual([]);
    expect(s.ownCalls()).toBe(0);
  });

  it('accepts other tokens that see the project datapacks AgentK sees', async () => {
    const s = setup({ claude: claude({ settings: 'mine', server: 'mine' }), tokens: { mine: OK(['dp1', 'dp2', 'extra']) } });
    await s.watch.refresh();
    expect(s.watch.status.gaps).toEqual([]);
    // One try for a token both places hold.
    expect(s.tried).toEqual(['mine']);
  });

  it('names a token Meko turns down, and where it is', async () => {
    const s = setup({ claude: claude({ server: 'bad' }) });
    await s.watch.refresh();
    expect(s.watch.status.gaps).toEqual(["The token on Claude Code's meko MCP server isn't accepted by mcp.mekodata.ai."]);
    // "checking", then the result; the same result again changes nothing.
    expect(s.changes()).toBe(2);
    await s.watch.refresh();
    expect(s.changes()).toBe(2);
  });

  it("names the datapacks another account's token can't see, ignoring ones AgentK can't see either", async () => {
    const s = setup({ claude: claude({ settings: 'other', server: 'two' }), tokens: { other: OK(['dp9']), two: OK(['dp1']) } });
    await s.watch.refresh();
    expect(s.watch.status.gaps).toEqual([
      "The token in Claude Code's settings can't see 2 of AgentK's project datapacks (adlc/proj/repo/one, adlc/proj/repo/two) on mcp.mekodata.ai.",
      "The token on Claude Code's meko MCP server can't see the datapack adlc/proj/repo/two on mcp.mekodata.ai.",
    ]);
  });

  it("calls a token it couldn't check unchecked, not a gap, and tries it again next time", async () => {
    const s = setup({ claude: claude({ settings: 'slow' }), tokens: { slow: { ok: false, rejected: false, error: 'Request timed out' } } });
    await s.watch.refresh();
    expect(s.watch.status).toEqual({
      gaps: [],
      unchecked: ["Couldn't check the token in Claude Code's settings on mcp.mekodata.ai: Request timed out."],
      checking: false,
    });
    await s.watch.refresh();
    expect(s.tried).toEqual(['slow', 'slow']);
  });

  it("can't check tokens when AgentK can't list its own datapacks", async () => {
    const s = setup({ claude: claude({ settings: 'mine' }), tokens: { mine: OK([]) }, own: new Error('Meko is down') });
    await s.watch.refresh();
    expect(s.watch.status.gaps).toEqual([]);
    expect(s.watch.status.unchecked[0]).toMatch(/AgentK couldn't list its own datapacks \(Meko is down\)/);
    expect(s.tried).toEqual([]);
  });

  it('reuses results until they expire, and starts over when forced', async () => {
    const s = setup({ claude: claude({ settings: 'mine' }), tokens: { mine: OK(['dp1', 'dp2']) } });
    await s.watch.refresh();
    await s.watch.refresh();
    expect(s.tried).toEqual(['mine']);
    expect(s.ownCalls()).toBe(1);
    s.later(CLAUDE_MEKO_TTL_MS);
    await s.watch.refresh();
    expect(s.tried).toEqual(['mine', 'mine']);
    await s.watch.refresh(true);
    expect(s.tried).toEqual(['mine', 'mine', 'mine']);
    expect(s.ownCalls()).toBe(3);
  });

  it("checks again when AgentK's key or the project datapacks change", async () => {
    const s = setup({ claude: claude({ settings: 'mine', server: 'mine' }), tokens: { mine: OK(['dp1', 'dp2']) } });
    await s.watch.refresh();
    s.t.cfg.meko.apiKey = 'agentk-new';
    await s.watch.refresh();
    expect(s.tried).toEqual(['mine', 'mine']);
  });

  it("doesn't try a token that sits on another server: that's a gap of its own", async () => {
    const s = setup({ claude: claude({ server: 'mine', serverUrl: 'https://other.example.org/mcp' }) });
    await s.watch.refresh();
    expect(s.watch.status.gaps).toEqual(["Claude Code's meko MCP server points at other.example.org, not mcp.mekodata.ai."]);
    expect(s.tried).toEqual([]);
  });

  it('asks whether claude knows a meko server only without a user-scope one, and reuses the answer', async () => {
    const s = setup({ claude: { ...claude(), server: null } });
    s.t.registered = false;
    await s.watch.refresh();
    await s.watch.refresh();
    expect(s.watch.status.gaps).toEqual(['Claude Code has no meko MCP server.']);
    expect(s.registeredCalls()).toBe(1);
    s.t.claude = claude();
    await s.watch.refresh();
    expect(s.watch.status.gaps).toEqual([]);
    expect(s.registeredCalls()).toBe(1);
  });

  it("lists a meko MCP server Claude Code couldn't be asked about as unchecked, and asks again next time", async () => {
    const s = setup({ claude: { ...claude(), server: null } });
    s.t.registered = null;
    await s.watch.refresh();
    expect(s.watch.status).toEqual({
      gaps: [],
      unchecked: ["Couldn't ask Claude Code whether it has a meko MCP server (`claude mcp get meko` didn't answer)."],
      checking: false,
    });
    await s.watch.refresh();
    expect(s.registeredCalls()).toBe(2);
  });

  it("lists a token from a variable AgentK can't see as unchecked, never as a gap", async () => {
    const s = setup({ claude: { ...claude(), server: { url: CLOUD, token: null, unresolved: 'MY_TOKEN' } } });
    await s.watch.refresh();
    expect(s.watch.status.gaps).toEqual([]);
    expect(s.watch.status.unchecked).toEqual([
      "Couldn't check the token on Claude Code's meko MCP server: it comes from ${MY_TOKEN}, which AgentK can't see (it isn't in Claude Code's settings env).",
    ]);
    expect(s.tried).toEqual([]);
  });

  it('runs refreshes one at a time; ones asked for meanwhile share the next run', async () => {
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    const s = setup({ claude: claude({ settings: 'mine' }) });
    const calls: string[] = [];
    s.t.tokens.mine = OK(['dp1', 'dp2']);
    const slow = new MekoClaudeWatch({
      config: () => s.t.cfg,
      available: () => true,
      read: () => s.t.claude,
      mcpRegistered: async () => true,
      projectDatapacks: () => [],
      ownDatapacks: async () => [],
      tryToken: async (_u, token) => {
        calls.push(token);
        await gate;
        return OK([]);
      },
      changed: () => undefined,
      log: silentLogger,
    });
    const first = slow.refresh(true);
    await Promise.resolve();
    const second = slow.refresh(true);
    const third = slow.refresh(true);
    expect(second).toBe(third);
    release();
    await Promise.all([first, second, third]);
    expect(calls).toEqual(['mine', 'mine']);
  });
});

describe("Claude Code's Meko: the installer's token check", () => {
  it('counts only tokens Meko turns down, and never tries AgentK’s own', async () => {
    const tried: string[] = [];
    const tryToken = async (_url: string, token: string): Promise<TokenTry> => {
      tried.push(token);
      if (token === 'bad') return { ok: false, rejected: true, error: 'invalid_token' };
      if (token === 'slow') return { ok: false, rejected: false, error: 'timeout' };
      return OK([]);
    };
    expect(await claudeTokenGaps(claude({ settings: 'bad', server: 'agentk' }), CLOUD, 'agentk', tryToken)).toEqual([
      "The token in Claude Code's settings isn't accepted by mcp.mekodata.ai.",
    ]);
    expect(await claudeTokenGaps(claude({ settings: 'slow', server: 'fine' }), CLOUD, 'agentk', tryToken)).toEqual([]);
    expect(tried).toEqual(['bad', 'slow', 'fine']);
  });
});
