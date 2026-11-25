import { existsSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { makeEnv, tempDir, waitFor } from '../helpers.js';

// makeEnv's cleanup must not remove the test home while something still writes into it.
describe('test env cleanup', () => {
  afterEach(() => {
    delete process.env.FAKE_CAPTURE_MS;
    delete process.env.FAKE_CAPTURE_LOG;
  });

  it('waits for a transcript capture still running', async () => {
    const out = tempDir('akl-capture-');
    try {
      process.env.FAKE_CAPTURE_MS = '700';
      process.env.FAKE_CAPTURE_LOG = join(out, 'capture.jsonl');
      const env = await makeEnv();
      try {
        const p = await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'create', name: 'agent-k-local/test/demo' } });
        const s = await env.core.sessions.create(p.id, { name: 'Slow capture', prompt: 'hi', kind: 'quick' });
        // The turn's end (Stop) starts a capture, which takes 700 ms.
        await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'first turn');
      } finally {
        await env.cleanup();
      }
      expect(existsSync(process.env.FAKE_CAPTURE_LOG)).toBe(true);
      expect(existsSync(env.home)).toBe(false);
    } finally {
      rmSync(out, { recursive: true, force: true });
    }
  });
});
