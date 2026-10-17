import { describe, expect, it } from 'vitest';
import type { HarnessId } from '../../src/shared/harnesses.js';
import { ClaudeCode } from '../../src/server/claudecode.js';
import { DEFAULT_CONFIG, type Config } from '../../src/server/config.js';
import { Harnesses } from '../../src/server/harness/index.js';
import { silentLogger } from '../../src/server/log.js';

/** The registry over fake CLIs: `installed` is what each one's --version prints (null: not installed). */
function setup(installed: Record<HarnessId, string | null>, o: { default?: HarnessId; off?: HarnessId[] } = {}) {
  const cfg: Config = structuredClone(DEFAULT_CONFIG);
  cfg.agents.default = o.default ?? 'claude';
  for (const id of o.off ?? []) cfg[id].enabled = false;
  const reads: string[] = [];
  const read = (id: HarnessId) => async () => (reads.push(id), installed[id]);
  const claudeCode = new ClaudeCode({ config: () => cfg, changed: () => undefined, readVersion: read('claude') });
  const harnesses = new Harnesses({ claudeCode, log: silentLogger, changed: () => undefined, kiroVersion: read('kiro') });
  return { cfg, harnesses, installed, reads, readAll: () => Promise.all(harnesses.list().map((h) => h.version(cfg))) };
}

describe('the agent new sessions and Ask AgentK start on', () => {
  it.each([
    ['the default while it is available', { claude: '2.1.300', kiro: '2.32.0' }, { default: 'kiro' as const }, 'kiro'],
    ['the first available one while the default is not installed', { claude: null, kiro: '2.32.0' }, {}, 'kiro'],
    ['the first available one while the default is turned off', { claude: '2.1.300', kiro: '2.32.0' }, { off: ['claude' as const] }, 'kiro'],
    ['the first available one while the default is too old', { claude: '2.1.300', kiro: '2.10.0' }, { default: 'kiro' as const }, 'claude'],
    ['none when no agent is available', { claude: null, kiro: '2.32.0' }, { off: ['kiro' as const] }, null],
  ])('%s', async (_, installed, o, expected) => {
    const s = setup(installed, o);
    await s.readAll();
    expect(s.harnesses.defaultFor(s.cfg)?.id ?? null).toBe(expected);
  });

  it('reads a version again before refusing, and says what to install when nothing is there', async () => {
    const s = setup({ claude: null, kiro: null });
    await s.readAll();
    expect(s.harnesses.status(s.cfg).map((h) => [h.id, h.available, h.checked])).toEqual([['claude', false, true], ['kiro', false, true]]);
    // Installed since the last read: the create finds it without waiting for the next one.
    s.installed.claude = '2.1.300';
    expect((await s.harnesses.pick(s.cfg, undefined)).id).toBe('claude');
    s.installed.claude = null;
    await s.harnesses.of('claude').version(s.cfg, true);
    s.reads.length = 0;
    await expect(s.harnesses.pick(s.cfg, undefined)).rejects.toMatchObject({ status: 409, message: expect.stringMatching(/^No coding agent is available: install Claude Code or Kiro CLI/) });
    expect(s.reads.sort()).toEqual(['claude', 'claude', 'kiro']);
    await expect(s.harnesses.pick(s.cfg, 'kiro')).rejects.toMatchObject({ status: 400, message: expect.stringMatching(/^Kiro isn't available for new sessions: install it/) });
  });
});
