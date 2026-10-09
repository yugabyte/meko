import { execFileSync } from 'node:child_process';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ServerEvent } from '../../src/shared/types.js';
import type { MekoDatapackRow } from '../../src/server/meko.js';
import { makeEnv, type ApiCall, type TestEnv } from '../helpers.js';

const OWN: MekoDatapackRow = {
  id: 'dp-own', name: 'adlc/proj/demo-repo/mine', grant: 'owner', createdAt: null, ownerName: 'Me', ownerEmail: 'me@example.com',
  description: null, ownerUserId: 'u-self',
};

function shared(id: string, name: string, grant = 'contributor', description: string | null = null): MekoDatapackRow {
  return { id, name, grant, createdAt: null, ownerName: 'Alice Ng', ownerEmail: 'alice@example.com', description, ownerUserId: 'u-alice' };
}

function incoming(inviteId: string, datapackId: string, name: string, grant = 'contributor') {
  return { invite_id: inviteId, owner_user_id: 'u-alice', owner_name: 'Alice Ng', owner_email: 'alice@example.com', grant, datapack_id: datapackId, datapack_name: name };
}

describe('datapacks shared with us', () => {
  let env: TestEnv;
  let rows: MekoDatapackRow[];
  let waiting: ReturnType<typeof incoming>[];
  let events: Extract<ServerEvent, { type: 'invite' }>[];
  let actionStatus: number;

  beforeEach(async () => {
    env = await makeEnv({ start: false });
    rows = [OWN];
    waiting = [];
    events = [];
    actionStatus = 200;
    (env.meko as { listDatapacks: unknown }).listDatapacks = async () => rows;
    env.core.on('notify', (e: ServerEvent) => {
      if (e.type === 'invite') events.push(e);
    });
  });
  afterEach(async () => {
    await env.cleanup();
  });

  /** Meko answers its share routes: it lists the invites waiting and acts on them. */
  async function apiMode() {
    env.mekoApi.handle((c: ApiCall) => {
      if (c.path === '/datapacks/incoming-shares') return { body: { data: waiting } };
      if (c.path === '/datapacks/invite-action') {
        return actionStatus === 200
          ? { body: { data: { status: 'accepted' } } }
          : { status: actionStatus, body: { error: { code: 'VALIDATION_ERROR', message: 'invite not found or not in pending status' } } };
      }
      return undefined;
    });
  }

  const pending = () => env.core.invites.pending().map((i) => [i.datapackId, i.state, i.inviteId]);

  it('records shares that already exist on the first sync without telling, then tells once about new ones', async () => {
    rows = [OWN, shared('dp-old', 'adlc/proj/demo-repo/old')];
    await env.core.invites.sync();
    expect(pending()).toEqual([]);
    expect(events).toEqual([]);

    rows = [...rows, shared('dp-new', 'adlc/proj/demo-repo/new-work', 'contributor', 'AgentK Local project "new-work" · repo git@github.com:corp/demo-repo.git')];
    await env.core.invites.sync();
    expect(pending()).toEqual([['dp-new', 'shared', null]]);
    expect(env.core.invites.pending()[0]).toMatchObject({ grant: 'contributor', ownerName: 'Alice Ng', repoRemote: 'git@github.com:corp/demo-repo.git' });
    expect(events).toEqual([{ type: 'invite', datapackId: 'dp-new', title: 'Project shared with you', body: 'Alice Ng shared adlc/proj/demo-repo/new-work with you as contributor.' }]);

    await env.core.invites.sync();
    expect(events).toHaveLength(1);
    expect(env.core.state().invites.map((i) => i.datapackId)).toEqual(['dp-new']);
  });

  it('ignores datapacks we own, other names, and ones a project here uses', async () => {
    await env.core.invites.sync(); // baseline
    await env.core.projects.create({ repoPath: env.repo, name: 'used', datapack: { mode: 'existing', id: 'dp-used', name: 'adlc/proj/demo-repo/used' } });
    rows = [OWN, shared('dp-kb', 'team/kb'), shared('dp-used', 'adlc/proj/demo-repo/used')];
    await env.core.invites.sync();
    expect(pending()).toEqual([]);
    expect(events).toEqual([]);
  });

  it('shows invites waiting in Meko, hides them with Later, and drops ones revoked there', async () => {
    await env.core.invites.sync(); // baseline
    await apiMode();
    waiting = [incoming('inv-1', 'dp-a', 'adlc/proj/demo-repo/a'), incoming('inv-2', 'dp-kb', 'team/kb')];
    await env.core.invites.sync();
    expect(pending()).toEqual([['dp-a', 'invited', 'inv-1']]);
    expect(events.map((e) => [e.datapackId, e.title, e.body])).toEqual([['dp-a', 'Project invite', 'Alice Ng invited you to adlc/proj/demo-repo/a as contributor.']]);

    env.core.invites.later('dp-a');
    expect(pending()).toEqual([]);
    expect(env.core.invites.open().map((i) => [i.datapackId, i.state])).toEqual([['dp-a', 'later']]);
    await env.core.invites.sync();
    expect(env.core.invites.open().map((i) => i.state)).toEqual(['later']);

    waiting = [];
    await env.core.invites.sync();
    expect(env.core.invites.open()).toEqual([]);
    expect(events).toHaveLength(1);
  });

  it('turns an invite accepted in Meko into a share without telling again', async () => {
    await env.core.invites.sync();
    await apiMode();
    waiting = [incoming('inv-1', 'dp-a', 'adlc/proj/demo-repo/a')];
    await env.core.invites.sync();
    waiting = [];
    rows = [OWN, shared('dp-a', 'adlc/proj/demo-repo/a')];
    await env.core.invites.sync();
    expect(pending()).toEqual([['dp-a', 'shared', null]]);
    expect(events).toHaveLength(1);
  });

  it('accepts the invite in Meko, then creates the project, and never offers it again', async () => {
    await env.core.invites.sync();
    await apiMode();
    waiting = [incoming('inv-1', 'dp-a', 'adlc/proj/demo-repo/a')];
    await env.core.invites.sync();

    const p = await env.core.projects.create({
      repoPath: env.repo, name: 'a', datapack: { mode: 'existing', id: 'dp-a', name: 'adlc/proj/demo-repo/a', inviteId: 'inv-1' },
    });
    const action = env.mekoApi.calls.find((c) => c.path === '/datapacks/invite-action');
    expect(action?.body).toEqual({ invite_id: 'inv-1', user_action: 'accept', recipient_user_id: 'u-self' });
    expect(p.datapacks[0]).toMatchObject({ id: 'dp-a', role: 'dedicated' });
    expect(env.core.invites.open()).toEqual([]);

    // Accepted, then the project deleted: it stays done with.
    waiting = [];
    rows = [OWN, shared('dp-a', 'adlc/proj/demo-repo/a')];
    await env.core.projects.delete(p.id);
    await env.core.invites.sync();
    expect(env.core.invites.open()).toEqual([]);
    expect(events).toHaveLength(1);
  });

  it('writes nothing when Meko refuses to accept the invite', async () => {
    await env.core.invites.sync();
    await apiMode();
    waiting = [incoming('inv-1', 'dp-a', 'adlc/proj/demo-repo/a')];
    await env.core.invites.sync();
    actionStatus = 400;
    await expect(env.core.projects.create({
      repoPath: env.repo, name: 'a', datapack: { mode: 'existing', id: 'dp-a', name: 'adlc/proj/demo-repo/a', inviteId: 'inv-1' },
    })).rejects.toMatchObject({ status: 400, message: expect.stringMatching(/Meko couldn't accept the invite: invite not found/) });
    expect(env.core.state().projects).toEqual([]);
    expect(pending()).toEqual([['dp-a', 'invited', 'inv-1']]);
  });

  it('creates the project on a second try when the first accepted the invite and then failed', async () => {
    await env.core.invites.sync();
    await apiMode();
    waiting = [incoming('inv-1', 'dp-a', 'adlc/proj/demo-repo/a')];
    await env.core.invites.sync();
    const req = { repoPath: env.repo, name: 'a', datapack: { mode: 'existing' as const, id: 'dp-a', name: 'adlc/proj/demo-repo/a', inviteId: 'inv-1' } };
    // An earlier try got as far as accepting the invite in Meko.
    await env.core.invites.acceptForProject('dp-a', 'inv-1');
    const accepts = () => env.mekoApi.calls.filter((c) => c.path === '/datapacks/invite-action').length;
    expect(accepts()).toBe(1);
    // Meko would now refuse the used invite; the retry doesn't ask again.
    actionStatus = 400;
    const p = await env.core.projects.create(req);
    expect(p.datapacks[0].id).toBe('dp-a');
    expect(accepts()).toBe(1);
  });

  it("doesn't let a sync that read Meko before a change write over it", async () => {
    await env.core.invites.sync();
    await apiMode();
    waiting = [incoming('inv-1', 'dp-a', 'adlc/proj/demo-repo/a')];
    await env.core.invites.sync();
    // A sync starts and is still reading Meko when Later is pressed.
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    let reads = 0;
    (env.meko as { listDatapacks: unknown }).listDatapacks = async () => {
      if (reads++ === 0) await gate;
      return rows;
    };
    const syncing = env.core.invites.sync();
    const joined = env.core.invites.sync();
    expect(joined).toBe(syncing);
    env.core.invites.later('dp-a');
    release();
    await syncing;
    expect(reads).toBe(2);
    expect(env.core.invites.open().map((i) => [i.datapackId, i.state, i.inviteId])).toEqual([['dp-a', 'later', 'inv-1']]);
    expect(events).toHaveLength(1);
  });

  it('reads the datapacks again when a list it was given predates a change', async () => {
    await env.core.invites.sync();
    await apiMode();
    waiting = [incoming('inv-3', 'dp-v', 'adlc/proj/demo-repo/v', 'viewer')];
    await env.core.invites.sync();
    // A list read, then a viewer invite accepted here, then the sync with the older list.
    const stamp = env.core.invites.stamp();
    const stale = [...rows];
    await env.core.invites.accept('dp-v');
    waiting = [];
    rows = [...rows, shared('dp-v', 'adlc/proj/demo-repo/v', 'viewer')];
    await env.core.invites.sync({ rows: stale, stamp });
    // Read afresh, the accepted share is still there (as Later), not dropped and offered again.
    expect(env.core.invites.open().map((i) => [i.datapackId, i.state])).toEqual([['dp-v', 'later']]);
    await env.core.invites.sync();
    expect(events).toHaveLength(1);
  });

  it("doesn't let a read from before an account change bring back the old account's id", async () => {
    await env.core.invites.sync();
    await apiMode();
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    let reads = 0;
    const oldRows = rows;
    (env.meko as { listDatapacks: unknown }).listDatapacks = async () => {
      if (reads++ === 0) {
        await gate;
        return oldRows; // the old account: owner u-self
      }
      return [shared('dp-q', 'adlc/proj/demo-repo/q')]; // the new account owns nothing
    };
    const syncing = env.core.invites.sync();
    await env.core.mekoChanged();
    release();
    await syncing;
    waiting = [incoming('inv-7', 'dp-w', 'adlc/proj/demo-repo/w')];
    await env.core.invites.sync();
    await env.core.projects.create({ repoPath: env.repo, name: 'w', datapack: { mode: 'existing', id: 'dp-w', name: 'adlc/proj/demo-repo/w', inviteId: 'inv-7' } });
    const body = env.mekoApi.calls.filter((c) => c.path === '/datapacks/invite-action').pop()?.body as Record<string, unknown>;
    expect(body.recipient_user_id).toBeUndefined();
  });

  it('forgets the old account when the Meko server or key changes', async () => {
    rows = [OWN, shared('dp-old', 'adlc/proj/demo-repo/old')];
    await env.core.invites.sync(); // baseline for the old account
    rows = [...rows, shared('dp-x', 'adlc/proj/demo-repo/x')];
    await env.core.invites.sync();
    expect(pending()).toEqual([['dp-x', 'shared', null]]);
    await env.core.projects.create({ repoPath: env.repo, name: 'u', datapack: { mode: 'existing', id: 'dp-u', name: 'adlc/proj/demo-repo/u' } });

    // The new account's existing shares are recorded quietly, as after an upgrade.
    events = [];
    rows = [{ ...OWN, ownerUserId: 'u-new' }, shared('dp-y', 'adlc/proj/demo-repo/y')];
    await env.core.mekoChanged();
    expect(env.core.invites.open()).toEqual([]);
    await env.core.invites.sync();
    expect(pending()).toEqual([]);
    expect(events).toEqual([]);
    // It no longer sends the old account's id with an accept.
    await apiMode();
    waiting = [incoming('inv-5', 'dp-z', 'adlc/proj/demo-repo/z')];
    await env.core.invites.sync();
    await env.core.projects.create({ repoPath: env.repo, name: 'z', datapack: { mode: 'existing', id: 'dp-z', name: 'adlc/proj/demo-repo/z', inviteId: 'inv-5' } });
    expect(env.mekoApi.calls.find((c) => c.path === '/datapacks/invite-action')?.body).toMatchObject({ recipient_user_id: 'u-new' });
  });

  it("declines in Meko, and refuses to answer invites without Meko's API URL", async () => {
    await env.core.invites.sync();
    await apiMode();
    waiting = [incoming('inv-1', 'dp-a', 'adlc/proj/demo-repo/a')];
    await env.core.invites.sync();
    await env.core.invites.decline('dp-a');
    expect(env.mekoApi.calls.find((c) => c.path === '/datapacks/invite-action')?.body).toMatchObject({ invite_id: 'inv-1', user_action: 'decline' });
    expect(env.core.invites.open()).toEqual([]);

    waiting = [incoming('inv-2', 'dp-b', 'adlc/proj/demo-repo/b', 'viewer')];
    await env.core.invites.sync();
    env.core.updateConfig((c) => {
      c.meko.url = 'https://meko.example.com/mcp';
    });
    await expect(env.core.invites.decline('dp-b')).rejects.toMatchObject({ status: 409, message: expect.stringMatching(/API URL isn't known/) });
    expect(() => env.core.invites.later('nope')).toThrow(/no longer waiting/);
  });

  it('logs a key the share routes refuse once, not on every sync, and again after it worked', async () => {
    const warn = vi.spyOn(env.core.log, 'warn');
    try {
      const refusals = () => warn.mock.calls.filter((c) => String(c[0]).startsWith("couldn't list the datapack invites"));
      await env.core.invites.sync();
      await env.core.invites.sync();
      expect(env.mekoApi.calls.filter((c) => c.path === '/datapacks/incoming-shares')).toHaveLength(2);
      expect(refusals()).toHaveLength(1);
      expect(String(refusals()[0][1])).toMatch(/invalid or expired token/);
      await apiMode();
      await env.core.invites.sync();
      env.mekoApi.handle(null);
      await env.core.invites.sync();
      expect(refusals()).toHaveLength(2);
    } finally {
      warn.mockRestore();
    }
  });

  it('accepts a viewer invite without making a project of it', async () => {
    await env.core.invites.sync();
    await apiMode();
    waiting = [incoming('inv-3', 'dp-v', 'adlc/proj/demo-repo/v', 'viewer')];
    await env.core.invites.sync();
    await env.core.invites.accept('dp-v');
    expect(env.mekoApi.calls.find((c) => c.path === '/datapacks/invite-action')?.body).toMatchObject({ invite_id: 'inv-3', user_action: 'accept' });
    expect(pending()).toEqual([]);
  });

  it("puts the repo's remote in the description of a datapack it creates", async () => {
    execFileSync('git', ['remote', 'add', 'origin', 'git@github.com:corp/demo-repo.git'], { cwd: env.repo });
    await env.core.projects.create({ repoPath: env.repo, name: 'fresh', datapack: { mode: 'create', name: 'adlc/proj/demo-repo/fresh' } });
    const call = env.meko.calls.find((c) => c.name === 'setDatapackDescription');
    expect(call?.args).toEqual(['dp-adlc-proj-demo-repo-fresh', 'AgentK Local project "fresh" · repo git@github.com:corp/demo-repo.git']);
  });

  it("writes a fork's upstream remote into the description, not the fork", async () => {
    execFileSync('git', ['remote', 'add', 'origin', 'git@github.com:hari90/demo-repo.git'], { cwd: env.repo });
    execFileSync('git', ['remote', 'add', 'upstream', 'git@github.com:corp/demo-repo.git'], { cwd: env.repo });
    await env.core.projects.create({ repoPath: env.repo, name: 'fresh', datapack: { mode: 'create', name: 'adlc/proj/demo-repo/fresh' } });
    expect(env.meko.calls.find((c) => c.name === 'setDatapackDescription')?.args[1]).toBe('AgentK Local project "fresh" · repo git@github.com:corp/demo-repo.git');
    const listed = (await env.core.projects.repos()).repos.find((r) => r.path === env.repo);
    expect(listed).toMatchObject({ remote: 'github.com/hari90/demo-repo', upstream: 'github.com/corp/demo-repo' });
  });

  it("falls back to origin when a fork's upstream is only a local path", async () => {
    execFileSync('git', ['remote', 'add', 'origin', 'git@github.com:hari90/demo-repo.git'], { cwd: env.repo });
    execFileSync('git', ['remote', 'add', 'upstream', '/mnt/mirror/demo-repo.git'], { cwd: env.repo });
    await env.core.projects.create({ repoPath: env.repo, name: 'fresh', datapack: { mode: 'create', name: 'adlc/proj/demo-repo/fresh' } });
    expect(env.meko.calls.find((c) => c.name === 'setDatapackDescription')?.args[1]).toBe('AgentK Local project "fresh" · repo git@github.com:hari90/demo-repo.git');
  });

  it('refuses a read-only datapack as the dedicated one, and asks Meko only when the shares table has no grant', async () => {
    await env.core.invites.sync();
    await apiMode();
    waiting = [incoming('inv-3', 'dp-v', 'adlc/proj/demo-repo/v', 'viewer')];
    await env.core.invites.sync();
    await expect(env.core.projects.create({
      repoPath: env.repo, name: 'v', datapack: { mode: 'existing', id: 'dp-v', name: 'adlc/proj/demo-repo/v', inviteId: 'inv-3' },
    })).rejects.toMatchObject({ status: 400, message: expect.stringMatching(/only read .* \(viewer\)/) });
    expect(env.mekoApi.calls.some((c) => c.path === '/datapacks/invite-action')).toBe(false);
    (env.meko as { describeDatapack: unknown }).describeDatapack = async () => ({ memoryCount: 0, knowledgeCount: 0, grant: 'viewer' });
    await expect(env.core.projects.create({ repoPath: env.repo, name: 'w', datapack: { mode: 'existing', id: 'dp-w', name: 'team/w' } }))
      .rejects.toMatchObject({ status: 400 });
    expect(env.core.state().projects).toEqual([]);
  });

  it("keeps a share accepted here while Meko's list catches up", async () => {
    await env.core.invites.sync();
    await apiMode();
    waiting = [incoming('inv-3', 'dp-v', 'adlc/proj/demo-repo/v', 'viewer')];
    await env.core.invites.sync();
    await env.core.invites.accept('dp-v');
    waiting = [];
    await env.core.invites.sync(); // datapack_list doesn't show it yet
    expect(env.core.invites.open().map((i) => [i.datapackId, i.state])).toEqual([['dp-v', 'later']]);
  });

  it('leaves a local path out of the description', async () => {
    execFileSync('git', ['remote', 'add', 'origin', '/home/alice/src/demo-repo.git'], { cwd: env.repo });
    await env.core.projects.create({ repoPath: env.repo, name: 'fresh', datapack: { mode: 'create', name: 'adlc/proj/demo-repo/fresh' } });
    expect(env.meko.calls.find((c) => c.name === 'setDatapackDescription')?.args[1]).toBe('AgentK Local project "fresh"');
  });

  it('never writes the credentials in a remote URL into the description', async () => {
    execFileSync('git', ['remote', 'add', 'origin', 'https://x-access-token:ghp_secret123@github.com/corp/demo-repo.git'], { cwd: env.repo });
    await env.core.projects.create({ repoPath: env.repo, name: 'fresh', datapack: { mode: 'create', name: 'adlc/proj/demo-repo/fresh' } });
    const description = String(env.meko.calls.find((c) => c.name === 'setDatapackDescription')?.args[1]);
    expect(description).toBe('AgentK Local project "fresh" · repo https://github.com/corp/demo-repo.git');
    expect(description).not.toContain('ghp_secret123');
  });

  it('still creates the project when the description update fails', async () => {
    (env.meko as { setDatapackDescription: unknown }).setDatapackDescription = async () => {
      throw new Error('datapack_update failed');
    };
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'fresh', datapack: { mode: 'create', name: 'adlc/proj/demo-repo/fresh' } });
    expect(p.datapacks[0].name).toBe('adlc/proj/demo-repo/fresh');
  });
});
