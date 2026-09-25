import { execFileSync } from 'node:child_process';
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { makeEnv, waitFor, type TestEnv } from '../helpers.js';
import { get, run } from '../../src/server/db.js';

function argvLog(env: TestEnv): { args: string[]; env: Record<string, string> }[] {
  const f = join(env.fakeDir, 'argv.jsonl');
  return existsSync(f) ? readFileSync(f, 'utf8').trim().split('\n').map((l) => JSON.parse(l)) : [];
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
    expect((await env.core.projects.repos()).repos[0].projectId).toBe('demo-repo');
    await expect(newProject()).rejects.toThrow(/already belongs/);
  });

  it('refuses to create a project when Meko is down', async () => {
    env.meko.check = async () => {
      throw new Error('boom');
    };
    env.core.mekoStatus = { ...env.core.mekoStatus, ok: false, error: 'boom', checkedAt: Date.now() };
    await expect(newProject()).rejects.toThrow(/Meko is unreachable/);
  });

  it('runs a session in a worktree, tracks status from hooks, and archives it', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Fix the thing', prompt: 'do it', skipDesign: true });
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
    expect(existsSync(s.worktreePath)).toBe(false);
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
    const s = await env.core.sessions.create(p.id, { name: 'Name me', prompt: 'hi', skipDesign: true });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'first turn');
    expect(launchedBefore).toEqual([0]);

    const u = await env.core.sessions.create(p.id, { name: 'Unnamed', prompt: 'hi', skipDesign: true });
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
    const s = await env.core.sessions.create(p.id, { name: 'Race me', prompt: 'hi', skipDesign: true });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'first turn');
    const recovered = get<{ n: number }>(env.core.db, "SELECT COUNT(*) AS n FROM session_events WHERE session_id = ? AND kind = 'recovered'", s.id);
    expect(recovered?.n).toBe(0);
    expect(argvLog(env)).toHaveLength(1);
  });

  it('archive keeps a worktree with uncommitted changes, and its branch', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Keep me', prompt: 'hi', skipDesign: true });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'first turn');
    writeFileSync(join(s.worktreePath, 'draft.txt'), 'wip\n');
    await env.core.sessions.action(s.id, 'archive');
    expect(existsSync(join(s.worktreePath, 'draft.txt'))).toBe(true);
    expect(execFileSync('git', ['branch', '--list', 'ak/*'], { cwd: env.repo, encoding: 'utf8' })).toContain('ak/keep-me');
  });

  it('discards a session with uncommitted and unpushed work', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Throw away', prompt: 'hi', skipDesign: true });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'first turn');
    const g = (...a: string[]) => execFileSync('git', a, { cwd: s.worktreePath, stdio: 'ignore' });
    writeFileSync(join(s.worktreePath, 'done.txt'), 'committed\n');
    g('add', 'done.txt');
    g('commit', '-q', '-m', 'local only');
    writeFileSync(join(s.worktreePath, 'draft.txt'), 'wip\n');

    expect(await env.core.sessions.discardCheck(s.id)).toEqual({ worktreeExists: true, uncommittedFiles: 1, unpushedCommits: 1 });

    await env.core.sessions.action(s.id, 'discard');
    expect((await env.core.tmux.panes()).has(s.tmuxName)).toBe(false);
    expect(existsSync(s.worktreePath)).toBe(false);
    expect(execFileSync('git', ['branch', '--list', 'ak/*'], { cwd: env.repo, encoding: 'utf8' })).not.toContain('ak/throw-away');
    expect(env.core.state().projects[0].sessions).toHaveLength(0);
    expect(env.core.state().projects[0].usage.messages).toBeGreaterThan(0); // discarded sessions still count
    const last = get<{ kind: string }>(env.core.db, 'SELECT kind FROM session_events WHERE session_id = ? ORDER BY id DESC LIMIT 1', s.id);
    expect(last?.kind).toBe('discarded');
    await expect(env.core.sessions.action(s.id, 'discard')).rejects.toThrow(/archived/);
  });

  it('shows a session\'s changes until its worktree is gone', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Show changes', prompt: 'hi', skipDesign: true });
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
    expect(s.stage).toBe('design');
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

  it('reports startup steps while a session starts', async () => {
    const p = await newProject();
    const steps: string[] = [];
    const onChange = () => {
      const s = env.core.state().projects[0]?.sessions[0];
      if (s?.startup && steps.at(-1) !== s.startup.step) steps.push(s.startup.step);
    };
    env.core.on('change', onChange);
    const s = await env.core.sessions.create(p.id, { name: 'Startup steps', prompt: 'hi', skipDesign: true });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'first turn');
    env.core.off('change', onChange);
    expect(steps.slice(0, 3)).toEqual(['worktree', 'launch', 'loading']);
    expect(steps).toContain('prompt');
    expect(env.core.sessions.get(s.id).startup).toBeNull();
  });

  it('marks exited sessions and resumes them with --resume', async () => {
    const p = await newProject();
    const s = await env.core.sessions.create(p.id, { name: 'Exit test', prompt: 'hi', skipDesign: true });
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
    const s = await env.core.sessions.create(p.id, { name: 'Crash test', prompt: 'hi', skipDesign: true });
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
    const s = await env.core.sessions.create(p.id, { name: 'Keep running', prompt: 'hi', skipDesign: true });
    await waitFor(() => env.core.sessions.get(s.id).status, (st) => st === 'idle', 10000, 'idle');
    const launches = argvLog(env).length;
    await waitFor(() => captureLog(env).length, (n) => n >= 1, 10000, 'turn capture');
    const captured = captureLog(env).length;
    await env.core.sessions.recover();
    expect(argvLog(env).length).toBe(launches);
    await waitFor(() => captureLog(env).length, (n) => n > captured, 10000, 'catch-up capture on boot');
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
    const s = await env.core.sessions.create(p.id, { name: 'Limits', prompt: 'STATUSLINE', skipDesign: true });
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
    const s = await env.core.sessions.create(p.id, { name: 'Review me', prompt: 'hi', skipDesign: true });
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
