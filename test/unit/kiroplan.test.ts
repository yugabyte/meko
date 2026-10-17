import { describe, expect, it } from 'vitest';
import type { AskRun, AskRunner } from '../../src/server/askagentk.js';
import { openDb, run } from '../../src/server/db.js';
import { KiroPlanStore } from '../../src/server/kiroplan.js';
import { silentLogger } from '../../src/server/log.js';
import { tempDir } from '../helpers.js';

const usage = (text: string, sessionId = 'u-1') =>
  JSON.stringify({ type: 'runFinished', data: { sessionId, status: 'success', finalText: text } });
const reply = (stdout: string): AskRun => ({ stdout, stderr: '', code: 0, spawnError: null, timedOut: false, aborted: false });

function setup() {
  const db = openDb(':memory:');
  run(db, `INSERT INTO projects(id, name, repo_path, default_branch, created_at) VALUES ('p', 'P', '/r', 'main', 1)`);
  const session = (id: string, harness: string) =>
    run(db, `INSERT INTO sessions(id, project_id, slug, name, harness, claude_session_id, branch, base_branch, worktree_path, tmux_name, hook_token,
             desired, status, stage, created_at) VALUES (?, 'p', ?, ?, ?, 'c', ?, 'main', '/w', 't', ?, 'archived', 'idle', 'code', 1)`,
      id, id, id, harness, `ak/${id}`, `tok-${id}`);
  const runs: string[][] = [];
  let answer = reply(usage('Estimated Usage | resets on 2099-01-01 | KIRO FREE\nCredits (10 of 50 covered in plan), 20%'));
  const runner: AskRunner = async (_cmd, args) => {
    runs.push(args);
    return answer;
  };
  let changes = 0;
  const cwd = tempDir('akl-kiro-usage-');
  const make = (available = true) => new KiroPlanStore({
    db, available: () => available, command: () => '/no/such/kiro-cli', cwd, changed: () => changes++, log: silentLogger, run: runner,
  });
  return { db, session, runs, make, setAnswer: (a: AskRun) => (answer = a), changes: () => changes };
}

describe("Kiro's plan reading", () => {
  it('reads only once Kiro has been used in AgentK, keeps the last reading through a failure and a restart', async () => {
    const t = setup();
    const store = t.make();
    t.session('c', 'claude');
    await store.refresh();
    expect(t.runs).toEqual([]);
    t.session('k', 'kiro');
    await store.refresh();
    expect(t.runs).toEqual([['chat', '--no-interactive', '--output-format', 'stream-json', '/usage']]);
    expect(store.current()).toMatchObject({ used: 10, covered: 50, plan: 'Free' });
    expect(t.changes()).toBe(1);
    // Signed out, say: the last reading stays.
    t.setAnswer({ ...reply(''), code: 1 });
    await store.refresh();
    expect(store.current()).toMatchObject({ used: 10 });
    expect(t.make().current()).toMatchObject({ used: 10 });
  });

  it('reads on the first Kiro session only, one read at a time', async () => {
    const t = setup();
    const store = t.make();
    t.session('k1', 'kiro');
    store.sessionCreated();
    store.sessionCreated();
    await store.refresh();
    t.session('k2', 'kiro');
    store.sessionCreated();
    await store.refresh();
    expect(t.runs).toHaveLength(2);
  });

  it.each([
    ['Kiro is unavailable', false, '2099-01-01'],
    ['its period has reset', true, '2020-01-01'],
  ])('shows no reading when %s', async (_, available, resets) => {
    const t = setup();
    t.session('k', 'kiro');
    t.setAnswer(reply(usage(`Estimated Usage | resets on ${resets} | KIRO FREE\nCredits (10 of 50 covered in plan)`)));
    await t.make().refresh();
    expect(t.make(available).current()).toBeNull();
  });
});
