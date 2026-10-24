import { existsSync, mkdirSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';
import type { AskRun, AskRunner } from '../../src/server/askagentk.js';
import { openDb, run } from '../../src/server/db.js';
import { KiroPlanStore, KIRO_PLAN_TURN_EVERY_MS as TURN_MS } from '../../src/server/kiroplan.js';
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
  let gate = Promise.resolve();
  const runner: AskRunner = async (_cmd, args) => {
    runs.push(args);
    await gate;
    return answer;
  };
  let changes = 0;
  const cwd = tempDir('akl-kiro-usage-');
  const kiroHome = tempDir('akl-kiro-home-');
  const deleted: string[] = [];
  const make = (available = true) => new KiroPlanStore({
    db, available: () => available, command: () => '/no/such/kiro-cli', cwd, changed: () => changes++, log: silentLogger, run: runner,
    kiroHome, deleteSession: (_cmd, id) => deleted.push(id),
  });
  /** A session in Kiro's store, as a run in `dir` leaves one. */
  const kiroSession = (id: string, dir: string) => {
    mkdirSync(join(kiroHome, 'sessions', 'cli'), { recursive: true });
    writeFileSync(join(kiroHome, 'sessions', 'cli', `${id}.json`), JSON.stringify({ session_id: id, cwd: dir }));
  };
  /** Holds the runs started from now on until the returned function is called. */
  const hold = () => {
    let open!: () => void;
    gate = new Promise<void>((r) => (open = r));
    return open;
  };
  return { db, cwd, session, kiroSession, runs, deleted, make, hold, setAnswer: (a: AskRun) => (answer = a), changes: () => changes };
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
    // As an inert agent written in the run's folder, never the user's default one.
    expect(t.runs).toEqual([['chat', '--no-interactive', '--output-format', 'stream-json', '--agent', 'agentk-usage', '/usage']]);
    expect(existsSync(join(t.cwd, '.kiro', 'agents', 'agentk-usage.json'))).toBe(true);
    expect(store.current()).toMatchObject({ used: 10, covered: 50, plan: 'Free' });
    expect(t.changes()).toBe(1);
    // Signed out, say: the last reading stays.
    t.setAnswer({ ...reply(''), code: 1 });
    await store.refresh();
    expect(store.current()).toMatchObject({ used: 10 });
    expect(t.make().current()).toMatchObject({ used: 10 });
  });

  it('reads on the first Kiro session only, one read at a time, and settles once the read in flight and its sweep are done', async () => {
    const t = setup();
    const store = t.make();
    t.session('k1', 'kiro');
    const open = t.hold();
    store.sessionCreated();
    store.sessionCreated();
    // What Core.stop waits on: not done while the run is held.
    let done = false;
    const settled = store.settled().then(() => (done = true));
    await new Promise((r) => setTimeout(r, 50));
    expect(done).toBe(false);
    open();
    await settled;
    expect(t.runs).toHaveLength(1);
    expect(t.deleted).toEqual(['u-1']);
    t.session('k2', 'kiro');
    store.sessionCreated();
    await store.refresh();
    expect(t.runs).toHaveLength(2);
  });

  it("deletes every session Kiro kept in the run's folder, the one it named and any a cut-off run left", async () => {
    const t = setup();
    t.session('k', 'kiro');
    t.kiroSession('left-by-timeout', t.cwd);
    t.kiroSession('a-worktree-session', '/w');
    await t.make().refresh();
    expect(t.deleted.sort()).toEqual(['left-by-timeout', 'u-1']);
  });

  it("never rejects, even when the database can't be read", async () => {
    const t = setup();
    const store = t.make();
    t.db.close();
    await expect(store.refresh()).resolves.toBeUndefined();
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

  it('re-reads on a turn at most once per window', async () => {
    const t = setup();
    const store = t.make();
    t.session('k', 'kiro');
    const t0 = 1_000_000;
    // The first turn reads (nothing read yet).
    await store.refreshIfStale(TURN_MS, t0);
    expect(t.runs).toHaveLength(1);
    // A turn inside the window does not read again.
    await store.refreshIfStale(TURN_MS, t0 + TURN_MS - 1);
    expect(t.runs).toHaveLength(1);
    // A turn past the window reads once more.
    await store.refreshIfStale(TURN_MS, t0 + TURN_MS);
    expect(t.runs).toHaveLength(2);
  });

  it('collapses turns while a read is in flight into the one read', async () => {
    const t = setup();
    const store = t.make();
    t.session('k', 'kiro');
    const open = t.hold();
    const first = store.refreshIfStale(TURN_MS, 1_000_000);
    // A second turn while the first read is held, well past the window: still the same read.
    const second = store.refreshIfStale(TURN_MS, 1_000_000 + 10 * TURN_MS);
    open();
    await Promise.all([first, second]);
    expect(t.runs).toHaveLength(1);
  });

  it('never reads on a turn while Kiro is unavailable or unused', async () => {
    const t = setup();
    // Unused: no Kiro session yet.
    await t.make().refreshIfStale(TURN_MS, 1_000_000);
    const t2 = setup();
    t2.session('k', 'kiro');
    // Unavailable.
    await t2.make(false).refreshIfStale(TURN_MS, 1_000_000);
    expect(t.runs).toEqual([]);
    expect(t2.runs).toEqual([]);
  });
});
