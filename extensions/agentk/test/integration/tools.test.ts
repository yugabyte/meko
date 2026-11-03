import { execFileSync } from 'node:child_process';
import { join } from 'node:path';
import { Client } from '@modelcontextprotocol/sdk/client/index.js';
import { InMemoryTransport } from '@modelcontextprotocol/sdk/inMemory.js';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { IssueOrPr } from '../../src/server/github.js';

// GitHub is mocked; git (with "GitHub" repos as local bare repos), the session host and the fake claude are real.
const gh = vi.hoisted(() => ({ items: new Map<string, IssueOrPr>() }));

vi.mock('../../src/server/github.js', async (orig) => {
  const real = await orig<typeof import('../../src/server/github.js')>();
  return {
    ...real,
    issueOrPr: async (_cwd: string, repo: string, n: number) => {
      const v = gh.items.get(`${repo}#${n}`);
      if (!v) throw new Error(`HTTP 404: Not Found (${repo}#${n})`);
      return v;
    },
    repoVisibility: async () => 'private',
    prWatch: async () => ({ state: 'OPEN', headRefOid: '', commits: [], reviews: [], body: '' }),
    viewerLogin: async () => 'me',
    issueText: async () => ({ title: '', body: '' }),
  };
});

const { makeEnv, gitRepo, readJsonl, waitFor } = await import('../helpers.js');
const { handle } = await import('../../src/server/mcp.js');
const { all, run } = await import('../../src/server/db.js');
const { toolsMcpConfigPath, toolsPluginDir } = await import('../../src/server/tools.js');
type TestEnv = Awaited<ReturnType<typeof makeEnv>>;

const git = (cwd: string, ...a: string[]) => execFileSync('git', a, { cwd, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] }).trim();

describe("AgentK's session tools through me_koder mcp (real session host + git, fake claude, GitHub mocked)", () => {
  let env: TestEnv;
  let projectId: string;
  let lead: string;
  let abc: string;

  const issue = (n: number, title: string) => gh.items.set(`acme/demo#${n}`, {
    kind: 'issue', number: n, url: `https://github.com/acme/demo/issues/${n}`, title, author: 'dana', state: 'open', body: '', authorAssociation: 'MEMBER',
  } as IssueOrPr);
  const idle = (id: string) => waitFor(() => env.core.sessions.get(id), (x) => x.status === 'idle' || x.status === 'needs_input', 10000, 'first turn');
  /**
   * The tools as an agent in `sessionId` sees them: `me_koder mcp` with that session's token, its
   * messages carried in memory from the MCP SDK's own client.
   */
  async function connect(sessionId: string | null, token?: string) {
    const link = { sock: env.core.paths.hookSock, token: token ?? (sessionId ? env.core.sessions.row(sessionId).hook_token : undefined) };
    const [a, b] = InMemoryTransport.createLinkedPair();
    a.onmessage = (m) => void handle(link, m as Parameters<typeof handle>[1]).then((r) => (r ? a.send(r as never) : undefined));
    await a.start();
    const client = new Client({ name: 'test', version: '1' });
    await client.connect(b);
    return {
      client,
      async call(name: string, args: Record<string, unknown> = {}) {
        const r = await client.callTool({ name, arguments: args }) as { content: { text: string }[]; isError?: boolean };
        return { text: r.content[0].text, isError: !!r.isError };
      },
    };
  }
  const events = (id: string) => all<{ text: string }>(env.core.db, "SELECT text FROM session_events WHERE session_id = ? AND kind = 'tools' ORDER BY id", id).map((e) => e.text);

  beforeEach(async () => {
    gh.items.clear();
    env = await makeEnv();
    // "github.com/acme/demo": a bare repo, reached through url.insteadOf, cloned under the scanned repos folder.
    const seed = gitRepo(join(env.home, 'seed'));
    git(env.home, 'clone', '-q', '--bare', seed, 'acme-demo.git');
    const upstream = join(env.home, 'acme-demo.git');
    const clone = join(env.home, 'code', 'demo');
    git(env.home, '-c', `url.${upstream}.insteadOf=https://github.com/acme/demo.git`, 'clone', '-q', 'https://github.com/acme/demo.git', clone);
    git(clone, 'config', '--add', `url.${upstream}.insteadOf`, 'https://github.com/acme/demo.git');
    projectId = (await env.core.projects.create({ repoPath: clone, name: 'Demo', datapack: { mode: 'none' } })).id;
    lead = (await env.core.sessions.create(projectId, { name: 'Lead', prompt: 'lead the work', start: 'explore' })).id;
    abc = (await env.core.sessions.create(projectId, { name: 'ABC cleanup', prompt: 'clean up ABC', start: 'design' })).id;
    await idle(lead);
    await idle(abc);
  });
  afterEach(async () => {
    await env.cleanup();
  });

  it('lists, starts sessions on issues and a task in one call, moves sessions (its own at its turn\'s end), and never opens its own PR', async () => {
    const t = await connect(lead);
    const { tools } = await t.client.listTools();
    expect(tools.map((x) => [x.name, x.annotations?.readOnlyHint])).toEqual([
      ['list', true], ['start_sessions', false], ['move_session', false], ['open_pr', false], ['open_project', false],
      ['review_report', true], ['review_score', true], ['review_outcome', true],
    ]);
    const listed = JSON.parse((await t.call('list')).text);
    expect(listed.projects).toEqual([expect.objectContaining({
      id: projectId, name: 'Demo', github: 'acme/demo',
      sessions: [expect.objectContaining({ id: lead, name: 'Lead', stage: 'explore', self: true }), expect.not.objectContaining({ self: true })],
    })]);

    // "Open sessions for #1 and #5, and one to update the docs, and move the ABC session to Code."
    issue(1, 'Fix login');
    issue(5, 'Dark mode');
    const started = await t.call('start_sessions', {
      sessions: [
        { ref: 'acme/demo#1' },
        { ref: 'https://github.com/acme/demo/issues/5', stage: 'code', autoRun: true },
        // Run to merge is only for a start at Code: left out here, and said so.
        { name: 'Docs', prompt: 'Update the docs', autoRun: true },
      ],
    });
    expect(started).toEqual({ isError: false, text: [
      "Left out Run to merge for Docs: it's only for a task that starts at Code.",
      'Started a task on acme/demo#1, "#1 Fix login", in Demo.',
      'Started a task on https://github.com/acme/demo/issues/5, "#5 Dark mode", in Demo, with Run to merge.',
      'Started "Docs" in Demo at Design.',
    ].join('\n') });
    const byName = (name: string) => env.core.sessions.listForProject(projectId).find((s) => s.name === name)!;
    expect(byName('#5 Dark mode')).toMatchObject({ stage: 'code', autoRun: true });
    expect(byName('Docs')).toMatchObject({ stage: 'design', autoRun: false });
    // An issue a running task already works on isn't started twice; one GitHub doesn't have fails alone.
    const again = await t.call('start_sessions', { sessions: [{ ref: 'acme/demo#1' }, { ref: 'acme/demo#404' }] });
    expect(again.isError).toBe(true);
    expect(again.text).toMatch(/^Couldn't start a session on acme\/demo#1: issue #1 of acme\/demo already has a session, "#1 Fix login"\.\nCouldn't start a session on acme\/demo#404: Couldn't find #404/);

    expect(await t.call('move_session', { sessionId: abc, stage: 'code' })).toEqual({ isError: false, text: 'Moved "ABC cleanup" to Code.' });
    expect(env.core.sessions.get(abc).stage).toBe('code');
    expect(events(abc)).toEqual(['Moved to Code by the agent in Lead']);
    // Its own move waits for its turn to end: the agent asked mid-turn.
    expect((await t.call('move_session', { sessionId: lead, stage: 'code' })).text).toBe('Moving "Lead" to Code once this turn ends.');
    expect(env.core.sessions.get(lead).stage).toBe('explore');
    await env.core.sessions.submit(lead, 'carry on');
    await waitFor(() => events(lead), (e) => e.includes('Moved to Code by its agent, at the turn\'s end'), 10000, 'the move at the turn\'s end');
    expect(env.core.sessions.get(lead).stage).toBe('code');
    expect(await t.call('open_pr', { sessionId: lead })).toEqual({
      isError: true, text: "Couldn't open the PR for \"Lead\": a session doesn't open its own PR: the user does, with Open PR in AgentK",
    });
    expect(events(lead)).toEqual([
      'Started a task on acme/demo#1, "#1 Fix login", in Demo.',
      'Started a task on https://github.com/acme/demo/issues/5, "#5 Dark mode", in Demo, with Run to merge.',
      'Started "Docs" in Demo at Design.',
      expect.stringMatching(/^Couldn't start a session on acme\/demo#1/),
      expect.stringMatching(/^Couldn't start a session on acme\/demo#404/),
      'Moved "ABC cleanup" to Code.',
      'Moving "Lead" to Code once this turn ends.',
      'Moved to Code by its agent, at the turn\'s end',
      expect.stringMatching(/^Couldn't open the PR for "Lead"/),
    ]);
  });

  it('refuses calls with the tools turned off, from a session on an untrusted PR head, and outside a session', async () => {
    // Claude Code sessions launch with the tools' MCP server and skill plugin while they're on, and without them once they're off.
    const launches = () => readJsonl<{ args: string[] }>(join(env.fakeDir, 'argv.jsonl')).map((l) => l.args);
    const withTools = ['--plugin-dir', toolsPluginDir(env.core.paths), '--mcp-config', toolsMcpConfigPath(env.core.paths)];
    expect(launches()[0]).toEqual(expect.arrayContaining(withTools));
    const t = await connect(abc);
    env.core.config.tools.enabled = false;
    await idle((await env.core.sessions.create(projectId, { name: 'Off', prompt: 'no tools', start: 'explore' })).id);
    expect(launches().at(-1)).not.toContain('--mcp-config');
    expect(launches().at(-1)).not.toContain(toolsPluginDir(env.core.paths));
    expect(await t.call('list')).toEqual({ isError: true, text: "AgentK's session tools are turned off (Settings → General in AgentK)." });
    env.core.config.tools.enabled = true;
    // A PR head from a public repo, and one not checked yet (a PR linked since the launch), can't use them.
    for (const untrusted of [1, null]) {
      run(env.core.db, "UPDATE sessions SET untrusted = ?, branch_reused = 1, pr_number = 7, pr_url = 'https://github.com/acme/demo/pull/7' WHERE id = ?", untrusted, abc);
      expect((await t.call('move_session', { sessionId: lead, stage: 'code' })).text).toMatch(/^This session works on a PR from a public repo, or one AgentK hasn't checked yet/);
    }
    expect(env.core.sessions.get(lead).stage).toBe('explore');
    run(env.core.db, 'UPDATE sessions SET untrusted = NULL, branch_reused = 0, pr_number = NULL, pr_url = NULL WHERE id = ?', abc);
    // A call still running when the hook socket closes (a stop, a restart) is answered then, so it doesn't hold the socket open.
    issue(2, 'Slow');
    env.core.githubLinks.start = () => new Promise(() => undefined);
    const slow = t.call('start_sessions', { sessions: [{ ref: 'acme/demo#2' }] });
    await new Promise((r) => setTimeout(r, 300));
    env.core.tools.pause();
    expect((await slow).text).toMatch(/^AgentK is restarting, so it couldn't wait for this/);
    expect((await (await connect(null)).call('list')).text).toBe("This isn't an AgentK session, so AgentK's session tools can't be used here.");
    expect((await (await connect(null, 'not-a-token')).call('list')).text).toBe('AgentK no longer runs this session, so its session tools are off.');
  });

  it('opens projects on a scanned repo or a GitHub repo, never another folder, and runs Do all through the route', async () => {
    const t = await connect(lead);
    const other = gitRepo(join(env.home, 'code', 'other'));
    expect(await t.call('open_project', { repo: other, datapack: 'none' })).toEqual({ isError: false, text: `Opened other on ${other}.` });
    // owner/name with a clone here uses the clone; a new datapack falls back to none at the plan's limit.
    (env.meko as unknown as { createDatapack: () => Promise<never> }).createDatapack = async () => {
      throw new Error('FREE_TIER_DATAPACK_LIMIT: Upgrade to Standard to create a new datapack');
    };
    const opened = await t.call('open_project', { repo: 'acme/demo', name: 'Demo 2' });
    expect(opened).toEqual({ isError: false, text: `Opened Demo 2 on ${join(env.home, 'code', 'demo')}.` });
    expect(env.core.projects.get(env.core.actionProjects().find((p) => p.name === 'Demo 2')!.id).datapacks).toEqual([]);
    expect((await t.call('open_project', { repo: join(env.home, 'elsewhere') })).text)
      .toMatch(/isn't a repo in your repos folders, a GitHub owner\/name or a clone URL$/);

    // Do all: go actions left out, the rest checked again and run in order, one result each.
    const { buildServer } = await import('../../src/server/server.js');
    const app = await buildServer(env.core, { webDir: null, updates: null });
    try {
      const runActions = (actions: unknown[]) => app.inject({ method: 'POST', url: '/api/actions/run', payload: { actions } });
      const r = await runActions([
        { type: 'go', view: 'usage' },
        { type: 'move', sessionId: abc, stage: 'code' },
        { type: 'move', sessionId: 'gone', stage: 'code' },
        { type: 'start', projectId, name: 'Docs', prompt: 'Update the docs', start: 'design' },
      ]);
      expect(r.statusCode).toBe(200);
      expect(r.json()).toEqual({
        results: [
          { ok: true, text: 'Moved "ABC cleanup" to Code.', projectId, sessionId: abc },
          { ok: true, text: 'Started "Docs" in Demo at Design.', projectId, sessionId: expect.any(String) },
        ],
        notes: ["Left out a move of a session that isn't running."],
      });
      expect((await runActions(Array(9).fill({ type: 'go', view: 'usage' }))).statusCode).toBe(400);
      expect((await runActions([{ type: 'go', view: 'usage' }])).json()).toMatchObject({ error: 'There was nothing to run.' });
    } finally {
      await app.close();
    }
  });
});
