import { execFileSync, spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';

const MOD = fileURLToPath(new URL('../../src/mod/agentk', import.meta.url));

/** Claude Code, when it's installed here (CI has none): the mod is checked by Claude Code itself. */
function claude(): string | null {
  try {
    execFileSync('claude', ['--version'], { stdio: 'ignore', timeout: 20_000 });
    return 'claude';
  } catch {
    return null;
  }
}

const bin = claude();

describe.skipIf(!bin)("AgentK's mod, checked by Claude Code", () => {
  const run = (...args: string[]) => spawnSync(bin!, args, { encoding: 'utf8', timeout: 120_000 });

  it('validates: its hooks and every call on $ are ones the engine takes', () => {
    const r = run('plugin', 'validate', MOD);
    expect(r.status, r.stdout + r.stderr).toBe(0);
    expect(r.stdout).toMatch(/hooks: session\.start, session\.measure/);
  });

  it('passes its own tests (src/mod/agentk/tests)', () => {
    const r = run('plugin', 'test', MOD);
    expect(r.status, r.stdout + r.stderr).toBe(0);
  }, 150_000);
});
