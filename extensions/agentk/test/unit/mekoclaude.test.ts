import { describe, expect, it } from 'vitest';
import { DEFAULT_CONFIG, type Config } from '../../src/server/config.js';
import { silentLogger } from '../../src/server/log.js';
import type { TokenTry } from '../../src/server/meko.js';
import { CLAUDE_MEKO_TTL_MS, MekoClaudeWatch, claudeTokenGaps } from '../../src/server/mekoclaude.js';
import type { ClaudeMeko } from '../../src/server/mekosetup.js';

const CLOUD = 'https://mcp.mekodata.ai/mcp';

/** Claude Code set up on Meko Cloud with the plugin, holding this token in its settings. */
function claude(o: { settings?: string | null; settingsUrl?: string | null } = {}): ClaudeMeko {
  return { plugin: 'ok', settings: { url: o.settingsUrl ?? null, token: o.settings === undefined ? 'agentk' : o.settings } };
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
  const t = {
    cfg,
    claude: o.claude ?? claude(),
    tokens: o.tokens ?? {},
    own: o.own ?? ['dp1', 'dp2'],
    available: o.available ?? true,
  };
  const watch = new MekoClaudeWatch({
    config: () => t.cfg,
    available: () => t.available,
    read: () => t.claude,
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
  };
}

describe("Claude Code's Meko: the watch", () => {
  it.each([
    ['Meko is off', { key: '' }],
    ["Claude Code isn't available", { available: false }],
  ])('reports nothing and calls nothing while %s', async (_, o) => {
    const s = setup({ ...o, claude: claude({ settings: 'x' }) });
    await s.watch.refresh();
    expect(s.watch.status).toEqual({ gaps: [], capture: [], unchecked: [], checking: false });
    expect(s.tried).toEqual([]);
  });

  it("doesn't try a token that's AgentK's own", async () => {
    const s = setup();
    await s.watch.refresh();
    expect(s.watch.status).toEqual({ gaps: [], capture: [], unchecked: [], checking: false });
    expect(s.tried).toEqual([]);
    expect(s.ownCalls()).toBe(0);
  });

  it('accepts another token that sees the project datapacks AgentK sees', async () => {
    const s = setup({ claude: claude({ settings: 'mine' }), tokens: { mine: OK(['dp1', 'dp2', 'extra']) } });
    await s.watch.refresh();
    expect(s.watch.status.gaps).toEqual([]);
    expect(s.tried).toEqual(['mine']);
  });

  it('names a token Meko turns down, as a gap Meko\'s setup fixes', async () => {
    const s = setup({ claude: claude({ settings: 'bad' }) });
    await s.watch.refresh();
    const gap = "The token in Claude Code's settings isn't accepted by mcp.mekodata.ai.";
    expect(s.watch.status).toMatchObject({ gaps: [gap], capture: [gap] });
    // "checking", then the result; the same result again changes nothing.
    expect(s.changes()).toBe(2);
    await s.watch.refresh();
    expect(s.changes()).toBe(2);
  });

  it.each([
    ['2 of them', ['dp9'], "The token in Claude Code's settings can't see 2 of AgentK's project datapacks (adlc/proj/repo/one, adlc/proj/repo/two) on mcp.mekodata.ai."],
    ['one', ['dp1'], "The token in Claude Code's settings can't see the datapack adlc/proj/repo/two on mcp.mekodata.ai."],
  ])("names the datapacks another account's token can't see (%s), ignoring ones AgentK can't see either", async (_, sees, gap) => {
    const s = setup({ claude: claude({ settings: 'other' }), tokens: { other: OK(sees) } });
    await s.watch.refresh();
    expect(s.watch.status.gaps).toEqual([gap]);
  });

  it("calls a token it couldn't check unchecked, not a gap, and tries it again next time", async () => {
    const s = setup({ claude: claude({ settings: 'slow' }), tokens: { slow: { ok: false, rejected: false, error: 'Request timed out' } } });
    await s.watch.refresh();
    expect(s.watch.status).toEqual({
      gaps: [],
      capture: [],
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
    const s = setup({ claude: claude({ settings: 'mine' }), tokens: { mine: OK(['dp1', 'dp2']) } });
    await s.watch.refresh();
    s.t.cfg.meko.apiKey = 'agentk-new';
    await s.watch.refresh();
    expect(s.tried).toEqual(['mine', 'mine']);
  });

  it("doesn't try a token for another server: that's a gap of its own", async () => {
    const s = setup({ claude: claude({ settings: 'mine', settingsUrl: 'https://other.example.org/mcp' }) });
    await s.watch.refresh();
    expect(s.watch.status.gaps).toEqual(["Claude Code's settings point at other.example.org, not mcp.mekodata.ai."]);
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
  it('counts only a token Meko turns down, and never tries AgentK’s own', async () => {
    const tried: string[] = [];
    const tryToken = async (_url: string, token: string): Promise<TokenTry> => {
      tried.push(token);
      if (token === 'bad') return { ok: false, rejected: true, error: 'invalid_token' };
      if (token === 'slow') return { ok: false, rejected: false, error: 'timeout' };
      return OK([]);
    };
    expect(await claudeTokenGaps(claude({ settings: 'bad' }), CLOUD, 'agentk', tryToken)).toEqual([
      "The token in Claude Code's settings isn't accepted by mcp.mekodata.ai.",
    ]);
    expect(await claudeTokenGaps(claude({ settings: 'slow' }), CLOUD, 'agentk', tryToken)).toEqual([]);
    expect(await claudeTokenGaps(claude({ settings: 'agentk' }), CLOUD, 'agentk', tryToken)).toEqual([]);
    expect(tried).toEqual(['bad', 'slow']);
  });
});
