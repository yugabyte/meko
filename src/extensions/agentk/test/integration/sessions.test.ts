import { execFileSync } from 'node:child_process';
import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { TEST_MEKO_URL, makeEnv, pinFiles, pinnedDatapack, readJsonl, sessionPinFile, waitFor, type TestEnv } from '../helpers.js';
import { AGENTK_AGENT_ID, legacyAgentId, sessionAgentId } from '../../src/server/meko.js';
import { PIN_KEEP_MS } from '../../src/server/projects.js';
import { postHook } from '../hook-client.js';
import { all, get, run } from '../../src/server/db.js';
import { usageOverview } from '../../src/server/usage.js';
import { sleep } from '../../src/server/hostclient.js';
import { transcriptPathFor } from '../../src/server/claude.js';
import { socketId, startHookServer } from '../../src/server/hooks.js';
import { MISSED_START_MS } from '../../src/server/sessions.js';
import { stagedDir } from '../../src/server/pastes.js';
import type { CreateSessionRequest } from '../../src/shared/types.js';

function argvLog(env: TestEnv): { args: string[]; env: Record<string, string> }[] {
  const f = join(env.fakeDir, 'argv.jsonl');
  return readJsonl(f);
}

/** HEAD isn't on a branch (an idle worktree). */
function detached(dir: string): boolean {
  try {
    execFileSync('git', ['symbolic-ref', '-q', 'HEAD'], { cwd: dir, stdio: 'ignore' });
    return false;
  } catch {
    return true;
  }
}

/** What the fake Claude's SessionStart hooks printed, per start. */
function sessionStarts(env: TestEnv): { sessionId: string; source: string; context: string }[] {
  const f = join(env.fakeDir, 'session-start.jsonl');
  return readJsonl(f);
}

function captureLog(env: TestEnv): { args: string[]; input: { transcript_path: string }; env: Record<string, string> }[] {
  const f = join(env.fakeDir, 'capture.jsonl');
  return readJsonl(f);
}

describe('projects and sessions (real session host + git, fake claude)', () => {
  let env: TestEnv;
  beforeEach(async () => {
    env = await makeEnv();
  });
  afterEach(async () => {
    await env.cleanup();
  });

  const events = (id: string) => all<{ kind: string; text: string | null }>(env.core.db, 'SELECT kind, text FROM session_events WHERE session_id = ? ORDER BY id', id);

  async function newProject() {
    return env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'create', name: 'agent-k-local/test/demo' } });
  }

  it('creates projects with a dedicated datapack each, pinned only once a session starts, several sharing a repo with sessions and worktrees of their own', async () => {
    const first = await newProject();
    expect(first.id).toBe('demo-repo');
    expect(first.datapacks).toEqual([{ id: 'dp-agent-k-local-test-demo', name: 'agent-k-local/test/demo', role: 'dedicated' }]);
    const second = await env.core.projects.create({
      repoPath: env.repo, name: 'Release tooling', datapack: { mode: 'create', name: 'agent-k-local/test/release' },
    });
    expect(second.id).toBe('demo-repo-release-tooling');
    expect(second.repoPath).toBe(first.repoPath);
    expect(second.datapacks).toEqual([{ id: 'dp-agent-k-local-test-release', name: 'agent-k-local/test/release', role: 'dedicated' }]);
    expect(pinFiles()).toEqual([]);
    const repos = (await env.core.projects.repos()).repos;
    expect(repos.map((r) => r.name)).toEqual(['demo-repo']);
    expect(repos[0].projects).toEqual([{ id: 'demo-repo', name: 'Demo' }, { id: 'demo-repo-release-tooling', name: 'Release tooling' }]);

    // Names only have to differ within a repo.
    await expect(env.core.projects.create({ repoPath: env.repo, name: ' demo ', datapack: { mode: 'existing', id: 'dp-x', name: 'x' } }))
      .rejects.toThrow(/already has a project named "Demo"/);

    // The same session name in both projects gets separate branches, worktrees and panes.
    const a = await env.core.sessions.create(first.id, { name: 'Fix it', prompt: 'hi', kind: 'quick' });
    const b = await env.core.sessions.create(second.id, { name: 'Fix it', prompt: 'hi', kind: 'quick' });
    expect([a.branch, b.branch]).toEqual(['ak/fix-it', 'ak/fix-it-2']);
    expect(a.worktreePath).not.toBe(b.worktreePath);
    expect(a.paneName).not.toBe(b.paneName);
    await waitFor(() => env.core.sessions.get(b.id).status, (st) => st === 'idle', 10000, 'second project session');
    // Each session has its own agent id and pin, for its own project's datapack.
    const [rowA, rowB] = [env.core.sessions.row(a.id), env.core.sessions.row(b.id)];
    expect(sessionAgentId(rowA)).not.toBe(sessionAgentId(rowB));
    expect([pinnedDatapack(rowA), pinnedDatapack(rowB)]).toEqual(['dp-agent-k-local-test-demo', 'dp-agent-k-local-test-release']);
    expect(argvLog(env).find((l) => l.env.MEKO_AGENT_ID === sessionAgentId(rowB))?.env.MEKO_DATAPACK_ID)
      .toBe('dp-agent-k-local-test-release');

    // Deleting one project leaves the other's session, worktree and branch alone. The deleted
    // project's worktree goes idle: the repo's other project may reuse it.
    await env.core.projects.delete(first.id);
    expect(existsSync(a.worktreePath)).toBe(true);
    expect(env.core.sessions.ownsWorktree(env.core.sessions.row(a.id))).toBe(false);
    expect(existsSync(b.worktreePath)).toBe(true);
    expect((await env.core.host.panes()).has(b.paneName)).toBe(true);
    expect(env.core.sessions.get(b.id).status).toBe('idle');
    expect(execFileSync('git', ['branch', '--list', 'ak/*'], { cwd: env.repo, encoding: 'utf8' })).toContain('ak/fix-it-2');
    expect((await env.core.projects.repos()).repos[0].projects).toEqual([{ id: 'demo-repo-release-tooling', name: 'Release tooling' }]);
  });

  it("passes on Meko's error when it can't create the project's datapack, and creates nothing", async () => {
    (env.meko as { createDatapack: unknown }).createDatapack = async () => {
      throw new Error('connect ECONNREFUSED');
    };
    await expect(newProject()).rejects.toThrow(/Meko couldn't create the datapack "agent-k-local\/test\/demo": connect ECONNREFUSED/);
    expect(env.core.state().projects).toEqual([]);
  });

  it('runs a session in a worktree, tracks status from hooks, and archives it', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Fix the thing', prompt: 'do it', kind: 'quick' });
    expect(s.branch).toBe('ak/fix-the-thing');
    expect(s.stage).toBe('code');
    expect(existsSync(s.worktreePath)).toBe(true);

    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'first turn to finish');
    expect(env.core.sessions.get(s.id).startup).toBeNull();
    const first = argvLog(env)[0];
    expect(first.args).toContain('--session-id');
    expect(first.args.at(-1)).toContain('do it');
    const agentId = sessionAgentId(env.core.sessions.row(s.id));
    expect(agentId).toMatch(/^claude_code:fix-the-thing-[0-9a-f]{8}$/);
    // CLAUDE_CODE_ENABLE_FUNCTION_HOOKS: Kode's mod is on (the fake reports a Claude Code new enough).
    // CLAUDE_CODE_DISABLE_BG_EXIT_HANDOFF: Claude Code keeps the conversation when it exits, not its background sessions.
    // KODE_FIRST_PROMPT: for Kode's view mod, which draws the first prompt folded.
    expect(first.env).toEqual({
      MEKO_AGENT_ID: agentId, MEKO_DATAPACK_ID: 'dp-agent-k-local-test-demo', CLAUDE_CODE_ENABLE_FUNCTION_HOOKS: '1', CLAUDE_CODE_DISABLE_BG_EXIT_HANDOFF: '1',
      KODE_FIRST_PROMPT: 'Task: Fix the thing',
    });

    // A terminal read line by line caps a line (1,024 bytes on macOS, 4,096 on Linux); Claude's TUI,
    // like the fake, reads it raw, so a typed prompt arrives whole however long its line is.
    await env.core.sessions.submit(s.id, `${'x'.repeat(5000)} LONG_END`);
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'second turn');
    expect(readFileSync(env.core.sessions.row(s.id).transcript_path!, 'utf8')).toContain(`${'x'.repeat(5000)} LONG_END`);

    // Token usage is read from the transcript on each Stop (duplicated lines count once).
    const usage = await waitFor(() => env.core.sessions.get(s.id).usage, (u) => u.messages === 2, 10000, 'usage');
    expect(usage).toEqual({ input: 6, output: 100, cacheRead: 2000, cacheWrite: 400, messages: 2 });
    expect(env.core.state().projects[0].usage.output).toBe(100);
    const home = await env.core.projects.home(p.id);
    expect(home.usage.bySession[0]).toMatchObject({ sessionId: s.id, archived: false });
    expect(home.usage.byModel[0].model).toBe('claude-fake-1');

    // Each turn's end sends the transcript to the session's Meko conversation.
    const captures = await waitFor(() => captureLog(env), (c) => c.length >= 2, 10000, 'transcript captures');
    expect(captures[0]).toMatchObject({
      args: ['checkpoint'],
      input: { transcript_path: join(env.fakeDir, `${s.claudeSessionId}.jsonl`) },
      env: { MEKO_MCP_URL: TEST_MEKO_URL, MEKO_API_KEY: 'test-key' },
    });
    const capturedBeforeArchive = captureLog(env).length;
    // Capture is on: sessions keep the Meko plugin.
    expect(JSON.parse(readFileSync(join(env.core.paths.run, 'hooks.json'), 'utf8')).enabledPlugins).toBeUndefined();

    // The session's Meko conversation was created once, titled after the session, keyed by its Claude session id.
    const named = env.meko.calls.filter((c) => c.name === 'ensureConversation' && c.args[2] === s.claudeSessionId);
    expect(named.map((c) => c.args)).toEqual([[agentId, 'dp-agent-k-local-test-demo', s.claudeSessionId, 'Fix the thing (Demo)']]);

    await env.core.sessions.action(s.id, 'archive');
    expect(captureLog(env).length).toBeGreaterThan(capturedBeforeArchive); // final capture before Claude is stopped
    const panes = await env.core.host.panes();
    expect(panes.has(s.paneName)).toBe(false);
    // The clean worktree waits for the next session, detached so the branch is free.
    expect(existsSync(s.worktreePath)).toBe(true);
    expect(detached(s.worktreePath)).toBe(true);
    expect(env.core.sessions.ownsWorktree(env.core.sessions.row(s.id))).toBe(false);
    const branches = execFileSync('git', ['branch', '--list', 'ak/*'], { cwd: env.repo, encoding: 'utf8' });
    expect(branches).toContain('ak/fix-the-thing'); // archive keeps the branch
    expect(env.core.state().projects[0].sessions).toHaveLength(0);
    expect(env.core.state().projects[0].usage.messages).toBe(2); // archived sessions still count
  });

  it("keeps what Claude's turn left running in the background until the next Stop, or Claude's exit", async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Background', prompt: 'run the tests BACKGROUND', kind: 'quick' });
    const get = () => env.core.sessions.get(s.id);
    // Claude Code's Stop lists the shell and the subagent still running: a shell by its command, others by their description.
    const idle = await waitFor(get, (x) => x.status === 'idle', 10000, 'first turn to finish');
    expect(idle.background).toEqual([{ type: 'shell', description: 'npm test' }, { type: 'subagent', description: 'Review the change' }]);
    // Each Stop replaces the list: one without it (a turn with none, or a Claude Code too old to send it) leaves none.
    await env.core.sessions.submit(s.id, 'next');
    await waitFor(get, (x) => x.status === 'idle' && x.background.length === 0, 10000, 'second turn');
    // Claude's exit takes its background work with it.
    await env.core.sessions.submit(s.id, 'again BACKGROUND');
    await waitFor(get, (x) => x.background.length === 2, 10000, 'third turn');
    await env.core.host.submit(s.paneName, 'exit');
    expect(await waitFor(get, (x) => x.status === 'exited', 10000, 'Claude to exit')).toMatchObject({ background: [] });
  });

  it("creates the session's Meko conversation before Claude starts, and launches even if that fails", async () => {
    const p = await newProject();
    const launchedBefore: number[] = [];
    env.meko.ensureConversation = async (...args: unknown[]) => {
      if (args[3] === 'Name me (Demo)') launchedBefore.push(argvLog(env).length);
      if (args[3] === 'Unnamed (Demo)') throw new Error('Meko is down');
      return 'conv-1';
    };
    const s = await env.core.sessions.create(p.id, { name: 'Name me', prompt: 'hi', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'first turn');
    expect(launchedBefore).toEqual([0]);

    const u = await env.core.sessions.create(p.id, { name: 'Unnamed', prompt: 'hi', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(u.id).status, (st) => st === 'idle', 10000, 'first turn without Meko');
    // Its system prompt gives the conversation_id, or how to make it when Meko didn't answer.
    const system = (n: number) => { const a = argvLog(env)[n].args; return a[a.indexOf('--append-system-prompt') + 1]; };
    expect(system(0)).toContain('Meko conversation_id for this session: "conv-1".');
    expect(system(1)).toContain(`call conversation_create once with session_id "${u.claudeSessionId}"`);
  });

  it('waits on Meko for a new session, but not for resumes for a while once Meko kept a launch waiting', async () => {
    const p = await newProject();
    env.meko.ensureConversation = () => new Promise<string>(() => undefined); // never answers
    const system = (n: number) => { const x = argvLog(env)[n].args; return x[x.indexOf('--append-system-prompt') + 1]; };
    const t0 = Date.now();
    const a = await env.core.sessions.create(p.id, { name: 'First', prompt: 'hi', kind: 'quick' });
    await waitFor(() => argvLog(env).length, (k) => k >= 1, 15000, 'first launch');
    expect(Date.now() - t0).toBeGreaterThanOrEqual(4500); // waited the 5 s
    expect(system(0)).toContain(`call conversation_create once with session_id "${a.claudeSessionId}"`);
    // A resume in the next minute doesn't wait.
    await waitFor(() => env.core.sessions.get(a.id).status, (st) => st === 'idle', 10000, 'first turn');
    await env.core.host.submit(a.paneName, 'exit');
    await waitFor(async () => {
      await env.core.sessions.reconcile();
      return env.core.sessions.get(a.id).status;
    }, (st) => st === 'exited', 10000, 'exited');
    const t = Date.now();
    await env.core.sessions.action(a.id, 'resume');
    await waitFor(() => argvLog(env).length, (k) => k >= 2, 15000, 'resume launch');
    expect(Date.now() - t).toBeLessThan(4000);
    expect(system(1)).toContain(`call conversation_create once with session_id "${a.claudeSessionId}"`);
    // A new session still waits: its conversation must be titled before the plugin's hook makes it.
    const t2 = Date.now();
    await env.core.sessions.create(p.id, { name: 'Second', prompt: 'hi', kind: 'quick' });
    await waitFor(() => argvLog(env).length, (k) => k >= 3, 15000, 'second new launch');
    expect(Date.now() - t2).toBeGreaterThanOrEqual(4500);
  });

  it("gives a session of a project with only shared datapacks the project's conversation, or how to make it", async () => {
    env.meko.ensureConversation = async (...args: unknown[]) => (String(args[2]).startsWith('agent-k-local-project-') ? 'conv-project' : 'conv-session');
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Shared', datapack: { mode: 'none' } });
    env.core.projects.attachDatapack(p.id, { id: 'dp-kb', name: 'team/kb' });
    const system = (n: number) => { const x = argvLog(env)[n].args; return x[x.indexOf('--append-system-prompt') + 1]; };
    await env.core.sessions.create(p.id, { name: 'Reader', prompt: 'hi', kind: 'quick' });
    await waitFor(() => argvLog(env).length, (k) => k >= 1, 10000, 'launch');
    expect(system(0)).toContain('Meko conversation_id for this session: "conv-project".');
    // Meko failing: the line names the project's conversation, the one Kode would have used.
    run(env.core.db, 'UPDATE projects SET meko_conversation_id = NULL WHERE id = ?', p.id);
    env.meko.ensureConversation = async () => {
      throw new Error('Meko is down');
    };
    await env.core.sessions.create(p.id, { name: 'Reader two', prompt: 'hi', kind: 'quick' });
    await waitFor(() => argvLog(env).length, (k) => k >= 2, 10000, 'second launch');
    expect(system(1)).toContain(`call conversation_create once with session_id "agent-k-local-project-${p.id}", agent_id "agent-k-local", and pass`);
  });

  it('renames a session, and its Meko conversation, but keeps its branch, worktree, pane and Meko agent id', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Old name', prompt: 'hi', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'first turn');
    const agentId = sessionAgentId(env.core.sessions.row(s.id));

    const renamed = env.core.sessions.rename(s.id, { name: '  New\n  name ' });
    expect(renamed).toMatchObject({ name: 'New name', branch: s.branch, worktreePath: s.worktreePath, paneName: s.paneName });
    expect(env.core.state().projects[0].sessions[0].name).toBe('New name');
    expect(env.core.sessions.row(s.id).meko_agent_id).toBe(agentId);
    expect((await env.core.projects.home(p.id)).activity.find((a) => a.kind === 'renamed')).toMatchObject({ text: 'Renamed from "Old name" to "New name"' });
    const retitled = await waitFor(() => env.meko.calls.filter((c) => c.name === 'retitleConversation'), (c) => c.length === 1, 5000, 'the Meko retitle');
    expect(retitled[0].args.slice(0, 4)).toEqual([agentId, 'dp-agent-k-local-test-demo', 'conv-1', 'New name (Demo)']);

    // The same name again changes nothing; a bad one is refused.
    env.core.sessions.rename(s.id, { name: 'New name' });
    env.core.sessions.rename(s.id, {});
    expect(env.meko.calls.filter((c) => c.name === 'retitleConversation')).toHaveLength(1);
    expect(() => env.core.sessions.rename(s.id, { name: ' ' })).toThrow(expect.objectContaining({ status: 400, message: 'Give the session a name.' }));
    expect(() => env.core.sessions.rename(s.id, { name: 'x'.repeat(101) })).toThrow(expect.objectContaining({ status: 400 }));
    expect(() => env.core.sessions.rename(s.id, { name: 'bell\u0007' })).toThrow(expect.objectContaining({ status: 400 }));
    expect(() => env.core.sessions.rename(s.id, { name: 42 as unknown as string })).toThrow(expect.objectContaining({ status: 400 }));
    expect(env.core.sessions.get(s.id).name).toBe('New name');

    // Meko failing leaves the rename done.
    env.meko.retitleConversation = async () => {
      throw new Error('Meko is down');
    };
    expect(env.core.sessions.rename(s.id, { name: 'Third' }).name).toBe('Third');

    await env.core.sessions.action(s.id, 'archive');
    expect(() => env.core.sessions.rename(s.id, { name: 'Fourth' })).toThrow(expect.objectContaining({ status: 409 }));
  });

  it('lists a new session first, and moves a session to the top of its project', async () => {
    const p = await newProject();
    const one = await env.core.sessions.create(p.id, { name: 'One', prompt: 'hi', kind: 'quick' });
    const two = await env.core.sessions.create(p.id, { name: 'Two', prompt: 'hi', kind: 'quick' });
    const three = await env.core.sessions.create(p.id, { name: 'Three', prompt: 'hi', kind: 'quick' });
    const order = () => env.core.state().projects[0].sessions.filter((s) => s.desired === 'running').sort((a, b) => a.position - b.position).map((s) => s.name);
    expect(order()).toEqual(['Three', 'Two', 'One']);

    await env.core.sessions.edit(one.id, { toTop: true });
    expect(order()).toEqual(['One', 'Three', 'Two']);
    const top = env.core.sessions.row(one.id).position;
    await env.core.sessions.edit(one.id, { toTop: true });
    expect(env.core.sessions.row(one.id).position).toBe(top);
    await expect(env.core.sessions.edit(two.id, { toTop: 'yes' as unknown as boolean })).rejects.toMatchObject({ status: 400 });

    await waitFor(() => env.core.sessions.get(three.id).status, (st) => st === 'idle', 10000, 'first turn');
    await env.core.sessions.action(three.id, 'archive');
    await expect(env.core.sessions.edit(three.id, { toTop: true })).rejects.toMatchObject({ status: 409 });
    await env.core.sessions.edit(two.id, { toTop: true });
    expect(order()).toEqual(['Two', 'One']);
  });

  it("changes a session's base to a branch it can be compared with, and refuses one it can't have", async () => {
    const g = (cwd: string, ...a: string[]) => execFileSync('git', a, { cwd, stdio: 'ignore' });
    // A release branch one file past main, and a branch with no history in common.
    g(env.repo, 'checkout', '-q', '-b', 'rel');
    writeFileSync(join(env.repo, 'rel.txt'), 'release\n');
    g(env.repo, 'add', '.');
    g(env.repo, 'commit', '-q', '-m', 'release');
    g(env.repo, 'checkout', '-q', '--orphan', 'alone');
    g(env.repo, 'commit', '-q', '--allow-empty', '-m', 'alone');
    g(env.repo, 'checkout', '-q', 'main');
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Backport', prompt: 'hi', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'first turn');
    expect(await env.core.sessions.baseBranches(s.id)).toEqual(expect.arrayContaining(['main', 'rel', 'alone']));
    expect(await env.core.sessions.baseBranches(s.id)).not.toContain(s.branch);

    // Claude moves the branch onto rel and works there: against main, rel's file shows too.
    g(s.worktreePath, 'reset', '-q', '--hard', 'rel');
    writeFileSync(join(s.worktreePath, 'fix.txt'), 'fix\n');
    g(s.worktreePath, 'add', '.');
    g(s.worktreePath, 'commit', '-q', '-m', 'fix');
    await env.core.sessions.refreshDiff(s.id);
    expect(env.core.sessions.get(s.id).diffStat).toEqual({ files: 2, added: 2, removed: 0 });

    const moved = await env.core.sessions.edit(s.id, { baseBranch: ' rel ' });
    expect(moved).toMatchObject({ baseBranch: 'rel', name: 'Backport', diffStat: { files: 1, added: 1, removed: 0 } });
    expect((await env.core.projects.home(p.id)).activity.find((a) => a.kind === 'base')).toMatchObject({ text: 'Base changed from main to rel' });

    const refused = async (req: Parameters<typeof env.core.sessions.edit>[1], status: number) =>
      expect(env.core.sessions.edit(s.id, req)).rejects.toMatchObject({ status });
    await refused({ baseBranch: 'nope' }, 400);
    await refused({ baseBranch: '' }, 400);
    await refused({ baseBranch: 'alone' }, 400);
    await refused({ baseBranch: s.branch }, 400); // its own branch: Discard would think nothing is unpushed
    await refused({ baseBranch: 'main', name: ' ' }, 400); // a bad name changes neither
    run(env.core.db, 'UPDATE sessions SET pr_number = 7 WHERE id = ?', s.id);
    await refused({ baseBranch: 'main' }, 409);
    run(env.core.db, "UPDATE sessions SET pr_number = NULL, kind = 'review-pr' WHERE id = ?", s.id);
    await refused({ baseBranch: 'main' }, 409);
    expect(env.core.sessions.get(s.id).baseBranch).toBe('rel');
  });

  it("searches Meko for the task when a session is created, and gives Claude what it found at SessionStart", async () => {
    const p = await newProject();
    env.meko.search = async (...args: unknown[]) => {
      env.meko.calls.push({ name: 'search', args });
      return {
        results: [
          { id: 'm1', source: 'memory' as const, text: 'The thing lives in src/thing.ts; it breaks when the cache is cold.', score: 0.8, datapackName: 'agent-k-local/test/demo' },
          { id: 'm2', source: 'memory' as const, text: 'Unrelated note.', score: 0.3, datapackName: 'agent-k-local/test/demo' },
        ],
        tokensSaved: 9600,
      };
    };
    const s = await env.core.sessions.create(p.id, { name: 'Fix the thing', prompt: 'Fix the thing when the cache is cold', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'first turn');
    const search = env.meko.calls.find((c) => c.name === 'search')!;
    expect(search.args.slice(0, 3)).toEqual(['Fix the thing Fix the thing when the cache is cold', { id: 'dp-agent-k-local-test-demo', name: 'agent-k-local/test/demo', role: 'dedicated' }, AGENTK_AGENT_ID]);
    const started = sessionStarts(env);
    expect(started).toHaveLength(1);
    const ctx = JSON.parse(started[0].context).hookSpecificOutput;
    expect(ctx.hookEventName).toBe('SessionStart');
    expect(ctx.additionalContext).toContain('## What Meko already knows about this task');
    expect(ctx.additionalContext).toContain('src/thing.ts');
    expect(ctx.additionalContext).not.toContain('Unrelated note.');
    expect(get(env.core.db, 'SELECT outcome, results, tokens_saved FROM meko_preloads WHERE session_id = ?', s.id)).toEqual({ outcome: 'found', results: 1, tokens_saved: 9600 });
    // The first turn is over, and something from Meko was in it.
    const home = await waitFor(() => env.core.projects.home(p.id), (h) => h.usage.mekoFirstTurn.sessions === 1, 10000, 'the first turn');
    const { chars } = get<{ chars: number }>(env.core.db, 'SELECT chars FROM meko_preloads WHERE session_id = ?', s.id)!;
    expect(chars).toBe(ctx.additionalContext.length);
    expect(home.usage.mekoFirstTurn).toEqual({ sessions: 1, startSearches: 1, searchedTokens: 0, seedTokens: Math.round(chars / 4) });

    // The prompts ask for a Meko search and say how to load the tools.
    const argv = argvLog(env)[0].args;
    const sys = argv[argv.indexOf('--append-system-prompt') + 1];
    expect(sys).toContain('ToolSearch query "+meko context_search"');
    // And the rule for what the session saves there for the team.
    expect(sys).toContain(`Saving to Meko: this project's datapack is shared by the team working on it.`);
    expect(sys).toContain(`"session":"${s.id}"`);
    expect(argv[argv.length - 1]).toMatch(/search Meko for earlier designs, decisions and gotchas about this task/);

    // A resumed session starts without it.
    await env.core.host.submit(s.paneName, 'exit');
    await waitFor(async () => {
      await env.core.sessions.reconcile();
      return env.core.sessions.get(s.id).status;
    }, (st) => st === 'exited', 10000, 'exited');
    await env.core.sessions.action(s.id, 'resume');
    await waitFor(() => sessionStarts(env).length, (n) => n === 2, 10000, 'the resume');
    expect(sessionStarts(env)[1]).toMatchObject({ sessionId: s.claudeSessionId, source: 'resume', context: '' });
  });

  it('searches every datapack at start, skips one whose search fails, and records a search that failed everywhere', async () => {
    const p = await newProject();
    env.core.projects.attachDatapack(p.id, { id: 'dp-kb', name: 'team/kb' });
    let down = new Set(['dp-kb']);
    env.meko.search = async (...args: unknown[]) => {
      const dp = args[1] as { id: string; name: string };
      env.meko.calls.push({ name: 'search', args });
      if (down.has(dp.id)) throw new Error(`${dp.name} is down`);
      return { results: [{ id: 'm1', source: 'memory' as const, text: 'A gotcha about the cold cache in src/thing.ts.', score: 0.8, datapackName: dp.name }], tokensSaved: 9000 };
    };
    const s = await env.core.sessions.create(p.id, { name: 'One down', prompt: 'the cold cache', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'first turn');
    expect(env.meko.calls.filter((c) => c.name === 'search').map((c) => (c.args[1] as { id: string }).id).sort())
      .toEqual(['dp-agent-k-local-test-demo', 'dp-kb']);
    // What Meko says the search saved counts only the datapacks that answered.
    expect(get(env.core.db, 'SELECT outcome, tokens_saved FROM meko_preloads WHERE session_id = ?', s.id)).toEqual({ outcome: 'found', tokens_saved: 9000 });

    // With both answering, it's the sum.
    down = new Set();
    const both = await env.core.sessions.create(p.id, { name: 'Both up', prompt: 'the cold cache', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(both.id).status, (st) => st === 'idle', 10000, 'first turn');
    expect(get(env.core.db, 'SELECT outcome, tokens_saved FROM meko_preloads WHERE session_id = ?', both.id)).toEqual({ outcome: 'found', tokens_saved: 18000 });

    down = new Set(['dp-kb', 'dp-agent-k-local-test-demo']);
    const u = await env.core.sessions.create(p.id, { name: 'All down', prompt: 'the cold cache', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(u.id).status, (st) => st === 'idle', 10000, 'first turn');
    await waitFor(() => get(env.core.db, 'SELECT outcome FROM meko_preloads WHERE session_id = ?', u.id), (r) => !!r, 5000, 'the failed search');
    expect(get(env.core.db, 'SELECT outcome FROM meko_preloads WHERE session_id = ?', u.id)).toEqual({ outcome: 'error' });
    expect(sessionStarts(env).at(-1)).toMatchObject({ source: 'startup', context: '' });
    // The project home's search still answers, with nothing.
    expect(await env.core.projects.search(p.id, 'cold cache')).toEqual([]);
  });

  it("creates a project's Meko conversation once when several callers ask at the same time", async () => {
    const p = await newProject();
    run(env.core.db, 'UPDATE projects SET meko_conversation_id = NULL WHERE id = ?', p.id);
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    let creates = 0;
    env.meko.ensureConversation = async () => {
      creates++;
      await gate;
      return 'conv-project';
    };
    const both = Promise.all([env.core.projects.ensureConversation(p.id), env.core.projects.ensureConversation(p.id)]);
    release();
    expect(await both).toEqual(['conv-project', 'conv-project']);
    expect(creates).toBe(1);
    expect(await env.core.projects.ensureConversation(p.id)).toBe('conv-project');
    expect(creates).toBe(1);
  });

  it("never holds up a session for a slow Meko search", async () => {
    const p = await newProject();
    env.meko.search = () => new Promise(() => undefined);
    const s = await env.core.sessions.create(p.id, { name: 'Slow', prompt: 'hi', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'first turn');
    expect(sessionStarts(env)).toEqual([expect.objectContaining({ source: 'startup', context: '' })]);
    expect(get(env.core.db, 'SELECT 1 AS x FROM meko_preloads WHERE session_id = ?', s.id)).toBeUndefined();
  });

  it('counts the Meko searches Claude makes and estimates what they saved', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Look it up', prompt: 'MEKO_SEARCH', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'first turn');
    // Both responses (the search and the one after it) must be read before the estimate settles.
    const home = await waitFor(() => env.core.projects.home(p.id), (h) => h.usage.meko.found === 1 && h.usage.total.messages === 2, 10000, 'the Meko search');
    expect(home.usage.meko).toMatchObject({ calls: 1, lookups: 1, found: 1, empty: 0, errors: 0, saves: 0 });
    // The stub's model has no price: the estimate is in tokens only, and the cost leaves it out.
    // Context 5,103 tokens, a 20-token result, one later response: 2 × 5,103 + 480 + (6,000 − 20) × 2.
    expect(home.usage.meko.savings).toEqual({ searches: 1, seeds: 0, tokens: 22_646, usd: null, unpricedSearches: 1, unpricedSeeds: 0 });
    expect(home.usage.cost).toEqual({ usd: 0, unpricedMessages: 2 });
    expect(home.usage.bySession[0].meko).toEqual({ lookups: 1, found: 1, saves: 0, mekoSaved: { tokens: 0, searches: 0, seeds: 0, unreported: 0 } });
    const overview = usageOverview(env.core.db);
    expect(overview.meko.found).toBe(1);
    expect(overview.byProject[0].meko).toMatchObject({ lookups: 1, found: 1, savings: { searches: 1, tokens: 22_646, usd: null } });
  });

  it('reconcile leaves a session alone while it is still being created', async () => {
    const p = await newProject();
    // A slow create (git fetch, a busy index.lock, Meko) lets the 5-second reconcile run
    // after the worktree exists but before Claude is launched. It must not "recover" the
    // session, or Claude is launched twice with the same session id.
    env.meko.ensureConversation = async () => {
      await waitFor(() => env.core.sessions.get('demo-repo--race-me').startup?.step, (st) => st === 'launch', 10000, 'worktree');
      await env.core.sessions.reconcile();
      return 'conv-1';
    };
    const s = await env.core.sessions.create(p.id, { name: 'Race me', prompt: 'hi', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'first turn');
    const recovered = get<{ n: number }>(env.core.db, "SELECT COUNT(*) AS n FROM session_events WHERE session_id = ? AND kind = 'recovered'", s.id);
    expect(recovered?.n).toBe(0);
    expect(argvLog(env)).toHaveLength(1);
  });

  it('refuses to archive a session with uncommitted or untracked files, and keeps its branch once it can', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Keep me', prompt: 'hi', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'first turn');
    writeFileSync(join(s.worktreePath, 'draft.txt'), 'wip\n');
    await expect(env.core.sessions.action(s.id, 'archive')).rejects.toMatchObject({
      status: 409, message: "This session's worktree has 1 uncommitted file. Ask Kode to commit, or Discard the session to throw the changes away.",
    });
    expect(env.core.sessions.get(s.id).desired).toBe('running');
    expect((await env.core.host.panes()).has(s.paneName)).toBe(true);

    const g = (...a: string[]) => execFileSync('git', a, { cwd: s.worktreePath, stdio: 'ignore' });
    g('add', 'draft.txt');
    g('commit', '-q', '-m', 'draft');
    await env.core.sessions.action(s.id, 'archive');
    expect(env.core.sessions.get(s.id).desired).toBe('archived');
    expect(execFileSync('git', ['branch', '--list', 'ak/*'], { cwd: env.repo, encoding: 'utf8' })).toContain('ak/keep-me');
  });

  it('shows a session\'s changes, and discards it with uncommitted and unpushed work', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Throw away', prompt: 'hi', kind: 'quick' });
    // The demo repo has no origin, so the base is used as is.
    const created = get<{ text: string }>(env.core.db, "SELECT text FROM session_events WHERE session_id = ? AND kind = 'created'", s.id);
    expect(created?.text).toBe('Started on ak/throw-away from main');
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'first turn');
    const g = (...a: string[]) => execFileSync('git', a, { cwd: s.worktreePath, stdio: 'ignore' });
    writeFileSync(join(s.worktreePath, 'done.txt'), 'committed\n');
    g('add', 'done.txt');
    g('commit', '-q', '-m', 'local only');
    writeFileSync(join(s.worktreePath, 'draft.txt'), 'one\ntwo\n');

    const c = await env.core.sessions.changes(s.id);
    expect(c.files).toMatchObject([
      { path: 'done.txt', kind: 'added', added: 1, uncommitted: false },
      { path: 'draft.txt', kind: 'untracked', added: 2, uncommitted: true },
    ]);
    await env.core.sessions.refreshDiff(s.id);
    expect(env.core.sessions.get(s.id).diffStat).toEqual({ files: 2, added: 3, removed: 0 });
    expect(await env.core.sessions.discardCheck(s.id)).toEqual({ worktreeExists: true, uncommittedFiles: 1, unpushedCommits: 1 });

    await env.core.sessions.action(s.id, 'discard', { skipLearn: true });
    expect((await env.core.host.panes()).has(s.paneName)).toBe(false);
    // The changes are gone; the worktree itself waits, clean and detached, for the next session.
    expect(existsSync(join(s.worktreePath, 'draft.txt'))).toBe(false);
    expect(detached(s.worktreePath)).toBe(true);
    expect(await env.core.sessions.discardCheck(s.id)).toEqual({ worktreeExists: false, uncommittedFiles: 0, unpushedCommits: 0 });
    expect(execFileSync('git', ['branch', '--list', 'ak/*'], { cwd: env.repo, encoding: 'utf8' })).not.toContain('ak/throw-away');
    expect(env.core.state().projects[0].sessions).toHaveLength(0);
    expect(env.core.state().projects[0].usage.messages).toBeGreaterThan(0); // discarded sessions still count
    const last = get<{ kind: string }>(env.core.db, 'SELECT kind FROM session_events WHERE session_id = ? ORDER BY id DESC LIMIT 1', s.id);
    expect(last?.kind).toBe('discarded');
    await expect(env.core.sessions.action(s.id, 'discard')).rejects.toThrow(/archived/);
    await expect(env.core.sessions.changes(s.id)).rejects.toThrow(/worktree is gone/);
  });

  const designRow = (id: string) =>
    get<{ markdown: string; html: string | null; artifact_url: string | null }>(env.core.db, 'SELECT markdown, html, artifact_url FROM designs WHERE session_id = ?', id);
  const transcriptText = (claudeSessionId: string) => readFileSync(join(env.fakeDir, `${claudeSessionId}.jsonl`), 'utf8');
  /** The prompts in the fake's transcript, oldest first. */
  const userPrompts = (claudeSessionId: string) => readJsonl<{ type: string; text: string }>(join(env.fakeDir, `${claudeSessionId}.jsonl`)).filter((e) => e.type === 'user').map((e) => e.text);

  it('designs outside plan mode, guards the repo, and moves to Code when the design is approved', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Design me', prompt: 'DESIGN_MD it' });
    // The code review is off here (makeEnv), so the Code Review step is skipped ahead of time.
    expect(s).toMatchObject({ kind: 'plan', stage: 'design', skippedStages: ['explore', 'reviewing-code'] });
    const designDir = join(env.core.paths.designs, s.id);
    const first = (await waitFor(() => argvLog(env), (l) => l.length > 0, 10000, 'launch'))[0].args;
    expect(first).not.toContain('--permission-mode');
    expect(first).toEqual(expect.arrayContaining(['--add-dir', designDir]));
    expect(first.at(-1)).toContain(join(designDir, 'design.md')); // the prompt says where the design goes

    const ready = await waitFor(() => env.core.sessions.get(s.id), (x) => x.status === 'needs_input' && x.hasDesign, 10000, 'design');
    expect(ready.statusDetail).toBe('Design ready for your approval');
    expect(ready.designUpdatedAt).toBeGreaterThan(0);
    expect(designRow(s.id)).toEqual({ markdown: '# Design\n\nDo the thing.\n', html: null, artifact_url: null });
    await waitFor(() => env.meko.calls.filter((c) => c.name === 'putArtifact').map((c) => c.args[0]), (n) => n.includes('design-design-me.md'), 5000, 'design saved to Meko');

    // While designing, edits inside the repo are denied and edits elsewhere go ahead.
    await env.core.sessions.submit(s.id, 'EDIT_REPO');
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'guarded turn');
    const guard = readJsonl<{ file: string; out: string }>(join(env.fakeDir, 'guard.jsonl'));
    expect(JSON.parse(guard[0].out).hookSpecificOutput).toMatchObject({ permissionDecision: 'deny', permissionDecisionReason: expect.stringMatching(/Design stage/) });
    expect(guard[1].out).toBe('');
    expect(env.core.sessions.get(s.id).status).toBe('idle'); // a turn that didn't touch the design isn't "ready" again

    await env.core.sessions.action(s.id, 'approve-design');
    expect(env.core.sessions.get(s.id).stage).toBe('code');
    await waitFor(() => transcriptText(s.claudeSessionId), (t) => t.includes('The design is approved'), 10000, 'approval prompt');
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'turn end after approval');

    // Past Design, the guard lets edits through.
    await env.core.sessions.submit(s.id, 'EDIT_REPO');
    await waitFor(() => readJsonl(join(env.fakeDir, 'guard.jsonl')).length, (n) => n === 4, 10000, 'second guard check');
    const after = readJsonl<{ file: string; out: string }>(join(env.fakeDir, 'guard.jsonl'));
    expect(after[2].out).toBe('');
  });

  it('keeps a published Artifact as the design', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Big design', prompt: 'DESIGN_ARTIFACT please' });
    await waitFor(() => env.core.sessions.get(s.id), (x) => x.status === 'needs_input' && x.hasDesign, 10000, 'design');
    const d = designRow(s.id)!;
    expect(d.markdown).toBe('');
    expect(d.html).toContain('<h1>Design</h1>');
    expect(d.artifact_url).toBe('https://claude.ai/code/artifact/0f1e2d3c-4b5a-6978-8a9b-0c1d2e3f4a5b');
    await waitFor(() => env.meko.calls.filter((c) => c.name === 'putArtifact').map((c) => c.args[0]), (n) => n.includes('design-big-design.html'), 5000, 'artifact saved to Meko');
    expect(env.meko.calls.some((c) => c.name === 'putArtifact' && c.args[0] === 'design-big-design.md')).toBe(false);
    await expect(env.core.sessions.action(s.id, 'approve-design')).resolves.toMatchObject({ stage: 'code' });
  });

  /** The design files of a session sent to Meko, as [name, content]. */
  const designsSent = (slug: string) => env.meko.calls.filter((c) => c.name === 'putArtifact' && String(c.args[0]).startsWith(`design-${slug}`))
    .map((c) => [c.args[0], c.args[1]] as [string, string]);

  it('saves each changed version of a design to Meko under its own name', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Redesign', prompt: 'hi', kind: 'quick' });
    const save = async (d: { markdown?: string; html?: string; artifactUrl?: string }, total: number) => {
      await env.core.lifecycle.saveDesign(s.id, d);
      await waitFor(() => designsSent('redesign').length, (n) => n >= total, 5000, `${total} designs sent`);
    };
    await save({ markdown: '# One' }, 1);
    await save({ markdown: '# One' }, 1); // unchanged: not sent again
    await save({ markdown: '# Two' }, 2);
    await save({ html: '<h1>A</h1>', artifactUrl: 'https://claude.ai/code/artifact/abc' }, 3);
    await save({ html: '<h1>A</h1>', artifactUrl: 'https://claude.ai/code/artifact/abc' }, 3);
    await save({ html: '<h1>B</h1>' }, 4);
    await save({ markdown: '# Three' }, 5);
    expect(designsSent('redesign')).toEqual([
      ['design-redesign.md', '# One'],
      ['design-redesign-v2.md', '# Two'],
      ['design-redesign.html', '<h1>A</h1>'],
      ['design-redesign-v2.html', '<h1>B</h1>'],
      ['design-redesign-v3.md', '# Three'],
    ]);
  });

  it('sends a design saved while Meko was off once it is back, as the first version', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Offline design', prompt: 'hi', kind: 'quick' });
    env.core.mekoStatus = { ...env.core.mekoStatus, configured: false };
    await env.core.lifecycle.saveDesign(s.id, { markdown: '# One' });
    await env.core.lifecycle.saveDesign(s.id, { markdown: '# Two' });
    env.core.mekoStatus = { ...env.core.mekoStatus, configured: true };
    await env.core.lifecycle.collectDesign(env.core.sessions.row(s.id)); // a turn end, with the design unchanged
    await waitFor(() => designsSent('offline-design'), (d) => d.length > 0, 5000, 'design sent');
    expect(designsSent('offline-design')).toEqual([['design-offline-design.md', '# Two']]);
  });

  it('sends a design again under the same name when sending it failed', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Retry', prompt: 'hi', kind: 'quick' });
    const put = env.meko.putArtifact;
    const down = vi.fn(async () => {
      throw new Error('Meko is down');
    });
    env.meko.putArtifact = down;
    try {
      await env.core.lifecycle.saveDesign(s.id, { markdown: '# One' });
      await waitFor(() => down.mock.calls.length, (n) => n > 0, 5000, 'failed send');
    } finally {
      env.meko.putArtifact = put;
    }
    await env.core.lifecycle.collectDesign(env.core.sessions.row(s.id));
    await waitFor(() => designsSent('retry'), (d) => d.length > 0, 5000, 'design sent');
    expect(designsSent('retry')).toEqual([['design-retry.md', '# One']]);
  });

  it('runs a task started at Explore with no design, PR or merge gates until it moves on', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Look around', prompt: 'why is it slow', start: 'explore' });
    expect(s).toMatchObject({ kind: 'plan', stage: 'explore', skippedStages: ['reviewing-code'] });
    expect(existsSync(s.worktreePath)).toBe(true);
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'first turn');
    const args = argvLog(env)[0].args;
    const system = args[args.indexOf('--append-system-prompt') + 1];
    expect(system).toContain('Stages: Explore → Design → Code → Pull Request → Merge & Close;');
    expect(system).toContain("It's in Explore now");
    // The name heads the task, so the first prompt needn't repeat it.
    expect(args.at(-1)).toMatch(/Explore stage of a Kode task[\s\S]*Task: Look around\n\nwhy is it slow/);

    await expect(env.core.sessions.action(s.id, 'approve-design')).rejects.toMatchObject({ status: 409 });
    await expect(env.core.sessions.action(s.id, 'open-pr')).rejects.toMatchObject({ status: 409 });
    expect(env.core.sessions.get(s.id).stage).toBe('explore');

    // Edits in the repo go ahead (POCs), and review comments on the diff reach Claude.
    await env.core.sessions.submit(s.id, 'EDIT_REPO');
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'edit turn');
    const guard = readJsonl<{ file: string; out: string }>(join(env.fakeDir, 'guard.jsonl'));
    expect(guard.every((g) => g.out === '')).toBe(true);
    env.core.comments.add(s.id, { path: 'a.txt', side: 'new', line: 1, endLine: 1, excerpt: 'a', body: 'Try it another way.' });
    expect(await env.core.comments.send(s.id)).toEqual({ sent: 1, queued: 0 });
    await waitFor(() => transcriptText(s.claudeSessionId), (t) => t.includes('Try it another way.'), 10000, 'comment prompt');
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'comment turn');

    await env.core.sessions.action(s.id, 'archive');
    expect(env.core.sessions.get(s.id)).toMatchObject({ status: 'archived', stage: 'explore' });
  });

  it('runs a learn session: a task at Explore on the seed prompt and the brief, closed with no learn step once Claude writes its summary', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, {
      name: 'Learn the project', learn: { about: 'The billing service.', where: 'services/billing/', months: 24 }, start: 'code', prompt: 'ignored',
    });
    expect(s).toMatchObject({ kind: 'plan', stage: 'explore', purpose: 'learn' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'first turn');
    const first = argvLog(env)[0].args.at(-1);
    expect(first).toMatch(/learn session of a Kode project[^]*Task: Learn the project\n\nWhat this project is about:\nThe billing service\.\n\nWhere its files are:\nservices\/billing\//);
    expect(first).toContain(`Read the PRs merged since ${String(new Date().getUTCFullYear() - 2)}-`);
    expect(first).toContain(join(env.core.paths.designs, s.id, 'learned.md'));
    // The seeding rule, not the usual one, which leaves out what a PR already says.
    expect(first).toContain('(PR #123)');
    expect(first).not.toContain('what the diff or PR description already says');
    // Not done yet: it stays, and the home doesn't offer another while it's open.
    expect(env.core.sessions.get(s.id).status).toBe('idle');
    expect((await env.core.projects.home(p.id)).learnSessionOpen).toBe(true);
    // A new dedicated datapack is offered one of its own: this session was launched on the old one.
    await env.core.projects.setDedicatedDatapack(p.id, { mode: 'create', name: 'team/switched', replace: p.datapacks[0].id });
    expect((await env.core.projects.home(p.id)).learnSessionOpen).toBe(false);

    // Done, but with a change of its own in the worktree, which closing would throw away: it stays, and says why.
    writeFileSync(join(s.worktreePath, 'notes.txt'), 'x');
    await env.core.sessions.submit(s.id, 'LEARN_DONE');
    await waitFor(() => events(s.id), (e) => e.some((x) => x.kind === 'learn-kept'), 10000, 'kept');
    expect(events(s.id).find((e) => e.kind === 'learn-done')?.text).toBe('Covered billing and 12 PRs.\nSaved 5 facts.');
    expect(existsSync(join(env.core.paths.designs, s.id, 'learned.md'))).toBe(false);
    expect(env.core.sessions.get(s.id).status).not.toBe('archived');

    rmSync(join(s.worktreePath, 'notes.txt'));
    await env.core.sessions.submit(s.id, 'LEARN_DONE');
    // The discard's last step: its git work (freeing the worktree, deleting the branch) is done by then.
    await waitFor(() => events(s.id), (e) => e.some((x) => x.kind === 'discarded'), 10000, 'closed');
    expect(get(env.core.db, 'SELECT COUNT(*) AS n FROM meko_learns WHERE session_id = ?', s.id)).toEqual({ n: 0 });
    expect((await env.core.projects.home(p.id)).learnSessionOpen).toBe(false);
  });

  it("holds a learn session's close for a Save learnings step still running, and closes when it ends", async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Learn', learn: { about: 'The billing service.', where: '', months: 0 } });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'first turn');
    const running = vi.spyOn(env.core.learn, 'running').mockReturnValue(true);
    await env.core.sessions.submit(s.id, 'LEARN_DONE');
    await waitFor(() => env.core.sessions.get(s.id), (x) => x.discarding, 10000, 'held');
    expect(env.core.sessions.get(s.id).status).not.toBe('archived');
    expect(existsSync(join(env.core.paths.designs, s.id, 'learned.md'))).toBe(false);
    running.mockRestore();
    env.core.lifecycle.afterLearn(s.id);
    await waitFor(() => events(s.id), (e) => e.some((x) => x.kind === 'discarded'), 10000, 'closed');
  });

  it.each([
    { what: 'no about', learn: { about: ' ', where: '', months: 24 }, error: /what the project is about/ },
    { what: 'months out of range', learn: { about: 'x', where: '', months: 121 }, error: /from 0 to 120/ },
    { what: 'months not whole', learn: { about: 'x', where: '', months: 1.5 }, error: /whole number/ },
    { what: 'a review', learn: { about: 'x', where: '', months: 1 }, kind: 'review-pr', error: /leave out the PR/ },
    { what: 'no dedicated datapack', learn: { about: 'x', where: '', months: 1 }, noDatapack: true, error: /has none/ },
    { what: 'Meko off', learn: { about: 'x', where: '', months: 1 }, mekoOff: true, error: /Meko isn't running on this computer/ },
  ] as const)('refuses a learn session with $what', async (c) => {
    const p = 'noDatapack' in c
      ? await env.core.projects.create({ repoPath: env.repo, name: 'Bare', datapack: { mode: 'none' } })
      : await newProject();
    if ('mekoOff' in c) env.core.mekoStatus = { ...env.core.mekoStatus, configured: false };
    const req = { name: 'Learn', learn: c.learn, ...('kind' in c ? { kind: c.kind, pr: { repo: 'acme/demo', number: 1 } } : {}) } as CreateSessionRequest;
    await expect(env.core.sessions.create(p.id, req)).rejects.toMatchObject({ status: 400, message: expect.stringMatching(c.error) });
  });

  it('starts a task at Code, stores it as plan, keeps the old types and skipDesign working, and refuses an unknown kind or start', async () => {
    const p = await newProject();
    const code = await env.core.sessions.create(p.id, { name: 'Quick one', prompt: 'hi', start: 'code' });
    expect(code).toMatchObject({ kind: 'plan', stage: 'code', skippedStages: ['explore', 'design', 'reviewing-code'] });
    // Stored and sent under an old type's name, so an older release (after a rollback) and a page
    // from before start stages (until it reloads) can still index their stage lists with it.
    expect(env.core.sessions.row(code.id).kind).toBe('plan');
    await waitFor(() => argvLog(env), (l) => l.length > 0, 10000, 'launch');
    const args = argvLog(env)[0].args;
    const system = args[args.indexOf('--append-system-prompt') + 1];
    expect(system).toContain('Stages: Explore → Design → Code → Pull Request → Merge & Close;');
    expect(system).not.toContain("It's in Explore now");
    const stageOf = async (req: Partial<CreateSessionRequest>) => (await env.core.sessions.create(p.id, { prompt: 'hi', ...req } as CreateSessionRequest)).stage;
    expect(await stageOf({ name: 'Old explore', kind: 'explore' })).toBe('explore');
    expect(await stageOf({ name: 'Old plan', kind: 'plan' })).toBe('design');
    expect(await stageOf({ name: 'Old quick', kind: 'quick', start: 'design' })).toBe('design');
    expect(await stageOf({ name: 'Old client', skipDesign: true })).toBe('code');
    await expect(env.core.sessions.create(p.id, { name: 'Odd', kind: 'yolo' as never })).rejects.toMatchObject({ status: 400 });
    await expect(env.core.sessions.create(p.id, { name: 'Odd', start: 'review' as never })).rejects.toMatchObject({ status: 400 });
  });

  it('uses the name as the first prompt of a task started without one, but not of one started at Explore, and has no design to approve yet', async () => {
    const p = await newProject();
    const quick = await env.core.sessions.create(p.id, { name: 'Fix the typo', prompt: '  ', start: 'code' });
    await waitFor(() => argvLog(env), (l) => l.length === 1, 10000, 'quick launch');
    expect(argvLog(env)[0].args.at(-1)).toMatch(/^Task: Fix the typo/);
    const plan = await env.core.sessions.create(p.id, { name: 'Add retries' });
    await waitFor(() => argvLog(env), (l) => l.length === 2, 10000, 'plan launch');
    expect(argvLog(env)[1].args.at(-1)).toMatch(/Task: Add retries/);
    await expect(env.core.sessions.action(plan.id, 'approve-design')).rejects.toThrow(/hasn't written a design/);
    const explore = await env.core.sessions.create(p.id, { name: 'Why slow', start: 'explore' });
    await waitFor(() => argvLog(env), (l) => l.length === 3, 10000, 'explore launch');
    // Explore too: with no first prompt, the name is the task.
    expect(argvLog(env)[2].args.at(-1)).toMatch(/^You are in the Explore stage[^]*\nTask: Why slow\n/);
    const rows = [quick, plan, explore].map((s) => env.core.sessions.row(s.id).initial_prompt);
    expect(rows).toEqual(['Fix the typo', 'Add retries', 'Why slow']);
    await expect(env.core.sessions.create(p.id, { name: 'Typed', prompt: 123 as never })).rejects.toMatchObject({ status: 400 });
  });

  it('answers with the session still starting, before its worktree is made, and reports its startup steps', async () => {
    const p = await newProject();
    const steps: string[] = [];
    const onChange = () => {
      const s = env.core.state().projects[0]?.sessions[0];
      if (s?.startup && steps.at(-1) !== s.startup.step) steps.push(s.startup.step);
    };
    env.core.on('change', onChange);
    const { session, done } = await env.core.sessions.begin(p.id, { name: 'Big repo', prompt: 'hi', kind: 'quick' });
    expect(session).toMatchObject({ status: 'starting', startup: { step: 'worktree', hasPrompt: true } });
    expect(existsSync(session.worktreePath)).toBe(false);
    // Recovery leaves it alone meanwhile.
    await env.core.sessions.reconcile();
    expect(env.core.sessions.row(session.id).status).toBe('starting');
    const made = await done;
    expect(existsSync(made.worktreePath)).toBe(true);
    expect(made.startup?.step).toBe('loading');
    await waitFor(() => env.core.sessions.get(session.id).status, (st) => st === 'idle', 10000, 'first turn');
    env.core.off('change', onChange);
    expect(steps.slice(0, 3)).toEqual(['worktree', 'launch', 'loading']);
    expect(steps).toContain('prompt');
    expect(env.core.sessions.get(session.id).startup).toBeNull();
  });

  it('starts a task with the images added in the New session dialog, moved into its folder for Claude to read', async () => {
    const p = await newProject();
    const png = Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 1, 2, 3]);
    const pastes = env.core.paths.pastes;
    const a = env.core.sessions.stageImage(p.id, png).id;
    const b = env.core.sessions.stageImage(p.id, png).id;
    expect(() => env.core.sessions.stageImage('nope', png)).toThrow(/no longer exists/);

    // Refused before anything starts: the images stay staged for the dialog to send again.
    await expect(env.core.sessions.begin(p.id, { name: 'Typo', start: 'code', baseBranch: 'mian', images: [a] })).rejects.toMatchObject({ status: 400 });
    await expect(env.core.sessions.begin(p.id, { name: 'Gone', start: 'code', images: ['20261007-101500-00000000.png'] }))
      .rejects.toMatchObject({ status: 400, message: expect.stringMatching(/no longer on the host/) });
    expect(existsSync(join(stagedDir(pastes, p.id), a))).toBe(true);

    const s = await env.core.sessions.create(p.id, { name: 'Fix the footer', prompt: 'fix the footer: it overflows on phones', start: 'code', images: [a, b] });
    const paths = [a, b].map((i) => join(pastes, s.id, i));
    for (const path of paths) expect(readFileSync(path)).toEqual(png);
    expect(existsSync(join(stagedDir(pastes, p.id), a))).toBe(false);
    // Kept apart from the task, and added to it in Claude's prompts.
    expect(env.core.sessions.row(s.id).initial_prompt).toBe('fix the footer: it overflows on phones');
    const prompt = (await waitFor(() => argvLog(env), (l) => l.length > 0, 10000, 'launch'))[0].args.at(-1);
    // A prompt that starts with the name (in any case) isn't given it again.
    expect(prompt).toContain(`Task: fix the footer: it overflows on phones\n\nImages the user attached (open each with the Read tool to see it):\n- ${paths[0]}\n- ${paths[1]}`);

    // At Explore with images and no text, the name is the task, and the images go with it.
    const c = env.core.sessions.stageImage(p.id, png).id;
    const e = await env.core.sessions.create(p.id, { name: 'Look at this', start: 'explore', images: [c] });
    expect(e.hasTask).toBe(true);
    await waitFor(() => argvLog(env), (l) => l.length > 1, 10000, 'second launch');
    expect(argvLog(env)[1].args.at(-1)).toContain(`Task: Look at this\n\nImages the user attached (open each with the Read tool to see it):\n- ${join(pastes, e.id, c)}`);
    await waitFor(() => env.core.sessions.get(e.id).status, (st) => st === 'idle', 10000, 'Explore turn');
    // One started so before the name became the task has none: moving on offers to take one.
    env.core.sessions.update(e.id, { initial_prompt: null });
    expect(env.core.sessions.get(e.id).hasTask).toBe(false);
    const moved = await env.core.sessions.move(e.id, { stage: 'design', task: 'Make the footer wrap' });
    expect(moved.hasTask).toBe(true);
    await waitFor(() => transcriptText(moved.claudeSessionId), (t) => t.includes('You are in the Design stage'), 10000, 'Design prompt');
    // The typed task, in the Design stage's start prompt, with the images still going with it.
    const designPrompt = transcriptText(moved.claudeSessionId).split('\n').find((l) => l.includes('You are in the Design stage'))!;
    expect(designPrompt).toContain('Make the footer wrap');
    expect(designPrompt).toContain(join(pastes, e.id, c));

    // An image gone before it could be moved (swept, or taken by another start) is left out, and the activity log says so.
    const d = env.core.sessions.stageImage(p.id, png).id;
    const gone = env.core.sessions.stageImage(p.id, png).id;
    const starting = env.core.sessions.begin(p.id, { name: 'Half there', prompt: 'Look', start: 'code', images: [d, gone] });
    // The images are checked before begin's first await.
    rmSync(join(stagedDir(pastes, p.id), gone));
    const { session: h, done } = await starting;
    await done;
    const half = (await waitFor(() => argvLog(env).map((l) => l.args.at(-1) ?? '').find((a) => a.includes(join(pastes, h.id))), (a) => !!a, 10000, 'third launch'))!;
    expect(half).toContain(join(pastes, h.id, d));
    expect(half).not.toContain(gone);
    expect(get<{ text: string }>(env.core.db, "SELECT text FROM session_events WHERE session_id = ? AND kind = 'images'", h.id)?.text)
      .toBe("An image you added wasn't on the host any more when the session started, so Kode doesn't have it. Paste it into the terminal.");

    // Closing the project removes what's still staged.
    env.core.sessions.stageImage(p.id, png);
    await env.core.projects.delete(p.id);
    expect(existsSync(stagedDir(pastes, p.id))).toBe(false);
  });

  it("keeps a start whose worktree can't be made as a failed start, until it's dismissed or started again", async () => {
    const p = await newProject();
    env.core.projects.update(p.id, { settings: { notify: true } });
    const notes: { title: string; body: string }[] = [];
    env.core.on('notify', (n) => notes.push(n));
    // A base branch the repo doesn't have is refused before the answer, so the dialog shows it.
    await expect(env.core.sessions.begin(p.id, { name: 'Typo', start: 'code', baseBranch: 'mian' }))
      .rejects.toMatchObject({ status: 400, message: "The base branch mian isn't in demo-repo or on its origin. Pick another base branch." });
    // Git refuses the branch only when the worktree is made: ak/no-base/x leaves no room for ak/no-base.
    const block = (branch: string) => execFileSync('git', ['branch', branch], { cwd: env.repo });
    block('ak/no-base/x');
    const image = env.core.sessions.stageImage(p.id, Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a])).id;
    const { session, done } = await env.core.sessions.begin(p.id, { name: 'No base', prompt: 'the task', start: 'code', images: [image] });
    await expect(done).rejects.toMatchObject({ status: 400, message: expect.stringMatching(/^Couldn't create the worktree: /) });
    // Its images go with it; the request still names the images, so Try again can say they weren't kept.
    expect(existsSync(join(env.core.paths.pastes, session.id))).toBe(false);
    expect(env.core.state().projects[0].sessions.map((s) => s.id)).not.toContain(session.id);
    expect(get(env.core.db, 'SELECT path FROM worktrees WHERE session_id = ?', session.id)).toBeUndefined();
    expect(env.core.state().failedStarts).toEqual([{
      id: session.id, projectId: p.id, name: 'No base', error: expect.stringMatching(/^Couldn't create the worktree: /), at: expect.any(Number),
      request: { name: 'No base', harness: 'claude', prompt: 'the task', kind: 'task', start: 'code', baseBranch: 'main', images: [image] },
    }]);
    expect(notes).toEqual([expect.objectContaining({ projectId: p.id, sessionId: session.id, title: "No base: Couldn't start" })]);

    // Started again under the same name, it's no longer a failed start; the other one is until dismissed.
    const req: CreateSessionRequest = { name: 'No base', prompt: 'the task', start: 'code' };
    block('ak/other/x');
    await expect(env.core.sessions.create(p.id, { ...req, name: 'Other' })).rejects.toMatchObject({ status: 400 });
    execFileSync('git', ['branch', '-D', 'ak/no-base/x'], { cwd: env.repo });
    const again = await env.core.sessions.create(p.id, req);
    expect(again.id).toBe(session.id);
    expect(env.core.state().failedStarts.map((f) => f.name)).toEqual(['Other']);
    // A failed start for a work item gives the item back the session it had.
    const work = await env.core.workQueue.create({ title: 'Fix it' });
    env.core.workQueue.update(work.id, { sessionId: again.id });
    block('ak/retry/x');
    await expect(env.core.sessions.create(p.id, { ...req, name: 'Retry', workItemId: work.id })).rejects.toMatchObject({ status: 400 });
    expect(env.core.workQueue.get(work.id).sessionId).toBe(again.id);
    env.core.sessions.dismissFailedStart('demo-repo--retry');
    env.core.sessions.dismissFailedStart('demo-repo--other');
    expect(env.core.state().failedStarts).toEqual([]);
    // Its fake Claude writes its transcript into the test home until the turn ends: removing the home before then fails.
    await waitFor(() => env.core.sessions.get(again.id).status, (st) => st === 'idle', 10000, 'the started session\'s first turn');
  });

  it("follows the transcript of a session whose hooks never arrive, and binds a removed hook socket again", async () => {
    const p = await newProject();
    // As when another server removed the socket: Claude's hooks find nothing and give up quietly.
    rmSync(env.core.paths.hookSock);
    const s = await env.core.sessions.create(p.id, { name: 'No hooks', prompt: 'hi', kind: 'quick' });
    await waitFor(() => existsSync(join(env.fakeDir, `${s.claudeSessionId}.jsonl`)), Boolean, 10000, 'the first turn');
    await env.core.sessions.reconcile();
    expect(env.core.sessions.get(s.id).status).toBe('starting');

    // The server restarts meanwhile: the new one picks the startup up, timed from when it began.
    env.core.sessions.clearStartup(s.id);
    await env.core.sessions.recover();
    const began = get<{ last_event_at: number }>(env.core.db, 'SELECT last_event_at FROM sessions WHERE id = ?', s.id)!.last_event_at;
    expect(env.core.sessions.get(s.id).startup).toMatchObject({ step: 'loading', startedAt: began, stepSince: began, hasPrompt: true });

    // The fake writes its transcript elsewhere; Claude Code writes it here.
    const transcript = transcriptPathFor(s.worktreePath, s.claudeSessionId);
    mkdirSync(dirname(transcript), { recursive: true });
    writeFileSync(transcript, '{}\n');
    vi.useFakeTimers({ toFake: ['Date'] });
    try {
      vi.setSystemTime(Date.now() + MISSED_START_MS + 20_000);
      await env.core.sessions.reconcile();
    } finally {
      vi.useRealTimers();
    }
    expect(env.core.sessions.get(s.id)).toMatchObject({ status: 'idle', startup: null });
    // It keeps following the transcript: written just now means working.
    writeFileSync(transcript, '{}\n{}\n');
    await env.core.sessions.reconcile();
    expect(env.core.sessions.get(s.id).status).toBe('working');

    await env.core.checkHookSocket();
    expect(existsSync(env.core.paths.hookSock)).toBe(true);
    await env.core.sessions.submit(s.id, 'second turn');
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'the Stop hook');
    // Hooks arrive again, so the transcript no longer sets the status.
    writeFileSync(transcript, '{}\n{}\n{}\n');
    await env.core.sessions.reconcile();
    expect(env.core.sessions.get(s.id).status).toBe('idle');
  });

  it('keeps trying to bind the hook socket after a failed rebind, and never binds it after stopping', async () => {
    // A directory in the socket's place makes the rename fail.
    rmSync(env.core.paths.hookSock);
    mkdirSync(join(env.core.paths.hookSock, 'x'), { recursive: true });
    await expect(env.core.checkHookSocket()).rejects.toThrow();
    rmSync(env.core.paths.hookSock, { recursive: true });
    await env.core.checkHookSocket();
    expect(socketId(env.core.paths.hookSock)).not.toBeNull();

    await env.core.stop();
    expect(existsSync(env.core.paths.hookSock)).toBe(false);
    await env.core.checkHookSocket();
    expect(existsSync(env.core.paths.hookSock)).toBe(false);
  });

  it('leaves a hook socket another server bound when it stops', async () => {
    rmSync(env.core.paths.hookSock);
    const other = await startHookServer(env.core.paths.hookSock, () => undefined);
    try {
      const id = socketId(env.core.paths.hookSock);
      await env.core.stop();
      expect(socketId(env.core.paths.hookSock)).toBe(id);
    } finally {
      await new Promise<void>((r) => other.close(() => r()));
    }
  });

  it('recovers after the session host dies: auto-continues mid-turn sessions, with a 15 minute guard', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Crash test', prompt: 'hi', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'idle');

    // Simulate a host crash in the middle of a turn.
    let launches = argvLog(env).length;
    run(env.core.db, "UPDATE sessions SET status = 'working' WHERE id = ?", s.id);
    await env.killHost();
    await env.core.sessions.reconcile();
    let after = env.core.sessions.get(s.id);
    expect(after.recoveredAt).not.toBeNull();
    expect(after.statusDetail).toMatch(/continuing/);
    await waitFor(() => argvLog(env).length, (n) => n > launches, 10000, 'relaunch');
    const relaunch = argvLog(env).at(-1)!;
    expect(relaunch.args).toEqual(expect.arrayContaining(['--resume', s.claudeSessionId]));
    expect(relaunch.args.at(-1)).toMatch(/host restarted/);
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'continued turn');

    // A second crash right away: resumed, but not auto-continued.
    launches = argvLog(env).length;
    run(env.core.db, "UPDATE sessions SET status = 'working' WHERE id = ?", s.id);
    await env.killHost();
    await env.core.sessions.reconcile();
    after = env.core.sessions.get(s.id);
    expect(after.status).toBe('recovered');
    expect(after.statusDetail).toMatch(/not auto-continued/);
    await waitFor(() => argvLog(env).length, (n) => n > launches, 10000, 'second relaunch');
    expect(argvLog(env).at(-1)!.args.at(-1)).not.toMatch(/host restarted/);
  });

  it('boot recovery leaves running sessions alone', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Keep running', prompt: 'hi', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'idle');
    const launches = argvLog(env).length;
    await waitFor(() => captureLog(env).length, (n) => n >= 1, 10000, 'turn capture');
    const captured = captureLog(env).length;
    await env.core.sessions.recover();
    expect(argvLog(env).length).toBe(launches);
    await waitFor(() => captureLog(env).length, (n) => n > captured, 10000, 'catch-up capture on boot');
  });

  it("won't start while a version before the session host's tmux service is installed, so no Claude runs twice, but starts beside a leftover one", async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'In tmux too', prompt: 'hi', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'idle');
    await env.core.stop();
    await env.killHost();
    const launches = argvLog(env).length;
    const core = env.core as unknown as { services: () => unknown; legacyTmuxPanes: () => Promise<Set<string>> };
    const services = (hostInstalled: boolean) => () => ({ legacyTmuxInstalled: () => true, installed: (svc: string) => svc === 'host' && hostInstalled });
    // No host service, or one but this session still in tmux (a tmux version reinstalled after the move).
    for (const [host, inTmux] of [[false, []], [true, [s.paneName]]] as const) {
      core.services = services(host);
      core.legacyTmuxPanes = async () => new Set(inTmux);
      await expect(env.core.start()).rejects.toThrow(/Run: meko kode install/);
    }
    expect(argvLog(env).length).toBe(launches);
    // Only a pane this home doesn't know (a deleted session's): the tmux service is a leftover of
    // the move, and refusing would roll back every update.
    core.legacyTmuxPanes = async () => new Set(['ak-deleted-session']);
    await env.core.start();
    await waitFor(() => argvLog(env).length, (n) => n > launches, 10000, 'resumed in the host');
  });

  it('waits for a session that is still starting before archiving it, and leaves no Claude behind', async () => {
    const p = await newProject();
    let discarding: Promise<unknown> | undefined;
    env.meko.ensureConversation = async () => {
      await waitFor(() => env.core.sessions.get('demo-repo--late').startup?.step, (st) => st === 'launch', 10000, 'worktree');
      discarding = env.core.sessions.action('demo-repo--late', 'discard');
      await sleep(300); // the discard is waiting for the start to finish
      return 'conv-1';
    };
    const s = await env.core.sessions.create(p.id, { name: 'Late', prompt: 'hi', kind: 'quick' });
    await discarding;
    expect((await env.core.host.panes()).has(s.paneName)).toBe(false);
    expect(env.core.sessions.ownsWorktree(env.core.sessions.row(s.id))).toBe(false);
    expect(execFileSync('git', ['branch', '--list', 'ak/*'], { cwd: env.repo, encoding: 'utf8' })).not.toContain('ak/late');
    expect(env.core.sessions.row(s.id)).toMatchObject({ desired: 'archived', status: 'archived' });

    // If it is archived anyway before Claude starts, Claude isn't started.
    env.meko.ensureConversation = async () => {
      run(env.core.db, "UPDATE sessions SET desired = 'archived', status = 'archived' WHERE id = ?", 'demo-repo--gone');
      return 'conv-2';
    };
    await expect(env.core.sessions.create(p.id, { name: 'Gone', prompt: 'hi', kind: 'quick' })).rejects.toThrow(/archived while it was starting/);
    expect((await env.core.host.panes()).has('ak-demo-repo--gone')).toBe(false);
    expect(env.core.sessions.row('demo-repo--gone').status).toBe('archived');
  });

  it('keeps the panes of two projects on one repo apart', async () => {
    const first = await newProject();
    const second = await env.core.projects.create({ repoPath: env.repo, name: 'X', datapack: { mode: 'create', name: 'agent-k-local/test/x' } });
    expect(second.id).toBe('demo-repo-x');
    // "demo-repo" + "x-fix" and "demo-repo-x" + "fix" used to share the pane name ak-demo-repo-x-fix.
    const a = await env.core.sessions.create(first.id, { name: 'X fix', prompt: 'hi', kind: 'quick' });
    const b = await env.core.sessions.create(second.id, { name: 'Fix', prompt: 'hi', kind: 'quick' });
    expect([a.paneName, b.paneName]).toEqual(['ak-demo-repo--x-fix', 'ak-demo-repo-x--fix']);
    await waitFor(() => [env.core.sessions.get(a.id).status, env.core.sessions.get(b.id).status], (st) => st.every((x) => x === 'idle'), 10000, 'both sessions');
    const panes = await env.core.host.panes();
    expect([panes.get(a.paneName)?.dead, panes.get(b.paneName)?.dead]).toEqual([false, false]);

    // A name some other session (such as one from before this naming) holds is never reused.
    await env.core.sessions.action(a.id, 'archive');
    run(env.core.db, 'UPDATE sessions SET tmux_name = ? WHERE id = ?', 'ak-demo-repo-x--dup', a.id);
    const c = await env.core.sessions.create(second.id, { name: 'Dup', prompt: 'hi', kind: 'quick' });
    expect(c.paneName).toBe('ak-demo-repo-x--dup-2');
  });

  it('never relaunches over a live Claude, and skips a reconcile tick when the host fails', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Live', prompt: 'hi', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'idle');
    const launches = argvLog(env).length;
    await expect(env.core.sessions.launch(env.core.sessions.row(s.id), { resume: true, prompt: null })).rejects.toThrow(/already running/);
    const panes = env.core.host.panes.bind(env.core.host);
    env.core.host.panes = async () => {
      throw new Error('host hiccup');
    };
    await expect(env.core.sessions.reconcile()).rejects.toThrow(/hiccup/);
    env.core.host.panes = panes;
    await env.core.sessions.reconcile();
    expect(argvLog(env)).toHaveLength(launches);
    expect(env.core.sessions.get(s.id).status).toBe('idle');
  });

  it('parks a mid-turn session after two auto-continues in an hour', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Parked', prompt: 'hi', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'idle');
    const now = Date.now();
    const history = [now - 40 * 60 * 1000, now - 20 * 60 * 1000];
    run(env.core.db, "UPDATE sessions SET status = 'working', auto_continue_at = ? WHERE id = ?", JSON.stringify(history), s.id);
    const launches = argvLog(env).length;
    await env.killHost();
    await env.core.sessions.reconcile();
    const after = env.core.sessions.get(s.id);
    expect(after.status).toBe('needs_input');
    expect(after.statusDetail).toMatch(/Recovered twice in the last hour/);
    await waitFor(() => argvLog(env).length, (n) => n > launches, 10000, 'relaunch');
    const relaunch = argvLog(env).at(-1)!.args;
    expect(relaunch).toEqual(expect.arrayContaining(['--resume', s.claudeSessionId]));
    expect(relaunch.join(' ')).not.toMatch(/host restarted/);
    expect(JSON.parse(env.core.sessions.row(s.id).auto_continue_at)).toEqual(history);
  });

  /** Runs the idle stop as if `ms` had passed. */
  async function stopIdleIn(ms: number): Promise<void> {
    vi.useFakeTimers({ toFake: ['Date'] });
    try {
      vi.setSystemTime(Date.now() + ms);
      await env.core.sessions.stopIdle();
    } finally {
      vi.useRealTimers();
    }
  }

  it("stops a session's agent idle past [sessions] idle_stop_hours, keeps it stopped through recovery, and resumes it", async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Sleepy', prompt: 'hi', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'idle');
    const notes: unknown[] = [];
    env.core.on('notify', (n) => notes.push(n));
    const live = async () => (await env.core.host.panes()).has(s.paneName);
    const set = (fields: string, ...values: (string | number | null)[]) => run(env.core.db, `UPDATE sessions SET ${fields} WHERE id = ?`, ...values, s.id);
    const HOUR = 60 * 60 * 1000;
    env.core.config.sessions.idleStopHours = 1;

    // Each of these keeps the agent running, however long it has been idle.
    const kept: [string, number, () => void, () => void][] = [
      ['not idle for the limit yet', 50 * 60 * 1000, () => undefined, () => undefined],
      ['waiting on you', 2 * HOUR, () => set("status = 'needs_input'"), () => set("status = 'idle'")],
      ['a queued Open PR', 2 * HOUR, () => set("queued_gate = 'open-pr'"), () => set('queued_gate = NULL')],
      ['Run to merge', 2 * HOUR, () => set('auto_run = 1'), () => set('auto_run = 0')],
      ['the stop turned off', 2 * HOUR, () => (env.core.config.sessions.idleStopHours = 0), () => (env.core.config.sessions.idleStopHours = 1)],
      // Hooks quiet for hours, but a key typed into its terminal half an hour ago.
      ['a key typed in its terminal', 30 * 60 * 1000, () => {
        set('last_event_at = ?', Date.now() - 2 * HOUR);
        env.core.sessions.typed(s.id);
      }, () => undefined],
      // An action that needed the agent may still be waiting on GitHub.
      ['an action on its way', 30 * 60 * 1000, () => {
        set('last_event_at = ?', Date.now() - 2 * HOUR);
        void env.core.sessions.awake(env.core.sessions.row(s.id), 'for a test');
      }, () => undefined],
    ];
    for (const [why, after, before, undo] of kept) {
      before();
      await stopIdleIn(after);
      expect([why, await live(), env.core.sessions.get(s.id).idleStoppedAt]).toEqual([why, true, null]);
      undo();
    }

    await stopIdleIn(2 * HOUR);
    const stopped = env.core.sessions.get(s.id);
    expect(stopped).toMatchObject({ status: 'exited', statusDetail: 'Stopped after 1 hour idle to free memory. Resume to pick up where it left off.' });
    expect(stopped.idleStoppedAt).not.toBeNull();
    expect(await live()).toBe(false);
    expect(events(s.id).at(-1)).toEqual({ kind: 'idle-stopped', text: 'Claude stopped after 1 hour idle' });

    // Neither reconcile nor a recovery after the host restarted brings it back.
    const launches = argvLog(env).length;
    await env.core.sessions.reconcile();
    await env.killHost();
    await env.core.sessions.reconcile();
    expect(argvLog(env).length).toBe(launches);
    expect(env.core.sessions.get(s.id)).toMatchObject({ status: 'exited', idleStoppedAt: stopped.idleStoppedAt });
    expect(notes).toEqual([]);

    await env.core.sessions.action(s.id, 'resume');
    await waitFor(() => argvLog(env).length, (n) => n > launches, 10000, 'resume launch');
    expect(argvLog(env).at(-1)!.args).toEqual(expect.arrayContaining(['--resume', s.claudeSessionId]));
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'resumed');
    expect(env.core.sessions.get(s.id).idleStoppedAt).toBeNull();
  });

  it('starts a session on the model picked, keeps it through an idle stop and a resume, and refuses a malformed one', async () => {
    const p = await newProject();
    await expect(env.core.sessions.create(p.id, { name: 'Bad', prompt: 'hi', kind: 'quick', model: '--dangerously-skip-permissions' }))
      .rejects.toThrow("--dangerously-skip-permissions isn't a model name");
    await expect(env.core.sessions.create(p.id, { name: 'Bad', prompt: 'hi', kind: 'quick', model: 'opus 5' })).rejects.toThrow("opus 5 isn't a model name");
    const launches = (id: string) => argvLog(env).filter((a) => a.args.includes(id)).map((a) => a.args);
    const modelArg = (args: string[]) => (args.includes('--model') ? args[args.indexOf('--model') + 1] : null);

    const plain = await env.core.sessions.create(p.id, { name: 'Plain', prompt: 'hi', kind: 'quick' });
    expect(plain.model).toBeNull();
    await waitFor(() => launches(plain.claudeSessionId).length, (n) => n === 1, 10000, 'launch');
    expect(launches(plain.claudeSessionId).map(modelArg)).toEqual([null]);

    const s = await env.core.sessions.create(p.id, { name: 'On opus', prompt: 'hi', kind: 'quick', model: ' opus ' });
    expect(s.model).toBe('opus');
    await waitFor(() => launches(s.claudeSessionId).length, (n) => n === 1, 10000, 'launch');
    expect(launches(s.claudeSessionId).map(modelArg)).toEqual(['opus']);
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'idle');

    // Resumed with no --model, Claude Code would run on its default: every launch passes it.
    env.core.config.sessions.idleStopHours = 1;
    await stopIdleIn(2 * 60 * 60 * 1000);
    expect(env.core.sessions.get(s.id).idleStoppedAt).not.toBeNull();
    await env.core.sessions.action(s.id, 'resume');
    await waitFor(() => launches(s.claudeSessionId).length, (n) => n === 2, 10000, 'resume launch');
    const resumed = launches(s.claudeSessionId)[1];
    expect(resumed).toEqual(expect.arrayContaining(['--resume', s.claudeSessionId]));
    expect(modelArg(resumed)).toBe('opus');
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'resumed');
  });

  it('starts a stopped agent again for what needs it, and sends what waited once it is up', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Wake me', prompt: 'hi', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'idle');
    env.core.config.sessions.idleStopHours = 1;
    const stop = async () => {
      await stopIdleIn(2 * 60 * 60 * 1000);
      expect(env.core.sessions.get(s.id).idleStoppedAt).not.toBeNull();
    };

    // Review comments: queued, the agent starts again, and they go once it's up.
    await stop();
    env.core.comments.add(s.id, { path: 'src/a.ts', side: 'new', line: 1, endLine: 1, excerpt: 'const a = 1;', body: 'Rename it.' });
    expect(await env.core.comments.send(s.id)).toEqual({ sent: 0, queued: 1 });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'started again');
    expect(events(s.id).filter((e) => e.kind === 'resumed').at(-1)?.text).toBe('Resumed for your review comments');
    expect(await env.core.comments.deliver(s.id)).toBe(1);
    await waitFor(() => transcriptText(s.claudeSessionId), (t) => t.includes('Rename it.'), 10000, 'comment prompt');
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'comment turn');

    // A move that sends nothing leaves it stopped; one that sends something starts it first, and its prompt follows.
    await stop();
    await env.core.sessions.move(s.id, { stage: 'explore', prompt: false });
    expect(env.core.sessions.get(s.id)).toMatchObject({ stage: 'explore', status: 'exited' });
    expect(env.core.sessions.get(s.id).idleStoppedAt).not.toBeNull();
    await env.core.sessions.move(s.id, { stage: 'design', instructions: 'Sketch it first.' });
    expect(events(s.id).filter((e) => e.kind === 'resumed').at(-1)?.text).toBe('Resumed for the move to Design');
    await waitFor(() => transcriptText(s.claudeSessionId), (t) => t.includes('Sketch it first.'), 10000, 'move prompt');
    expect(env.core.sessions.get(s.id).idleStoppedAt).toBeNull();
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'move turn');

    // A start that fails (its branch and worktree are gone) says why, and recovery takes it from there.
    await stop();
    rmSync(s.worktreePath, { recursive: true, force: true });
    execFileSync('git', ['worktree', 'prune'], { cwd: env.repo });
    execFileSync('git', ['branch', '-D', s.branch], { cwd: env.repo });
    env.core.sessions.wakeSoon(s.id, 'for a test');
    const failed = await waitFor(() => env.core.sessions.get(s.id), (x) => x.idleStoppedAt === null, 10000, 'failed start');
    expect(failed).toMatchObject({ status: 'exited', statusDetail: expect.stringMatching(/^Couldn't start Claude again: .*Kode tries again in 5 minutes, or Resume it now\.$/) });
  });

  it('follows /clear to the new conversation, so Resume continues that one', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Clear me', prompt: 'hi', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'idle');
    await env.core.host.submit(s.paneName, '/clear');
    const moved = await waitFor(() => env.core.sessions.row(s.id).claude_session_id, (id) => id !== s.claudeSessionId, 10000, 'new session id');
    expect(env.core.sessions.get(s.id).status).toBe('idle'); // /clear's SessionEnd isn't an exit
    await env.core.host.submit(s.paneName, 'exit');
    await waitFor(async () => {
      await env.core.sessions.reconcile();
      return env.core.sessions.get(s.id).status;
    }, (st) => st === 'exited', 10000, 'exited');
    await env.core.sessions.action(s.id, 'resume');
    const last = await waitFor(() => argvLog(env).at(-1)!.args, (a) => a.includes('--resume'), 10000, 'resume launch');
    expect(last).toEqual(expect.arrayContaining(['--resume', moved]));
  });

  it('ignores hook events that carry an unknown token', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Tokens', prompt: 'hi', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'idle');
    for (const ev of [{ hook_event_name: 'SessionStart', session_id: 'someone-else' }, { hook_event_name: 'SessionEnd', reason: 'exit' }]) {
      expect(await postHook(env.core.paths.hookSock, '/hook', 'not-a-session-token', ev)).toBe(204);
    }
    await sleep(300);
    expect(env.core.sessions.row(s.id)).toMatchObject({ claude_session_id: s.claudeSessionId, status: 'idle' });
  });

  it('notices the folder-trust prompt in a recovered session, not only a new one', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Trust me', prompt: 'hi', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'idle');
    writeFileSync(join(env.fakeDir, 'trust-prompt'), '');
    await env.killHost();
    await env.core.sessions.reconcile();
    expect(env.core.sessions.get(s.id).status).toBe('recovered');
    await waitFor(async () => {
      await env.core.sessions.reconcile();
      return env.core.sessions.get(s.id);
    }, (x) => x.status === 'needs_input' && /trust this folder/.test(x.statusDetail ?? ''), 15000, 'trust prompt noticed');
    rmSync(join(env.fakeDir, 'trust-prompt'));
    await env.core.host.sendKeys(s.paneName, 'Enter');
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'answered');
  });

  it('backs off after a failed recovery instead of retrying every tick', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Unlucky', prompt: 'hi', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'idle');
    const tries = vi.spyOn(env.core.sessions, 'recoverOne');
    // The worktree is gone and can't be restored (as when the remote is unreachable).
    run(env.core.db, 'UPDATE sessions SET repo_path = ? WHERE id = ?', join(env.home, 'gone'), s.id);
    rmSync(s.worktreePath, { recursive: true, force: true });
    await env.killHost();
    await env.core.sessions.reconcile();
    expect(tries).toHaveBeenCalledTimes(1);
    expect(env.core.sessions.get(s.id).status).toBe('exited');
    expect(env.core.sessions.get(s.id).statusDetail).toMatch(/Couldn't restart Claude.*tries again in 5 minutes/);
    await env.core.sessions.reconcile();
    await env.core.sessions.reconcile();
    expect(tries).toHaveBeenCalledTimes(1);
  });

  it('stops recovering a session whose branch is gone, and says so', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Branchless', prompt: 'hi', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'idle');
    const tries = vi.spyOn(env.core.sessions, 'recoverOne');
    await env.killHost();
    execFileSync('git', ['worktree', 'remove', '--force', s.worktreePath], { cwd: env.repo });
    execFileSync('git', ['branch', '-D', s.branch], { cwd: env.repo, stdio: 'ignore' });
    await env.core.sessions.reconcile();
    await env.core.sessions.reconcile();
    expect(tries).toHaveBeenCalledTimes(1);
    expect(env.core.sessions.get(s.id)).toMatchObject({ status: 'exited', statusDetail: expect.stringMatching(/ak\/branchless is gone/) });
    await expect(env.core.sessions.action(s.id, 'resume')).rejects.toMatchObject({ status: 409, message: expect.stringMatching(/is gone/) });
  });

  it('refuses a second prompt submitted while the first one starts a turn', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'One at a time', prompt: 'hi', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'idle');
    const results = await Promise.allSettled([env.core.sessions.submit(s.id, 'first one'), env.core.sessions.submit(s.id, 'second one')]);
    expect(results.map((r) => r.status)).toEqual(['fulfilled', 'rejected']);
    expect(String((results[1] as PromiseRejectedResult).reason)).toMatch(/working on another prompt/);
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'turn');
    const prompts = userPrompts(s.claudeSessionId);
    expect(prompts.at(-1)).toBe('first one');
  });

  it("pins the dedicated datapack for the session's own agent id each time it starts", async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Pinned', prompt: 'hi', kind: 'quick' });
    const row = env.core.sessions.row(s.id);
    expect(pinnedDatapack(row)).toBe('dp-agent-k-local-test-demo');
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'first turn');
    // Gone (say, cleaned up by hand): resuming pins it again.
    rmSync(sessionPinFile(row));
    await env.core.host.submit(s.paneName, 'exit');
    await waitFor(async () => {
      await env.core.sessions.reconcile();
      return env.core.sessions.get(s.id).status;
    }, (st) => st === 'exited', 10000, 'exited');
    await env.core.sessions.action(s.id, 'resume');
    await waitFor(() => existsSync(sessionPinFile(row)), (x) => x, 10000, 'pin on resume');
    expect(pinnedDatapack(row)).toBe('dp-agent-k-local-test-demo');
  });

  it('keeps a session from before per-session agent ids on its project-wide one until it is launched again', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Old one', prompt: 'hi', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'first turn');
    // As a session launched before the upgrade: Claude runs with claude_code:<project id>.
    run(env.core.db, 'UPDATE sessions SET meko_agent_id = NULL WHERE id = ?', s.id);
    const dp = env.core.projects.get(p.id).datapacks[0];
    env.meko.calls.length = 0;
    await env.core.sessions.conversationId(env.core.sessions.row(s.id), dp);
    expect(env.meko.calls.find((c) => c.name === 'ensureConversation')?.args[0]).toBe(legacyAgentId(p.id));

    // Launched again, it gets its own.
    await env.core.host.submit(s.paneName, 'exit');
    await waitFor(async () => {
      await env.core.sessions.reconcile();
      return env.core.sessions.get(s.id).status;
    }, (st) => st === 'exited', 10000, 'exited');
    const before = argvLog(env).length;
    await env.core.sessions.action(s.id, 'resume');
    await waitFor(() => argvLog(env).length, (n) => n > before, 10000, 'resume launch');
    const row = env.core.sessions.row(s.id);
    expect(row.meko_agent_id).toBe(sessionAgentId(row));
    expect(argvLog(env).at(-1)!.env.MEKO_AGENT_ID).toBe(sessionAgentId(row));
  });

  it("removes an archived session's pin a day later, and never a project-wide one", async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Pinned', prompt: 'hi', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'first turn');
    const row = env.core.sessions.row(s.id);
    // What sessions used before, and what the Meko plugin uses for plain Claude Code in a repo of that name.
    const legacy = join(process.env.MEKO_WATERMARK_DIR!, `pin-claude_code_${p.id}.json`);
    writeFileSync(legacy, '{"datapack_id":"mine"}');
    await env.core.sessions.action(s.id, 'archive');
    const archivedAt = env.core.sessions.row(s.id).archived_at!;
    env.core.projects.prunePins(archivedAt + PIN_KEEP_MS - 1000);
    expect(existsSync(sessionPinFile(row))).toBe(true);
    env.core.projects.prunePins(archivedAt + PIN_KEEP_MS + 1000);
    expect(existsSync(sessionPinFile(row))).toBe(false);
    expect(existsSync(legacy)).toBe(true);
  });

  it('deleting a project keeps worktrees with uncommitted changes, archived or not', async () => {
    const p = await newProject();
    const kept = await env.core.sessions.create(p.id, { name: 'Kept', prompt: 'hi', kind: 'quick' });
    const dirty = await env.core.sessions.create(p.id, { name: 'Dirty', prompt: 'hi', kind: 'quick' });
    const clean = await env.core.sessions.create(p.id, { name: 'Clean', prompt: 'hi', kind: 'quick' });
    await waitFor(() => [kept, dirty, clean].map((x) => env.core.sessions.get(x.id).status), (st) => st.every((x) => x === 'idle'), 10000, 'idle');
    writeFileSync(join(kept.worktreePath, 'draft.txt'), 'wip\n');
    // Archive refuses this; a PR merged on GitHub still archives with the changes kept.
    await env.core.sessions.archive(env.core.sessions.row(kept.id), { keepBranch: true });
    writeFileSync(join(dirty.worktreePath, 'draft.txt'), 'wip\n');
    await env.core.projects.delete(p.id);
    expect(existsSync(join(kept.worktreePath, 'draft.txt'))).toBe(true);
    expect(existsSync(join(dirty.worktreePath, 'draft.txt'))).toBe(true);
    expect(existsSync(clean.worktreePath)).toBe(false);
    expect((await env.core.host.panes()).has(dirty.paneName)).toBe(false);
  });

  it("reports plan limits from the status line and still prints the user's own status line", async () => {
    // The user's status line, which ours replaces and then runs with the same input.
    mkdirSync(process.env.CLAUDE_CONFIG_DIR!, { recursive: true });
    writeFileSync(join(process.env.CLAUDE_CONFIG_DIR!, 'settings.json'), JSON.stringify({
      statusLine: { type: 'command', command: `cat > '${join(env.fakeDir, 'user-statusline.in')}'; printf 'my status'`, padding: 2 },
    }));
    const { writeHookFiles } = await import('../../src/server/claude.js');
    writeHookFiles(env.core.paths);
    const settings = JSON.parse(readFileSync(join(env.core.paths.run, 'hooks.json'), 'utf8'));
    expect(settings.statusLine).toEqual({ type: 'command', command: join(env.core.paths.run, 'statusline.sh'), padding: 2 });
    expect(env.core.state().limits).toBeNull();

    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Limits', prompt: 'STATUSLINE', kind: 'quick' });
    const limits = await waitFor(() => env.core.state().limits, (l) => l !== null, 10000, 'plan limits');
    expect(limits).toMatchObject({ fiveHour: { usedPercent: 42, resetsAt: 4102444800000 }, sevenDay: { usedPercent: 18 } });
    await waitFor(get, (x) => x.status === 'idle', 10000, 'turn end');
    expect(readFileSync(join(env.fakeDir, 'statusline.out'), 'utf8')).toBe('my status');
    expect(JSON.parse(readFileSync(join(env.fakeDir, 'user-statusline.in'), 'utf8')).rate_limits.five_hour.used_percentage).toBe(42);
    function get() {
      return env.core.sessions.get(s.id);
    }
  });

  it('sends review comments to Claude now when idle, or when its turn ends', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Review me', prompt: 'hi', kind: 'quick' });
    const get = () => env.core.sessions.get(s.id);
    const state = () => env.core.comments.list(s.id).map((c) => c.state);
    await waitFor(get, (x) => x.status === 'idle', 10000, 'first turn');
    const at = { path: 'src/a.ts', side: 'new', line: 3, endLine: 4, excerpt: 'const a = 1;\nconst b = 2;' };

    // Idle: pasted straight away, as one prompt grouped by file.
    const first = env.core.comments.add(s.id, { ...at, body: 'Use one declaration.' });
    expect(get().comments).toEqual({ drafts: 1, queued: 0 });
    expect(await env.core.comments.send(s.id)).toEqual({ sent: 1, queued: 0 });
    expect(state()).toEqual(['sent']);
    expect(() => env.core.comments.edit(s.id, first.id, 'changed')).toThrow(/already sent/);
    await waitFor(() => transcriptText(s.claudeSessionId), (t) => t.includes('Use one declaration.'), 10000, 'comment prompt');
    const prompt = userPrompts(s.claudeSessionId).at(-1)!.replace(/\s+/g, ' ');
    expect(prompt).toMatch(/left 1 comment\. Address each one/);
    expect(prompt).toContain('## src/a.ts Lines 3–4: ``` const a = 1; const b = 2; ``` Use one declaration.');
    await waitFor(get, (x) => x.status === 'idle', 10000, 'comment turn');

    // Mid-turn: queued, then sent when the Stop hook ends the turn.
    writeFileSync(join(env.fakeDir, 'turn-ms'), '1500');
    await env.core.sessions.submit(s.id, 'a long turn');
    env.core.comments.add(s.id, { ...at, body: 'Second thought.' });
    expect(await env.core.comments.send(s.id)).toEqual({ sent: 0, queued: 1 });
    expect(get().comments).toEqual({ drafts: 0, queued: 1 });
    await waitFor(state, (st) => st.every((x) => x === 'sent'), 10000, 'queued comment sent at turn end');
    await waitFor(() => transcriptText(s.claudeSessionId), (t) => t.includes('Second thought.'), 10000, 'queued prompt');
    await waitFor(get, (x) => x.status === 'idle', 10000, 'second comment turn');
    writeFileSync(join(env.fakeDir, 'turn-ms'), '200');

    // Never pasted into a permission prompt; goes out once Claude is back at its prompt (as after a restart).
    env.core.sessions.setStatus(s.id, 'needs_input', 'Permission needed for Bash');
    const third = env.core.comments.add(s.id, { ...at, body: 'Third.' });
    expect(env.core.comments.edit(s.id, third.id, '  Third, edited.  ').body).toBe('Third, edited.');
    expect(await env.core.comments.send(s.id)).toEqual({ sent: 0, queued: 1 });
    env.core.sessions.setStatus(s.id, 'recovered');
    expect(await env.core.comments.deliver(s.id)).toBe(1);
    expect(await env.core.comments.deliver(s.id)).toBe(0);
    await waitFor(() => transcriptText(s.claudeSessionId), (t) => t.includes('Third, edited.'), 10000, 'third prompt');

    // Drafts can be deleted; nothing to send is an error, and archived sessions take no comments.
    const extra = env.core.comments.add(s.id, { ...at, body: 'Never mind.' });
    env.core.comments.remove(s.id, extra.id);
    await expect(env.core.comments.send(s.id)).rejects.toThrow(/no draft comments/);
    await env.core.sessions.action(s.id, 'archive');
    expect(() => env.core.comments.add(s.id, { ...at, body: 'Late.' })).toThrow(/archived/);
    expect(env.core.comments.list(s.id)).toHaveLength(3);
  });
});

describe('pushes straight to the base branch', () => {
  let env: TestEnv;
  beforeEach(async () => {
    env = await makeEnv();
  });
  afterEach(async () => {
    await env.cleanup();
  });

  it('finds a push in the transcript, counts its commits with git, and records what it changed per file type, never the paths', async () => {
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'create', name: 'agent-k-local/test/demo' } });
    const s = await env.core.sessions.create(p.id, { name: 'Direct', prompt: 'do it', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'first turn');
    const git = (...a: string[]) => execFileSync('git', a, { cwd: env.repo, encoding: 'utf8' }).trim();
    const from = git('rev-parse', '--short', 'HEAD');
    mkdirSync(join(env.repo, 'src'), { recursive: true });
    writeFileSync(join(env.repo, 'src', 'secret-module.ts'), 'a\nb\n');
    writeFileSync(join(env.repo, 'src', 'Other.TS'), 'c\n');
    writeFileSync(join(env.repo, 'README.md'), '# test\nmore\n');
    writeFileSync(join(env.repo, 'logo.png'), Buffer.from([0, 1, 2, 3, 0, 255]));
    git('add', '.');
    git('commit', '-q', '-m', 'files');
    const to = git('rev-parse', '--short', 'HEAD');
    const result = { stdout: '', stderr: `To https://github.com/acme/demo.git\n   ${from}..${to}  HEAD -> main\n`, interrupted: false };
    writeFileSync(join(env.fakeDir, `${s.claudeSessionId}.jsonl`), JSON.stringify({ type: 'user', timestamp: new Date().toISOString(), toolUseResult: result }) + '\n', { flag: 'a' });
    env.core.sessions.ingestUsage(s.id);
    const row = await waitFor(
      () => get<{ commits: number | null; file_types: string | null }>(env.core.db, 'SELECT commits, file_types FROM pushes WHERE session_id = ?', s.id),
      (r) => r?.commits != null, 10000, 'commit count');
    expect(row?.commits).toBe(1);
    expect(JSON.parse(row!.file_types!)).toEqual([
      { ext: 'ts', files: 2, additions: 3, deletions: 0 },
      { ext: 'md', files: 1, additions: 1, deletions: 0 },
      { ext: 'png', files: 1, additions: 0, deletions: 0 },
    ]);
    expect(row!.file_types).not.toContain('secret-module');
  });
});

describe('with Meko transcript capture off', () => {
  let env: TestEnv;
  beforeEach(async () => {
    env = await makeEnv({ capture: false });
  });
  afterEach(async () => {
    await env.cleanup();
  });

  it('sends no transcripts, runs sessions without the Meko plugin, and gives Claude a conversation_id itself', async () => {
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'create', name: 'agent-k-local/test/demo' } });
    const s = await env.core.sessions.create(p.id, { name: 'Quiet', prompt: 'do it', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'first turn');
    await env.core.sessions.submit(s.id, 'second turn');
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'second turn');
    await env.core.sessions.action(s.id, 'archive');
    expect(captureLog(env)).toEqual([]);
    // 2147483 s is the longest interval Node's setInterval honours; the plugin's timer never fires.
    expect(argvLog(env)[0].env.MEKO_CHECKPOINT_INTERVAL).toBe('2147483');
    // The Meko plugin is off in sessions, so its own hooks send nothing at compaction or the end either.
    const settings = JSON.parse(readFileSync(join(env.core.paths.run, 'hooks.json'), 'utf8'));
    expect(settings.enabledPlugins['meko-agent-skills@meko-agent-skills']).toBe(false);
    // Its SessionStart gave Claude the conversation_id Meko's tools require; the system prompt does now.
    expect(env.meko.calls.filter((c) => c.name === 'ensureConversation' && c.args[2] === s.claudeSessionId)).toHaveLength(1);
    const argv = argvLog(env)[0].args;
    expect(argv[argv.indexOf('--append-system-prompt') + 1]).toContain('Meko conversation_id for this session: "conv-1".');
  });
});
