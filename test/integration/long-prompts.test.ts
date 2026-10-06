import { readFileSync } from 'node:fs';
import { afterEach, describe, expect, it } from 'vitest';
import { makeEnv, waitFor, type TestEnv } from '../helpers.js';

describe('typed prompts (real tmux + git, fake claude)', () => {
  let env: TestEnv;
  afterEach(async () => {
    await env.cleanup();
  });

  // A terminal read line by line caps a line (1,024 bytes on macOS, 4,096 on Linux); Claude's TUI,
  // like the fake, reads it raw. A typed Design prompt runs past macOS's cap.
  it('arrive whole however long a line is', async () => {
    env = await makeEnv();
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'existing', id: 'dp-1', name: 'team/demo' } });
    const s = await env.core.sessions.create(p.id, { name: 'Long', prompt: 'do it', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'first turn');
    await env.core.sessions.submit(s.id, `${'x'.repeat(5000)} LONG_END`);
    const t = await waitFor(() => readFileSync(env.core.sessions.row(s.id).transcript_path!, 'utf8'), (x) => x.includes('LONG_END'), 10000, 'the long prompt');
    expect(t).toContain('x'.repeat(5000));
  }, 30_000);
});
