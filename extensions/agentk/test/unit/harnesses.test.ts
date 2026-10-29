import { describe, expect, it } from 'vitest';
import type { HarnessId } from '../../src/shared/harnesses.js';
import { ClaudeCode } from '../../src/server/claudecode.js';
import { DEFAULT_CONFIG, type Config } from '../../src/server/config.js';
import { getPaths } from '../../src/server/paths.js';
import { Harnesses } from '../../src/server/harness/index.js';
import { silentLogger } from '../../src/server/log.js';
import type { MekoAgentList, MekoAgentState } from '../../src/server/mekoagents.js';

/** The registry over fake CLIs: `installed` is what each one's --version prints (null or left out: not installed). */
function setup(
  installed: Partial<Record<HarnessId, string | null>>,
  o: { default?: HarnessId; off?: HarnessId[]; meko?: () => MekoAgentList | null; refreshMeko?: () => Promise<void> } = {},
) {
  const cfg: Config = structuredClone(DEFAULT_CONFIG);
  cfg.agents.default = o.default ?? 'claude';
  for (const id of o.off ?? []) cfg[id].enabled = false;
  const read = (id: HarnessId) => async () => installed[id] ?? null;
  const claudeCode = new ClaudeCode({ config: () => cfg, changed: () => undefined, readVersion: read('claude') });
  const harnesses = new Harnesses({
    claudeCode, paths: getPaths('/nonexistent/agentk'), log: silentLogger, changed: () => undefined, kiroVersion: read('kiro'), openCodeVersion: read('opencode'),
    meko: o.meko, refreshMeko: o.refreshMeko,
  });
  return { cfg, harnesses, installed, readAll: () => Promise.all(harnesses.list().map((h) => h.version(cfg))) };
}

describe('the agent new sessions and Ask AgentK start on', () => {
  it.each([
    ['the default while it is available', { claude: '2.1.300', kiro: '2.32.0' }, { default: 'kiro' as const }, 'kiro'],
    ['the first available one while the default is not installed', { claude: null, kiro: '2.32.0' }, {}, 'kiro'],
    ['the first available one while the default is turned off', { claude: '2.1.300', kiro: '2.32.0' }, { off: ['claude' as const] }, 'kiro'],
    ['the first available one while the default is too old', { claude: '2.1.300', kiro: '2.10.0' }, { default: 'kiro' as const }, 'claude'],
    ['OpenCode before Kiro, in their order, while the default is not installed', { opencode: '2.1.4', kiro: '2.32.0' }, {}, 'opencode'],
    ['Kiro while OpenCode is too old', { opencode: '1.19.4', kiro: '2.32.0' }, {}, 'kiro'],
    ['none when no agent is available', { claude: null, kiro: '2.32.0' }, { off: ['kiro' as const] }, null],
  ])('%s', async (_, installed, o, expected) => {
    const s = setup(installed, o);
    await s.readAll();
    expect(s.harnesses.defaultFor(s.cfg)?.id ?? null).toBe(expected);
  });

  it('reads a version again before refusing, and says what to install when nothing is there', async () => {
    const s = setup({ claude: null, kiro: null });
    await s.readAll();
    expect(s.harnesses.status(s.cfg).map((h) => [h.id, h.available, h.checked])).toEqual([['claude', false, true], ['opencode', false, true], ['kiro', false, true]]);
    // Installed since the last read: the create finds it without waiting for the next one.
    s.installed.claude = '2.1.300';
    expect((await s.harnesses.pick(s.cfg, undefined)).id).toBe('claude');
    s.installed.claude = null;
    await s.harnesses.of('claude').version(s.cfg, true);
    await expect(s.harnesses.pick(s.cfg, undefined)).rejects.toMatchObject({ status: 409, message: expect.stringMatching(/^No coding agent is available: install Claude Code, OpenCode or Kiro CLI/) });
    await expect(s.harnesses.pick(s.cfg, 'kiro')).rejects.toMatchObject({ status: 400, message: expect.stringMatching(/^Kiro isn't available for new sessions: install it/) });
  });
});

describe('new sessions start only on agents Meko has connected', () => {
  /** Meko's list with these agents in these states (an agent left out: Meko didn't find it). */
  const list = (states: Partial<Record<HarnessId, MekoAgentState>>): MekoAgentList => ({
    target: { kind: 'local', url: 'http://127.0.0.1:7778/mcp' },
    agents: Object.entries(states).map(([name, state]) => ({ name, title: name, state, url: state === 'none' ? null : 'http://127.0.0.1:7778/mcp' })),
  });
  const installed = { claude: '2.1.300', opencode: '2.1.4', kiro: '2.32.0' };

  it.each([
    ['not connected', 'none' as const],
    ['needing reconnecting', 'stale' as const],
    ['connected elsewhere', 'elsewhere' as const],
  ])('refuses one %s, naming the fix, after reading Meko\'s list again', async (_, kiro) => {
    let reads = 0;
    const s = setup(installed, { meko: () => list({ claude: 'connected', kiro }), refreshMeko: async () => void reads++ });
    await s.readAll();
    expect(s.harnesses.status(s.cfg).find((h) => h.id === 'kiro')).toMatchObject({ available: false, meko: { state: kiro } });
    await expect(s.harnesses.pick(s.cfg, 'kiro')).rejects.toMatchObject({
      status: 400, message: "Kiro isn't connected to Meko: connect it in Settings › Harness, or run meko agent connect kiro.",
    });
    expect(reads).toBe(1);
    // A connected one starts without asking Meko again.
    expect((await s.harnesses.pick(s.cfg, 'claude')).id).toBe('claude');
    expect(reads).toBe(1);
  });

  it("offers one Meko didn't find: Meko never sees AgentK's own command for it", async () => {
    const s = setup(installed, { meko: () => list({ claude: 'connected' }) });
    await s.readAll();
    expect(s.harnesses.status(s.cfg).find((h) => h.id === 'kiro')).toMatchObject({ available: true, meko: { state: 'missing' } });
    expect((await s.harnesses.pick(s.cfg, 'kiro')).id).toBe('kiro');
  });

  it.each([
    ['the one asked for', 'kiro' as const],
    ['the default', undefined],
  ])("starts on %s once Meko's list, read again, has it connected (meko agent connect in a terminal)", async (_, asked) => {
    const states: { value: Partial<Record<HarnessId, MekoAgentState>> } = { value: { claude: 'none', kiro: 'none' } };
    const s = setup(installed, { default: 'kiro', off: ['opencode'], meko: () => list(states.value), refreshMeko: async () => void (states.value = { claude: 'none', kiro: 'connected' }) });
    await s.readAll();
    expect((await s.harnesses.pick(s.cfg, asked)).id).toBe('kiro');
  });

  it('starts on the first connected agent while the default is not, and says what to connect when none is', async () => {
    const states: { value: Partial<Record<HarnessId, MekoAgentState>> } = { value: { claude: 'none', opencode: 'stale', kiro: 'connected' } };
    const s = setup(installed, { meko: () => list(states.value) });
    await s.readAll();
    expect(s.harnesses.defaultFor(s.cfg)?.id).toBe('kiro');
    states.value = { claude: 'none', opencode: 'none', kiro: 'elsewhere' };
    await expect(s.harnesses.pick(s.cfg, undefined)).rejects.toMatchObject({
      status: 409,
      message: expect.stringMatching(/^No coding agent is available: .* Claude Code isn't connected to Meko: .* OpenCode isn't connected .* Kiro isn't connected to Meko: connect it in Settings › Harness, or run meko agent connect kiro\.$/),
    });
  });

  it('gates nothing without a list (never read, or a Meko too old to give one)', async () => {
    const s = setup(installed, { meko: () => null });
    await s.readAll();
    expect(s.harnesses.status(s.cfg).map((h) => [h.id, h.available, h.meko])).toEqual([['claude', true, null], ['opencode', true, null], ['kiro', true, null]]);
    expect((await s.harnesses.pick(s.cfg, 'kiro')).id).toBe('kiro');
  });

  it.each([
    ['connected', 'connected' as const, true],
    ['elsewhere', 'elsewhere' as const, true],
    ['none', 'none' as const, false],
  ])("counts OpenCode's meko server Meko reports (%s) as its own, which AgentK then leaves alone", (_, state, own) => {
    const s = setup(installed, { meko: () => list({ opencode: state }) });
    expect(s.harnesses.openCodeOwnMeko()).toBe(own);
  });
});
