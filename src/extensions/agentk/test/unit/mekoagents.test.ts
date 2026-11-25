import { describe, expect, it } from 'vitest';
import { openDb } from '../../src/server/db.js';
import { silentLogger } from '../../src/server/log.js';
import { MekoAgents, mekoMcpGaps, parseMekoAgents, readMekoAgents, type MekoAgentList, type MekoAgentsRead } from '../../src/server/mekoagents.js';
import { MekoAutoConnect } from '../../src/server/mekoautoconnect.js';
import type { HarnessMekoState, HarnessStatus } from '../../src/shared/types.js';

const LOCAL = { url: 'http://127.0.0.1:7778/mcp', token: 'test-token' };
const LIST: MekoAgentList = {
  target: { kind: 'local', url: 'http://127.0.0.1:7778/mcp' },
  agents: [{ name: 'claude', title: 'Claude Code', state: 'connected', url: 'http://127.0.0.1:7778/mcp' }],
};

/** A fetch that answers `status` with `body`, recording what it was asked. */
function fakeFetch(status: number, body: unknown) {
  const asked: { url: string; auth: string }[] = [];
  const fetchImpl = async (url: string, init: { headers: Record<string, string> }) => {
    asked.push({ url, auth: init.headers.Authorization });
    return { ok: status >= 200 && status < 300, status, json: async () => body };
  };
  return { asked, fetchImpl };
}

describe("Meko's list of agents: reading it", () => {
  it("asks the local Meko's API listener with its client token, and reads Meko's envelope", async () => {
    const f = fakeFetch(200, {
      data: {
        target: LIST.target,
        agents: [
          { name: 'claude', title: 'Claude Code', command: '~/.local/bin/claude', state: 'connected', url: 'http://127.0.0.1:7778/mcp', notes: [] },
          { name: 'kiro', title: 'Kiro CLI', state: 'none', url: null, notes: [] },
          { name: 'codex', state: 'sideways' },
        ],
      },
    });
    expect(await readMekoAgents(LOCAL, f.fetchImpl)).toEqual({
      kind: 'list',
      list: {
        target: LIST.target,
        agents: [LIST.agents[0], { name: 'kiro', title: 'Kiro CLI', state: 'none', url: null }],
      },
    });
    expect(f.asked).toEqual([{ url: 'http://127.0.0.1:7778/api/v1/agents', auth: 'Bearer test-token' }]);
  });

  it.each([
    ['a Meko too old to list agents (404)', LOCAL, fakeFetch(404, { error: { code: 'NOT_FOUND' } }).fetchImpl, 'old'],
    ['an answer that is not a list', LOCAL, fakeFetch(200, { data: { agents: 'none' } }).fetchImpl, 'failed'],
    ['a refusal', LOCAL, fakeFetch(401, {}).fetchImpl, 'failed'],
    ['no Meko running here', null, fakeFetch(200, { data: LIST }).fetchImpl, 'failed'],
    ['a Meko that is down', LOCAL, async () => Promise.reject(new Error('connect ECONNREFUSED')), 'failed'],
  ])('reads %s as %s', async (_, local, fetchImpl, kind) => {
    expect((await readMekoAgents(local, fetchImpl)).kind).toBe(kind);
  });

  it('leaves out a target it can\'t read', () => {
    expect(parseMekoAgents({ target: { kind: 'elsewhere', url: 'x' }, agents: [] })).toBe(null);
  });
});

describe("Meko's list of agents: what Kode gates on", () => {
  const setup = (db = openDb(':memory:')) => {
    const r: { value: MekoAgentsRead } = { value: { kind: 'list', list: LIST } };
    let changes = 0;
    const agents = new MekoAgents({ db, read: async () => r.value, changed: () => changes++, log: silentLogger });
    return { db, r, agents, changes: () => changes };
  };

  it('gates nothing before a list was ever read, even while Meko is down', async () => {
    const s = setup();
    expect([s.agents.current(), s.agents.known()]).toEqual([null, 'unread']);
    s.r.value = { kind: 'failed', error: 'down' };
    await s.agents.refresh();
    expect([s.agents.current(), s.agents.known(), s.changes()]).toEqual([null, 'unread', 0]);
  });

  it("keeps the last list read in state.db, for when Meko doesn't answer, even after a restart", async () => {
    const s = setup();
    await s.agents.refresh();
    expect([s.agents.current(), s.agents.known(), s.changes()]).toEqual([LIST, 'read', 1]);
    // The same list again changes nothing.
    await s.agents.refresh();
    expect(s.changes()).toBe(1);
    const again = setup(s.db);
    again.r.value = { kind: 'failed', error: 'down' };
    await again.agents.refresh();
    expect(again.agents.current()).toEqual(LIST);
  });

  it('reads again after a read under way, so a refresh after Connect never gets the list from before it', async () => {
    const db = openDb(':memory:');
    const answers: MekoAgentsRead[] = [];
    const releases: (() => void)[] = [];
    let reads = 0;
    const agents = new MekoAgents({
      db, changed: () => undefined, log: silentLogger,
      read: async () => {
        const n = reads++;
        await new Promise<void>((r) => releases.push(r));
        return answers[n];
      },
    });
    const connected = { ...LIST, agents: [{ ...LIST.agents[0], name: 'kiro' }] };
    answers.push({ kind: 'list', list: LIST }, { kind: 'list', list: connected });
    const before = agents.refresh();
    await Promise.resolve();
    // Asked while the first read runs (Meko connected Kiro meanwhile): both share one read after it.
    const after = agents.refresh();
    expect(agents.refresh()).toBe(after);
    releases[0]();
    await before;
    expect(agents.current()).toEqual(LIST);
    await new Promise((r) => setTimeout(r, 0));
    releases[1]();
    await after;
    expect([agents.current(), reads]).toEqual([connected, 2]);
  });

  it('gates nothing once Meko answers 404, and forgets the list it kept', async () => {
    const s = setup();
    await s.agents.refresh();
    s.r.value = { kind: 'old' };
    await s.agents.refresh();
    expect([s.agents.current(), s.agents.known(), s.changes()]).toEqual([null, 'old', 2]);
    expect(setup(s.db).agents.current()).toBe(null);
  });
});

describe('Connecting Kiro and OpenCode to Meko by itself', () => {
  const status = (id: HarnessStatus['id'], state: HarnessMekoState | null, o: Partial<HarnessStatus> = {}): Omit<HarnessStatus, 'autoConnect'> => ({
    id, enabled: true, version: '3.0.0', minVersion: '2.32.0', available: false, checked: true, isDefault: false, updating: false,
    meko: state ? { state, url: null } : null, ...o,
  });

  it.each([
    ['Kiro with no meko server', true, status('kiro', 'none')],
    ['OpenCode with a stale one', true, status('opencode', 'stale')],
    ['Claude Code, whose Meko setup is the user\'s', false, status('claude', 'none')],
    ['one pointing elsewhere, which a connect would replace', false, status('kiro', 'elsewhere')],
    ['one already connected', false, status('kiro', 'connected')],
    ["one Meko didn't find", false, status('kiro', 'missing')],
    ['any while nothing is gated', false, status('kiro', null)],
    ['one turned off in Settings', false, status('kiro', 'none', { enabled: false })],
    ['one too old for sessions', false, status('kiro', 'none', { version: '2.0.0' })],
    ['one whose version was never read', false, status('kiro', 'none', { version: null })],
  ])('%s: due %s', (_, due, h) => {
    expect(new MekoAutoConnect(openDb(':memory:')).due(h)).toBe(due);
  });

  it('lets one page connect an agent once, ever, even after a restart', () => {
    const db = openDb(':memory:');
    const auto = new MekoAutoConnect(db);
    expect([auto.claim(status('kiro', 'none')), auto.claim(status('kiro', 'none'))]).toEqual([true, false]);
    // Disconnected on purpose later (none again), or after a restart: never automatic again.
    const after = new MekoAutoConnect(db);
    expect([after.due(status('kiro', 'none')), after.claim(status('kiro', 'stale'))]).toEqual([false, false]);
    // Each agent on its own; nothing claimed while it isn't due.
    expect([after.claim(status('opencode', 'connected')), after.claim(status('opencode', 'none'))]).toEqual([false, true]);
  });
});

describe("An agent's meko MCP server as a gap", () => {
  it.each([
    ['nothing gated', null, []],
    ['connected', { state: 'connected' as const, url: 'http://127.0.0.1:7778/mcp' }, []],
    ['none', { state: 'none' as const, url: null }, ['Kiro has no meko MCP server.']],
    ['elsewhere', { state: 'elsewhere' as const, url: 'https://user:secret@meko.example.com/mcp' }, ["Kiro's meko MCP server points at meko.example.com, not 127.0.0.1:7778."]],
    ['stale', { state: 'stale' as const, url: 'http://127.0.0.1:7778/mcp' }, ["Kiro's meko MCP server needs reconnecting to Meko."]],
    ['missing', { state: 'missing' as const, url: null }, ["Meko didn't find Kiro on this computer."]],
  ])('%s', (_, meko, gaps) => {
    expect(mekoMcpGaps('Kiro', meko, 'http://127.0.0.1:7778/mcp')).toEqual(gaps);
  });
});
