import { execFileSync } from 'node:child_process';
import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { makeEnv, waitFor, type TestEnv } from '../helpers.js';
import { postHook } from '../hook-client.js';
import { get, run } from '../../src/server/db.js';
import { sleep } from '../../src/server/tmux.js';
import { transcriptPathFor } from '../../src/server/claude.js';
import { socketId, startHookServer } from '../../src/server/hooks.js';
import { MISSED_START_MS } from '../../src/server/sessions.js';

function argvLog(env: TestEnv): { args: string[]; env: Record<string, string> }[] {
  const f = join(env.fakeDir, 'argv.jsonl');
  return existsSync(f) ? readFileSync(f, 'utf8').trim().split('\n').map((l) => JSON.parse(l)) : [];
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

function captureLog(env: TestEnv): { args: string[]; input: { transcript_path: string }; env: Record<string, string> }[] {
  const f = join(env.fakeDir, 'capture.jsonl');
  return existsSync(f) ? readFileSync(f, 'utf8').trim().split('\n').map((l) => JSON.parse(l)) : [];
}

describe('projects and sessions (real tmux + git, fake claude)', () => {
  let env: TestEnv;
  beforeEach(async () => {
    env = await makeEnv();
  });
  afterEach(async () => {
    await env.cleanup();
  });

  async function newProject() {
    return env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'create', name: 'agent-k-local/test/demo' } });
  }

  it('lists repos and creates a project with a dedicated datapack and Meko pin', async () => {
    const repos = await env.core.projects.repos();
    expect(repos.repos.map((r) => r.name)).toEqual(['demo-repo']);
    const p = await newProject();
    expect(p.id).toBe('demo-repo');
    expect(p.datapacks).toEqual([{ id: 'dp-agent-k-local-test-demo', name: 'agent-k-local/test/demo', role: 'dedicated' }]);
    const pin = JSON.parse(readFileSync(join(process.env.MEKO_WATERMARK_DIR!, 'pin-claude_code_demo-repo.json'), 'utf8'));
    expect(pin.datapack_id).toBe('dp-agent-k-local-test-demo');
    expect((await env.core.projects.repos()).repos[0].projects).toEqual([{ id: 'demo-repo', name: 'Demo' }]);
  });

  it('lets several projects share a repo, each with its own datapack, sessions and worktrees', async () => {
    const first = await newProject();
    const second = await env.core.projects.create({
      repoPath: env.repo, name: 'Release tooling', datapack: { mode: 'create', name: 'agent-k-local/test/release' },
    });
    expect(second.id).toBe('demo-repo-release-tooling');
    expect(second.repoPath).toBe(first.repoPath);
    expect(second.datapacks).toEqual([{ id: 'dp-agent-k-local-test-release', name: 'agent-k-local/test/release', role: 'dedicated' }]);
    const pin = JSON.parse(readFileSync(join(process.env.MEKO_WATERMARK_DIR!, 'pin-claude_code_demo-repo-release-tooling.json'), 'utf8'));
    expect(pin.datapack_id).toBe('dp-agent-k-local-test-release');
    const repos = (await env.core.projects.repos()).repos;
    expect(repos).toHaveLength(1);
    expect(repos[0].projects).toEqual([{ id: 'demo-repo', name: 'Demo' }, { id: 'demo-repo-release-tooling', name: 'Release tooling' }]);

    // Names only have to differ within a repo.
    await expect(env.core.projects.create({ repoPath: env.repo, name: ' demo ', datapack: { mode: 'existing', id: 'dp-x', name: 'x' } }))
      .rejects.toThrow(/already has a project named "Demo"/);

    // The same session name in both projects gets separate branches, worktrees and tmux sessions.
    const a = await env.core.sessions.create(first.id, { name: 'Fix it', prompt: 'hi', kind: 'quick' });
    const b = await env.core.sessions.create(second.id, { name: 'Fix it', prompt: 'hi', kind: 'quick' });
    expect([a.branch, b.branch]).toEqual(['ak/fix-it', 'ak/fix-it-2']);
    expect(a.worktreePath).not.toBe(b.worktreePath);
    expect(a.tmuxName).not.toBe(b.tmuxName);
    await waitFor(() => env.core.sessions.get(b.id).status, (st) => st === 'idle', 10000, 'second project session');
    expect(argvLog(env).find((l) => l.env.MEKO_AGENT_ID === 'claude_code:demo-repo-release-tooling')?.env.MEKO_DATAPACK_ID)
      .toBe('dp-agent-k-local-test-release');

    // Deleting one project leaves the other's session, worktree and branch alone. The deleted
    // project's worktree goes idle: the repo's other project may reuse it.
    await env.core.projects.delete(first.id);
    expect(existsSync(a.worktreePath)).toBe(true);
    expect(env.core.sessions.ownsWorktree(env.core.sessions.row(a.id))).toBe(false);
    expect(existsSync(b.worktreePath)).toBe(true);
    expect((await env.core.tmux.panes()).has(b.tmuxName)).toBe(true);
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
    expect(first.env).toEqual({ MEKO_AGENT_ID: 'claude_code:demo-repo', MEKO_DATAPACK_ID: 'dp-agent-k-local-test-demo' });

    await env.core.sessions.submit(s.id, 'second turn');
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'second turn');

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
      env: { MEKO_MCP_URL: 'https://mcp.mekodata.ai/mcp', MEKO_API_KEY: 'test-key' },
    });
    const capturedBeforeArchive = captureLog(env).length;

    // The session's Meko conversation was created once, titled after the session, keyed by its Claude session id.
    const named = env.meko.calls.filter((c) => c.name === 'ensureConversation' && c.args[2] === s.claudeSessionId);
    expect(named.map((c) => c.args)).toEqual([['claude_code:demo-repo', 'dp-agent-k-local-test-demo', s.claudeSessionId, 'Fix the thing (Demo)']]);

    await env.core.sessions.action(s.id, 'archive');
    expect(captureLog(env).length).toBeGreaterThan(capturedBeforeArchive); // final capture before Claude is stopped
    const panes = await env.core.tmux.panes();
    expect(panes.has(s.tmuxName)).toBe(false);
    // The clean worktree waits for the next session, detached so the branch is free.
    expect(existsSync(s.worktreePath)).toBe(true);
    expect(detached(s.worktreePath)).toBe(true);
    expect(env.core.sessions.ownsWorktree(env.core.sessions.row(s.id))).toBe(false);
    const branches = execFileSync('git', ['branch', '--list', 'ak/*'], { cwd: env.repo, encoding: 'utf8' });
    expect(branches).toContain('ak/fix-the-thing'); // archive keeps the branch
    expect(env.core.state().projects[0].sessions).toHaveLength(0);
    expect(env.core.state().projects[0].usage.messages).toBe(2); // archived sessions still count
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
      status: 409, message: "This session's worktree has 1 uncommitted file. Ask AgentK to commit, or Discard the session to throw the changes away.",
    });
    expect(env.core.sessions.get(s.id).desired).toBe('running');
    expect((await env.core.tmux.panes()).has(s.tmuxName)).toBe(true);

    const g = (...a: string[]) => execFileSync('git', a, { cwd: s.worktreePath, stdio: 'ignore' });
    g('add', 'draft.txt');
    g('commit', '-q', '-m', 'draft');
    await env.core.sessions.action(s.id, 'archive');
    expect(env.core.sessions.get(s.id).desired).toBe('archived');
    expect(execFileSync('git', ['branch', '--list', 'ak/*'], { cwd: env.repo, encoding: 'utf8' })).toContain('ak/keep-me');
  });

  it('discards a session with uncommitted and unpushed work', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Throw away', prompt: 'hi', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'first turn');
    const g = (...a: string[]) => execFileSync('git', a, { cwd: s.worktreePath, stdio: 'ignore' });
    writeFileSync(join(s.worktreePath, 'done.txt'), 'committed\n');
    g('add', 'done.txt');
    g('commit', '-q', '-m', 'local only');
    writeFileSync(join(s.worktreePath, 'draft.txt'), 'wip\n');

    expect(await env.core.sessions.discardCheck(s.id)).toEqual({ worktreeExists: true, uncommittedFiles: 1, unpushedCommits: 1 });

    await env.core.sessions.action(s.id, 'discard');
    expect((await env.core.tmux.panes()).has(s.tmuxName)).toBe(false);
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
  });

  it('shows a session\'s changes until its worktree is gone', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Show changes', prompt: 'hi', kind: 'quick' });
    // The demo repo has no origin, so the base is used as is.
    const created = get<{ text: string }>(env.core.db, "SELECT text FROM session_events WHERE session_id = ? AND kind = 'created'", s.id);
    expect(created?.text).toBe('Started on ak/show-changes from main');
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'first turn');
    writeFileSync(join(s.worktreePath, 'notes.txt'), 'one\ntwo\n');
    const c = await env.core.sessions.changes(s.id);
    expect(c.files).toMatchObject([{ path: 'notes.txt', kind: 'untracked', added: 2, uncommitted: true }]);
    await env.core.sessions.refreshDiff(s.id);
    expect(env.core.sessions.get(s.id).diffStat).toEqual({ added: 2, removed: 0 });
    await env.core.sessions.action(s.id, 'discard');
    await expect(env.core.sessions.changes(s.id)).rejects.toThrow(/worktree is gone/);
  });

  const designRow = (id: string) =>
    get<{ markdown: string; html: string | null; artifact_url: string | null }>(env.core.db, 'SELECT markdown, html, artifact_url FROM designs WHERE session_id = ?', id);
  const transcriptText = (claudeSessionId: string) => readFileSync(join(env.fakeDir, `${claudeSessionId}.jsonl`), 'utf8');

  it('designs outside plan mode, guards the repo, and moves to Code when the design is approved', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Design me', prompt: 'DESIGN_MD it' });
    expect(s).toMatchObject({ kind: 'plan', stage: 'design' });
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
    const guard = readFileSync(join(env.fakeDir, 'guard.jsonl'), 'utf8').trim().split('\n').map((l) => JSON.parse(l));
    expect(JSON.parse(guard[0].out).hookSpecificOutput).toMatchObject({ permissionDecision: 'deny', permissionDecisionReason: expect.stringMatching(/Design stage/) });
    expect(guard[1].out).toBe('');
    expect(env.core.sessions.get(s.id).status).toBe('idle'); // a turn that didn't touch the design isn't "ready" again

    await env.core.sessions.action(s.id, 'approve-design');
    expect(env.core.sessions.get(s.id).stage).toBe('code');
    await waitFor(() => transcriptText(s.claudeSessionId), (t) => t.includes('The design is approved'), 10000, 'approval prompt');
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'turn end after approval');

    // Past Design, the guard lets edits through.
    await env.core.sessions.submit(s.id, 'EDIT_REPO');
    await waitFor(() => readFileSync(join(env.fakeDir, 'guard.jsonl'), 'utf8').trim().split('\n').length, (n) => n === 4, 10000, 'second guard check');
    const after = readFileSync(join(env.fakeDir, 'guard.jsonl'), 'utf8').trim().split('\n').map((l) => JSON.parse(l));
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

  it('refuses to approve a design that does not exist yet', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'No design', prompt: 'just think' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'first turn');
    await expect(env.core.sessions.action(s.id, 'approve-design')).rejects.toThrow(/hasn't written a design/);
  });

  it('runs an Exploration session with no design, PR or merge gates', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Look around', prompt: 'why is it slow', kind: 'explore' });
    expect(s).toMatchObject({ kind: 'explore', stage: 'explore' });
    expect(existsSync(s.worktreePath)).toBe(true);
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'first turn');
    const args = argvLog(env)[0].args;
    const system = args[args.indexOf('--append-system-prompt') + 1];
    expect(system).toContain('This is an Exploration session');
    expect(system).not.toContain('Merge & Close');
    expect(args.at(-1)).toMatch(/Exploration session[\s\S]*Task: why is it slow/);

    await expect(env.core.sessions.action(s.id, 'approve-design')).rejects.toMatchObject({ status: 409 });
    await expect(env.core.sessions.action(s.id, 'open-pr')).rejects.toMatchObject({ status: 409 });
    expect(env.core.sessions.get(s.id).stage).toBe('explore');

    // Edits in the repo go ahead (POCs), and review comments on the diff reach Claude.
    await env.core.sessions.submit(s.id, 'EDIT_REPO');
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'edit turn');
    const guard = readFileSync(join(env.fakeDir, 'guard.jsonl'), 'utf8').trim().split('\n').map((l) => JSON.parse(l));
    expect(guard.every((g) => g.out === '')).toBe(true);
    env.core.comments.add(s.id, { path: 'a.txt', side: 'new', line: 1, endLine: 1, excerpt: 'a', body: 'Try it another way.' });
    expect(await env.core.comments.send(s.id)).toEqual({ sent: 1, queued: 0 });
    await waitFor(() => transcriptText(s.claudeSessionId), (t) => t.includes('Try it another way.'), 10000, 'comment prompt');
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'comment turn');

    await env.core.sessions.action(s.id, 'archive');
    expect(env.core.sessions.get(s.id)).toMatchObject({ status: 'archived', stage: 'explore' });
  });

  it('starts Quick Code in Code, keeps skipDesign working, and refuses an unknown session type', async () => {
    const p = await newProject();
    const quick = await env.core.sessions.create(p.id, { name: 'Quick one', prompt: 'hi', kind: 'quick' });
    expect(quick).toMatchObject({ kind: 'quick', stage: 'code' });
    await waitFor(() => argvLog(env), (l) => l.length > 0, 10000, 'launch');
    const args = argvLog(env)[0].args;
    expect(args[args.indexOf('--append-system-prompt') + 1]).toContain('Stages: Code → PR → Review → Merge & Close;');
    const legacy = await env.core.sessions.create(p.id, { name: 'Old client', prompt: 'hi', skipDesign: true });
    expect(legacy).toMatchObject({ kind: 'quick', stage: 'code' });
    await expect(env.core.sessions.create(p.id, { name: 'Odd', kind: 'yolo' as never })).rejects.toMatchObject({ status: 400 });
  });

  it('uses the name as the first prompt of a task session started without one, but not of an exploration', async () => {
    const p = await newProject();
    const quick = await env.core.sessions.create(p.id, { name: 'Fix the typo', prompt: '  ', kind: 'quick' });
    await waitFor(() => argvLog(env), (l) => l.length === 1, 10000, 'quick launch');
    expect(argvLog(env)[0].args.at(-1)).toMatch(/^Task: Fix the typo/);
    const plan = await env.core.sessions.create(p.id, { name: 'Add retries' });
    await waitFor(() => argvLog(env), (l) => l.length === 2, 10000, 'plan launch');
    expect(argvLog(env)[1].args.at(-1)).toMatch(/Task: Add retries/);
    const explore = await env.core.sessions.create(p.id, { name: 'Why slow', kind: 'explore' });
    await waitFor(() => argvLog(env), (l) => l.length === 3, 10000, 'explore launch');
    expect(argvLog(env)[2].args).not.toContain('--'); // a prompt always follows "--"
    const rows = [quick, plan, explore].map((s) => env.core.sessions.row(s.id).initial_prompt);
    expect(rows).toEqual(['Fix the typo', 'Add retries', null]);
    await expect(env.core.sessions.create(p.id, { name: 'Typed', prompt: 123 as never })).rejects.toMatchObject({ status: 400 });
  });

  it('reports startup steps while a session starts', async () => {
    const p = await newProject();
    const steps: string[] = [];
    const onChange = () => {
      const s = env.core.state().projects[0]?.sessions[0];
      if (s?.startup && steps.at(-1) !== s.startup.step) steps.push(s.startup.step);
    };
    env.core.on('change', onChange);
    const s = await env.core.sessions.create(p.id, { name: 'Startup steps', prompt: 'hi', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'first turn');
    env.core.off('change', onChange);
    expect(steps.slice(0, 3)).toEqual(['worktree', 'launch', 'loading']);
    expect(steps).toContain('prompt');
    expect(env.core.sessions.get(s.id).startup).toBeNull();
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

  it('marks exited sessions and resumes them with --resume', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Exit test', prompt: 'hi', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'idle');
    await env.core.tmux.submit(s.tmuxName, 'exit');
    await waitFor(async () => {
      await env.core.sessions.reconcile();
      return env.core.sessions.get(s.id).status;
    }, (st) => st === 'exited', 10000, 'exited');
    const before = argvLog(env).length;
    await env.core.sessions.action(s.id, 'resume');
    await waitFor(() => argvLog(env).length, (n) => n > before, 10000, 'resume launch');
    const last = argvLog(env).at(-1)!;
    expect(last.args).toEqual(expect.arrayContaining(['--resume', s.claudeSessionId]));
  });

  it('recovers after tmux dies: auto-continues mid-turn sessions, with a 15 minute guard', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Crash test', prompt: 'hi', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'idle');

    // Simulate a host crash in the middle of a turn.
    let launches = argvLog(env).length;
    run(env.core.db, "UPDATE sessions SET status = 'working' WHERE id = ?", s.id);
    execFileSync('tmux', ['-L', env.socket, 'kill-server']);
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
    execFileSync('tmux', ['-L', env.socket, 'kill-server']);
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
    expect((await env.core.tmux.panes()).has(s.tmuxName)).toBe(false);
    expect(env.core.sessions.ownsWorktree(env.core.sessions.row(s.id))).toBe(false);
    expect(execFileSync('git', ['branch', '--list', 'ak/*'], { cwd: env.repo, encoding: 'utf8' })).not.toContain('ak/late');
    expect(env.core.sessions.row(s.id)).toMatchObject({ desired: 'archived', status: 'archived' });

    // If it is archived anyway before Claude starts, Claude isn't started.
    env.meko.ensureConversation = async () => {
      run(env.core.db, "UPDATE sessions SET desired = 'archived', status = 'archived' WHERE id = ?", 'demo-repo--gone');
      return 'conv-2';
    };
    await expect(env.core.sessions.create(p.id, { name: 'Gone', prompt: 'hi', kind: 'quick' })).rejects.toThrow(/archived while it was starting/);
    expect((await env.core.tmux.panes()).has('ak-demo-repo--gone')).toBe(false);
    expect(env.core.sessions.row('demo-repo--gone').status).toBe('archived');
  });

  it('keeps the tmux sessions of two projects on one repo apart', async () => {
    const first = await newProject();
    const second = await env.core.projects.create({ repoPath: env.repo, name: 'X', datapack: { mode: 'create', name: 'agent-k-local/test/x' } });
    expect(second.id).toBe('demo-repo-x');
    // "demo-repo" + "x-fix" and "demo-repo-x" + "fix" used to share the tmux name ak-demo-repo-x-fix.
    const a = await env.core.sessions.create(first.id, { name: 'X fix', prompt: 'hi', kind: 'quick' });
    const b = await env.core.sessions.create(second.id, { name: 'Fix', prompt: 'hi', kind: 'quick' });
    expect([a.tmuxName, b.tmuxName]).toEqual(['ak-demo-repo--x-fix', 'ak-demo-repo-x--fix']);
    await waitFor(() => [env.core.sessions.get(a.id).status, env.core.sessions.get(b.id).status], (st) => st.every((x) => x === 'idle'), 10000, 'both sessions');
    const panes = await env.core.tmux.panes();
    expect([panes.get(a.tmuxName)?.dead, panes.get(b.tmuxName)?.dead]).toEqual([false, false]);

    // A name some other session (such as one from before this naming) holds is never reused.
    await env.core.sessions.action(a.id, 'archive');
    run(env.core.db, 'UPDATE sessions SET tmux_name = ? WHERE id = ?', 'ak-demo-repo-x--dup', a.id);
    const c = await env.core.sessions.create(second.id, { name: 'Dup', prompt: 'hi', kind: 'quick' });
    expect(c.tmuxName).toBe('ak-demo-repo-x--dup-2');
  });

  it('never relaunches over a live Claude, and skips a reconcile tick when tmux fails', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Live', prompt: 'hi', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'idle');
    const launches = argvLog(env).length;
    await expect(env.core.sessions.launch(env.core.sessions.row(s.id), { resume: true, prompt: null })).rejects.toThrow(/already running/);
    const panes = env.core.tmux.panes.bind(env.core.tmux);
    env.core.tmux.panes = async () => {
      throw new Error('tmux hiccup');
    };
    await expect(env.core.sessions.reconcile()).rejects.toThrow(/hiccup/);
    env.core.tmux.panes = panes;
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
    execFileSync('tmux', ['-L', env.socket, 'kill-server']);
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

  it('follows /clear to the new conversation, so Resume continues that one', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Clear me', prompt: 'hi', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'idle');
    await env.core.tmux.submit(s.tmuxName, '/clear');
    const moved = await waitFor(() => env.core.sessions.row(s.id).claude_session_id, (id) => id !== s.claudeSessionId, 10000, 'new session id');
    expect(env.core.sessions.get(s.id).status).toBe('idle'); // /clear's SessionEnd isn't an exit
    await env.core.tmux.submit(s.tmuxName, 'exit');
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
    execFileSync('tmux', ['-L', env.socket, 'kill-server']);
    await env.core.sessions.reconcile();
    expect(env.core.sessions.get(s.id).status).toBe('recovered');
    await waitFor(async () => {
      await env.core.sessions.reconcile();
      return env.core.sessions.get(s.id);
    }, (x) => x.status === 'needs_input' && /trust this folder/.test(x.statusDetail ?? ''), 15000, 'trust prompt noticed');
    rmSync(join(env.fakeDir, 'trust-prompt'));
    await env.core.tmux.sendKeys(s.tmuxName, 'Enter');
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
    execFileSync('tmux', ['-L', env.socket, 'kill-server']);
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
    execFileSync('tmux', ['-L', env.socket, 'kill-server']);
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
    const prompts = transcriptText(s.claudeSessionId).trim().split('\n').map((l) => JSON.parse(l)).filter((e) => e.type === 'user').map((e) => e.text);
    expect(prompts.at(-1)).toBe('first one');
  });

  it('re-pins the dedicated datapack when a session starts', async () => {
    const p = await newProject();
    const pin = join(process.env.MEKO_WATERMARK_DIR!, 'pin-claude_code_demo-repo.json');
    rmSync(pin);
    await env.core.sessions.create(p.id, { name: 'Pinned', prompt: 'hi', kind: 'quick' });
    expect(JSON.parse(readFileSync(pin, 'utf8')).datapack_id).toBe('dp-agent-k-local-test-demo');
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
    expect((await env.core.tmux.panes()).has(dirty.tmuxName)).toBe(false);
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
    const prompt = transcriptText(s.claudeSessionId).trim().split('\n').map((l) => JSON.parse(l)).filter((e) => e.type === 'user').at(-1).text.replace(/\s+/g, ' ');
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

  it('finds a push in the transcript and counts its commits with git', async () => {
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'create', name: 'agent-k-local/test/demo' } });
    const s = await env.core.sessions.create(p.id, { name: 'Direct', prompt: 'do it', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'first turn');
    const git = (...a: string[]) => execFileSync('git', a, { cwd: env.repo, encoding: 'utf8' }).trim();
    const from = git('rev-parse', '--short', 'HEAD');
    git('commit', '--allow-empty', '-q', '-m', 'one');
    git('commit', '--allow-empty', '-q', '-m', 'two');
    const to = git('rev-parse', '--short', 'HEAD');
    const result = { stdout: '', stderr: `To https://github.com/acme/demo.git\n   ${from}..${to}  HEAD -> main\n`, interrupted: false };
    writeFileSync(join(env.fakeDir, `${s.claudeSessionId}.jsonl`), JSON.stringify({ type: 'user', timestamp: new Date().toISOString(), toolUseResult: result }) + '\n', { flag: 'a' });
    env.core.sessions.ingestUsage(s.id);
    const row = await waitFor(
      () => get<{ commits: number | null }>(env.core.db, 'SELECT commits FROM pushes WHERE session_id = ?', s.id),
      (r) => r?.commits != null, 10000, 'commit count');
    expect(row?.commits).toBe(2);
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

  it('sends no transcripts and keeps the Meko plugin timer from sending any', async () => {
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'Demo', datapack: { mode: 'create', name: 'agent-k-local/test/demo' } });
    const s = await env.core.sessions.create(p.id, { name: 'Quiet', prompt: 'do it', kind: 'quick' });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'first turn');
    await env.core.sessions.submit(s.id, 'second turn');
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'second turn');
    await env.core.sessions.action(s.id, 'archive');
    expect(captureLog(env)).toEqual([]);
    expect(env.meko.calls.filter((c) => c.name === 'ensureConversation' && c.args[2] === s.claudeSessionId)).toEqual([]);
    // 2147483 s is the longest interval Node's setInterval honours; the plugin's timer never fires.
    expect(argvLog(env)[0].env.MEKO_CHECKPOINT_INTERVAL).toBe('2147483');
  });
});
