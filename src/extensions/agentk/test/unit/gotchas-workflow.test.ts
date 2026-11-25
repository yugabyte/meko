import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';

const RUN = fileURLToPath(new URL('../gotchas/run.sh', import.meta.url));
const hasJq = spawnSync('jq', ['--version']).status === 0;

// gotchas/gotchas.yml runs on GitHub's ubuntu-latest, so its script is tested on Linux only.
describe.skipIf(process.platform !== 'linux' || !hasJq)('the gotchas workflow', () => {
  it('mines into Meko, and refuses what it must, in every scenario of test/gotchas/run.sh', () => {
    const r = spawnSync('bash', [RUN], { encoding: 'utf8', timeout: 120_000 });
    expect(r.stderr + r.stdout).toContain('All gotchas tests passed');
    expect(r.status).toBe(0);
  }, 150_000);
});
