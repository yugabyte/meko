import { execFileSync } from 'node:child_process';
import { cpSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { hostname, tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { FastifyInstance } from 'fastify';
import { makeEnv, waitFor, type TestEnv } from '../helpers.js';
import { DEFAULT_CONFIG, type Config } from '../../src/server/config.js';
import type { HarnessStatus } from '../../src/shared/types.js';
import { buildServer, stampVersion } from '../../src/server/server.js';
import { shortHost } from '../../src/server/core.js';
import { issueSetupToken, totpCode } from '../../src/server/auth.js';

describe('HTTP API: auth, origin checks, projects', () => {
  let env: TestEnv;
  let app: FastifyInstance;
  beforeEach(async () => {
    // OpenCode offered too, as on an install that has it, so Settings can turn it off.
    env = await makeEnv({ opencode: true });
    app = await buildServer(env.core, { tls: null, webDir: null, updater: null });
  });
  afterEach(async () => {
    await app.close();
    await env.cleanup();
  });

  const cookieOf = (res: { headers: Record<string, unknown> }) => String(res.headers['set-cookie']).split(';')[0];

  async function setup(): Promise<string> {
    const token = issueSetupToken(env.core.db);
    const res = await app.inject({ method: 'POST', url: '/api/auth/setup', payload: { token, username: 'me', password: 'correct horse battery' } });
    expect(res.statusCode).toBe(204);
    return cookieOf(res);
  }

  it('requires setup, then login; rejects reuse of a setup token', async () => {
    const st = await app.inject({ url: '/api/auth/status' });
    expect(st.json()).toMatchObject({ setupRequired: true, loggedIn: false });
    expect((await app.inject({ url: '/api/state' })).statusCode).toBe(401);

    const token = issueSetupToken(env.core.db);
    const weak = await app.inject({ method: 'POST', url: '/api/auth/setup', payload: { token, username: 'me', password: 'short' } });
    expect(weak.statusCode).toBe(400);
    const ok = await app.inject({ method: 'POST', url: '/api/auth/setup', payload: { token, username: 'me', password: 'correct horse battery' } });
    expect(ok.statusCode).toBe(204);
    const cookie = cookieOf(ok);
    expect(String(ok.headers['set-cookie'])).toMatch(/HttpOnly/i);
    expect(String(ok.headers['set-cookie'])).toMatch(/SameSite=Strict/i);
    const again = await app.inject({ method: 'POST', url: '/api/auth/setup', payload: { token, username: 'x', password: 'correct horse battery' } });
    expect(again.statusCode).toBe(403);

    const state = await app.inject({ url: '/api/state', headers: { cookie } });
    expect(state.statusCode).toBe(200);
    expect(state.json().version).toBe('0.0.0-test');
    expect(state.json().meko.webUrl).toBe('https://cloud.mekodata.ai');

    const bad = await app.inject({ method: 'POST', url: '/api/auth/login', payload: { username: 'me', password: 'nope' } });
    expect(bad.statusCode).toBe(401);
    const good = await app.inject({ method: 'POST', url: '/api/auth/login', payload: { username: 'me', password: 'correct horse battery' } });
    expect(good.statusCode).toBe(204);
  });

  it('accepts an email address as the username and refuses other characters', async () => {
    const setupAs = (username: string) =>
      app.inject({ method: 'POST', url: '/api/auth/setup', payload: { token: issueSetupToken(env.core.db), username, password: 'correct horse battery' } });
    for (const bad of ['jane doe', 'jane/doe', 'jane+tag@example.com', '', 'a'.repeat(65)]) {
      const res = await setupAs(bad);
      expect(res.statusCode, bad).toBe(400);
      expect(res.json().error).toMatch(/@/);
    }
    expect((await setupAs('jane.doe@example.com')).statusCode).toBe(204);
    const login = await app.inject({ method: 'POST', url: '/api/auth/login', payload: { username: 'jane.doe@example.com', password: 'correct horse battery' } });
    expect(login.statusCode).toBe(204);
    const enroll = await app.inject({ method: 'POST', url: '/api/auth/totp/enroll', headers: { cookie: cookieOf(login) } });
    expect(enroll.json().otpauthUrl).toContain(':jane.doe%40example.com?');
  });

  it('says a setup link replaces the existing login, so the setup screen can offer a reset', async () => {
    const status = async () => (await app.inject({ url: '/api/auth/status' })).json();
    expect(await status()).toMatchObject({ setupRequired: true, reset: false });
    const cookie = await setup();
    expect(await status()).toMatchObject({ setupRequired: false, reset: false });

    // Forgot password: `agent-k-local setup-link` on the host issues a token for a reset.
    const token = issueSetupToken(env.core.db);
    expect(await status()).toMatchObject({ setupRequired: true, reset: true });
    expect((await app.inject({ url: '/api/auth/status', headers: { cookie } })).json()).toMatchObject({ setupRequired: false, reset: false });

    const res = await app.inject({ method: 'POST', url: '/api/auth/setup', payload: { token, username: 'me2', password: 'a brand new passphrase' } });
    expect(res.statusCode).toBe(204);
    expect(await status()).toMatchObject({ setupRequired: false, reset: false });
    // The reset signed out the old browser, and only the new password works.
    expect((await app.inject({ url: '/api/state', headers: { cookie } })).statusCode).toBe(401);
    const old = await app.inject({ method: 'POST', url: '/api/auth/login', payload: { username: 'me', password: 'correct horse battery' } });
    expect(old.statusCode).toBe(401);
    const next = await app.inject({ method: 'POST', url: '/api/auth/login', payload: { username: 'me2', password: 'a brand new passphrase' } });
    expect(next.statusCode).toBe(204);
  });

  it('rate limits failed logins, but takes a valid setup link meanwhile, so a locked-out owner gets back in', async () => {
    await setup();
    for (let i = 0; i < 5; i++) {
      await app.inject({ method: 'POST', url: '/api/auth/login', payload: { username: 'me', password: 'wrong' } });
    }
    const login = await app.inject({ method: 'POST', url: '/api/auth/login', payload: { username: 'me', password: 'correct horse battery' } });
    expect(login.statusCode).toBe(429);
    const reset = (token: string) => app.inject({ method: 'POST', url: '/api/auth/setup', payload: { token, username: 'me', password: 'a brand new passphrase' } });
    const token = issueSetupToken(env.core.db);
    // A wrong token is still refused by the limit, and doesn't use up the real one.
    expect((await reset('not-the-token')).statusCode).toBe(429);
    expect((await reset(token)).statusCode).toBe(204);
    expect((await reset(token)).statusCode).toBe(429);
  });

  it('enforces TOTP once enrolled', async () => {
    const cookie = await setup();
    const enroll = await app.inject({ method: 'POST', url: '/api/auth/totp/enroll', headers: { cookie } });
    const { secret, qrSvg } = enroll.json();
    expect(qrSvg).toContain('<svg');
    const confirm = await app.inject({ method: 'POST', url: '/api/auth/totp/confirm', headers: { cookie }, payload: { code: totpCode(secret) } });
    expect(confirm.statusCode).toBe(204);
    const noCode = await app.inject({ method: 'POST', url: '/api/auth/login', payload: { username: 'me', password: 'correct horse battery' } });
    expect(noCode.statusCode).toBe(401);
    expect(noCode.json().totpRequired).toBe(true);
    // The code that confirmed enrollment is used up; the next one (within the drift window) works once.
    const reused = await app.inject({ method: 'POST', url: '/api/auth/login', payload: { username: 'me', password: 'correct horse battery', totp: totpCode(secret) } });
    expect(reused.statusCode).toBe(401);
    const next = totpCode(secret, Date.now() + 30_000);
    const withCode = await app.inject({ method: 'POST', url: '/api/auth/login', payload: { username: 'me', password: 'correct horse battery', totp: next } });
    expect(withCode.statusCode).toBe(204);
    const replay = await app.inject({ method: 'POST', url: '/api/auth/login', payload: { username: 'me', password: 'correct horse battery', totp: next } });
    expect(replay.statusCode).toBe(401);
  });

  it('returns one project, and 404 once it is closed', async () => {
    const p = await env.core.projects.create({ repoPath: env.repo, name: 'One', datapack: { mode: 'none' } });
    const cookie = await setup();
    const res = await app.inject({ url: `/api/projects/${p.id}`, headers: { cookie } });
    expect(res.statusCode).toBe(200);
    expect(res.json()).toMatchObject({ id: p.id, name: 'One', repoPath: env.repo });
    await env.core.projects.delete(p.id);
    expect((await app.inject({ url: `/api/projects/${p.id}`, headers: { cookie } })).statusCode).toBe(404);
  });

  it('shows the host name as the server name until Settings sets one', async () => {
    const cookie = await setup();
    const state = async () => (await app.inject({ url: '/api/state', headers: { cookie } })).json();
    const put = (serverName: unknown) => app.inject({ method: 'PUT', url: '/api/settings', headers: { cookie }, payload: { serverName } });
    expect(await state()).toMatchObject({ serverName: shortHost(hostname()), serverHost: hostname() });
    expect((await app.inject({ url: '/api/settings', headers: { cookie } })).json()).toMatchObject({ serverName: '', hostName: shortHost(hostname()) });

    const saved = await put('  work laptop  ');
    expect(saved.statusCode).toBe(200);
    expect(saved.json().serverName).toBe('work laptop');
    expect((await state()).serverName).toBe('work laptop');
    expect(env.core.config.server.name).toBe('work laptop');

    for (const bad of ['x'.repeat(65), 'two\nlines', 5]) {
      const res = await put(bad);
      expect(res.statusCode).toBe(400);
      expect(res.json().error).toMatch(/Server name must be 64 characters or fewer/);
    }
    expect((await state()).serverName).toBe('work laptop');

    expect((await put('')).json().serverName).toBe('');
    expect((await state()).serverName).toBe(shortHost(hostname()));
  });

  it.each([
    ['usage stats', 'callHomeEnabled', (c: Config) => c.callHome.enabled, '[call_home]\nenabled = false', 'call_home'],
    ['resolving merge conflicts', 'resolveConflicts', (c: Config) => c.pr.resolveConflicts, '[pr]\nresolve_conflicts = false', 'resolve_conflicts'],
    // Its section also holds the fake's command, which the test sets.
    ['offering OpenCode', 'opencodeEnabled', (c: Config) => c.opencode.enabled, /\[opencode\][^[]*enabled = false/, /\[opencode\][^[]*enabled/],
    ["OpenCode's tracking", 'opencodeDisableTracking', (c: Config) => c.opencode.disableTracking, /\[opencode\][^[]*disable_tracking = true/, 'disable_tracking'],
  ] as const)('changes %s from Settings and back', async (_name, key, live, written, tomlKey) => {
    const cookie = await setup();
    const settings = async () => (await app.inject({ url: '/api/settings', headers: { cookie } })).json()[key];
    const put = (value: unknown) => app.inject({ method: 'PUT', url: '/api/settings', headers: { cookie }, payload: { [key]: value } });
    // The default, which config.toml leaves out.
    const from = live(DEFAULT_CONFIG);
    expect(await settings()).toBe(from);

    const changed = await put(!from);
    expect(changed.statusCode).toBe(200);
    expect(changed.json()[key]).toBe(!from);
    expect(live(env.core.config)).toBe(!from);
    expect(readFileSync(env.core.paths.config, 'utf8')).toMatch(written);
    expect(await settings()).toBe(!from);

    const bad = await put('no');
    expect(bad.statusCode).toBe(400);
    expect(bad.json().error).toBe(`${key} must be true or false.`);

    expect((await put(from)).json()[key]).toBe(from);
    expect(readFileSync(env.core.paths.config, 'utf8')).not.toMatch(tomlKey);
  });

  it("shows usage stats off while YB_DISABLE_CALLHOME turns them off", async () => {
    const cookie = await setup();
    const settings = async () => (await app.inject({ url: '/api/settings', headers: { cookie } })).json();
    expect((await settings()).callHomeOffBy).toBeUndefined();
    process.env.YB_DISABLE_CALLHOME = 'true';
    try {
      expect(await settings()).toMatchObject({ callHomeEnabled: true, callHomeOffBy: 'YB_DISABLE_CALLHOME' });
    } finally {
      delete process.env.YB_DISABLE_CALLHOME;
    }
  });

  it('Check again reads a harness version that changed since the last read', async () => {
    const cookie = await setup();
    env.core.updateConfig((c) => {
      c.kiro.enabled = true;
    });
    const kiro = async () => {
      const res = await app.inject({ method: 'POST', url: '/api/harnesses/refresh', headers: { cookie } });
      return (res.json() as HarnessStatus[]).find((h) => h.id === 'kiro');
    };
    expect(await kiro()).toMatchObject({ version: '2.32.0', available: true });
    // Kiro went back to an older version: read again now, not when the 10-minute-old read expires.
    mkdirSync(process.env.FAKE_KIRO_DIR!, { recursive: true });
    writeFileSync(join(process.env.FAKE_KIRO_DIR!, 'version'), 'kiro-cli 2.0.0\n');
    expect(await kiro()).toMatchObject({ version: '2.0.0', available: false });
    expect(env.core.state().harnesses.find((h) => h.id === 'kiro')?.version).toBe('2.0.0');
  });

  it('creates a project and a session through the API', async () => {
    const cookie = await setup();
    const repos = (await app.inject({ url: '/api/repos', headers: { cookie } })).json();
    expect(repos.repos).toHaveLength(1);
    expect(repos.repos[0].projects).toEqual([]);
    const p = await app.inject({
      method: 'POST', url: '/api/projects', headers: { cookie },
      payload: { repoPath: repos.repos[0].path, name: 'Demo', datapack: { mode: 'existing', id: 'dp-1', name: 'team/demo' } },
    });
    expect(p.statusCode).toBe(200);
    const pid = p.json().id;
    const attach = await app.inject({ method: 'POST', url: `/api/projects/${pid}/datapacks`, headers: { cookie }, payload: { id: 'dp-shared', name: 'team/kb' } });
    expect(attach.json().datapacks.map((d: { role: string }) => d.role)).toEqual(['dedicated', 'shared']);
    const detachDedicated = await app.inject({ method: 'DELETE', url: `/api/projects/${pid}/datapacks/dp-1`, headers: { cookie } });
    expect(detachDedicated.statusCode).toBe(400);
    const s = await app.inject({ method: 'POST', url: `/api/projects/${pid}/sessions`, headers: { cookie }, payload: { name: 'API session', start: 'code' } });
    expect(s.statusCode).toBe(200);
    // It answers before the worktree is made: the session's tab shows the startup steps meanwhile.
    expect(s.json()).toMatchObject({ kind: 'plan', stage: 'code', status: 'starting', startup: { step: 'worktree' } });
    const started = (id: string) =>
      waitFor(() => env.core.sessions.get(id).startup?.step, (st) => st !== 'worktree' && st !== 'launch', 10000, `${id} to start`);
    await started(s.json().id);
    const badKind = await app.inject({ method: 'POST', url: `/api/projects/${pid}/sessions`, headers: { cookie }, payload: { name: 'Odd', kind: 'yolo' } });
    expect(badKind.statusCode).toBe(400);
    expect(badKind.json().error).toMatch(/Pick a session kind/);
    const badStart = await app.inject({ method: 'POST', url: `/api/projects/${pid}/sessions`, headers: { cookie }, payload: { name: 'Odd', start: 'review' } });
    expect(badStart.statusCode).toBe(400);
    expect(badStart.json().error).toBe('Pick where the session starts: explore, design, code.');
    const repoBranches = await app.inject({ url: `/api/repos/branches?path=${encodeURIComponent(repos.repos[0].path)}`, headers: { cookie } });
    expect(repoBranches.json()).toEqual({ defaultBranch: 'main', branches: expect.arrayContaining(['main', 'ak/api-session']) });
    expect((await app.inject({ url: '/api/repos/branches?path=/nowhere', headers: { cookie } })).statusCode).toBe(400);
    expect((await app.inject({ url: '/api/repos/branches', headers: { cookie } })).statusCode).toBe(400);
    const named = await app.inject({
      method: 'POST', url: `/api/projects/${pid}/sessions`, headers: { cookie },
      payload: { name: 'Named', kind: 'quick', repoPath: repos.repos[0].path, branch: 'feature/named' },
    });
    expect(named.json()).toMatchObject({ branch: 'feature/named', repoPath: repos.repos[0].path });
    await started(named.json().id);
    const taken = await app.inject({
      method: 'POST', url: `/api/projects/${pid}/sessions`, headers: { cookie }, payload: { name: 'Again', kind: 'quick', branch: 'feature/named' },
    });
    expect(taken.statusCode).toBe(409);
    // A worktree that can't be made once it has answered is a failed start in the state, until dismissed.
    // (ak/no-base/x leaves git no room for the branch ak/no-base.)
    execFileSync('git', ['branch', 'ak/no-base/x'], { cwd: repos.repos[0].path });
    const failing = await app.inject({
      method: 'POST', url: `/api/projects/${pid}/sessions`, headers: { cookie }, payload: { name: 'No base', start: 'code' },
    });
    expect(failing.statusCode).toBe(200);
    await waitFor(() => env.core.state().failedStarts, (f) => f.length === 1, 10000, 'the failed start');
    expect((await app.inject({ url: '/api/state', headers: { cookie } })).json().failedStarts).toMatchObject([{ id: failing.json().id, name: 'No base' }]);
    const dismiss = `/api/failed-starts/${encodeURIComponent(failing.json().id)}`;
    expect((await app.inject({ method: 'DELETE', url: dismiss, headers: { cookie } })).statusCode).toBe(204);
    expect(env.core.state().failedStarts).toEqual([]);
    const home = (await app.inject({ url: `/api/projects/${pid}/home`, headers: { cookie } })).json();
    expect(home.datapacks[0].memoryCount).toBe(3);
    expect(home.activity.length).toBeGreaterThan(0);
    const unknown = await app.inject({ method: 'POST', url: `/api/sessions/${s.json().id}/actions/explode`, headers: { cookie } });
    expect(unknown.statusCode).toBe(400);
    const changes = await app.inject({ url: `/api/sessions/${s.json().id}/changes`, headers: { cookie } });
    expect(changes.statusCode).toBe(200);
    expect(changes.json()).toMatchObject({ base: 'main', files: [], added: 0, removed: 0, ahead: 0, suggestedBase: null });
    expect((await app.inject({ url: `/api/sessions/${s.json().id}/base-branches`, headers: { cookie } })).json()).toContain('main');

    // Renaming: behind the origin check, the body checked.
    const rename = (payload: unknown, headers: Record<string, string> = { cookie }) =>
      app.inject({ method: 'PATCH', url: `/api/sessions/${s.json().id}`, headers, payload: payload as object });
    expect((await rename({ name: 'Renamed' }, { cookie, origin: 'https://evil.example' })).statusCode).toBe(403);
    expect((await rename({ name: ['x'] })).statusCode).toBe(400);
    expect((await rename({ name: '' })).json().error).toBe('Give the session a name.');
    expect((await app.inject({ method: 'PATCH', url: '/api/sessions/nope', headers: { cookie }, payload: { name: 'x' } })).statusCode).toBe(404);
    const renamed = await rename({ name: 'Renamed' });
    expect(renamed.statusCode).toBe(200);
    expect(renamed.json()).toMatchObject({ name: 'Renamed', branch: 'ak/api-session' });
    // Started without a first prompt, its task was its name, and it follows the new one.
    expect(env.core.sessions.row(s.json().id).initial_prompt).toBe('Renamed');

    // Review comments: create, edit and delete drafts.
    const url = `/api/sessions/${s.json().id}/comments`;
    const draft = { path: 'a.ts', side: 'new', line: 1, endLine: 1, excerpt: 'x', body: 'Why?' };
    const bad = await app.inject({ method: 'POST', url, headers: { cookie }, payload: { ...draft, body: ' ' } });
    expect(bad.statusCode).toBe(400);
    expect(bad.json().error).toMatch(/Write something/);
    const made = await app.inject({ method: 'POST', url, headers: { cookie }, payload: draft });
    expect(made.statusCode).toBe(200);
    expect(made.json()).toMatchObject({ path: 'a.ts', body: 'Why?', state: 'draft' });
    const cid = made.json().id;
    const edited = await app.inject({ method: 'PATCH', url: `${url}/${cid}`, headers: { cookie }, payload: { body: 'Why this?' } });
    expect(edited.json().body).toBe('Why this?');
    expect((await app.inject({ url, headers: { cookie } })).json()).toHaveLength(1);
    const state = (await app.inject({ url: '/api/state', headers: { cookie } })).json();
    expect(state.projects[0].sessions[0].comments).toEqual({ drafts: 1, queued: 0 });
    expect((await app.inject({ method: 'DELETE', url: `${url}/${cid}`, headers: { cookie } })).statusCode).toBe(204);
    expect((await app.inject({ method: 'DELETE', url: `${url}/${cid}`, headers: { cookie } })).statusCode).toBe(404);
    const nothing = await app.inject({ method: 'POST', url: `${url}/send`, headers: { cookie } });
    expect(nothing.statusCode).toBe(409);

    // Answers to the design: only in Design (this session started at Code). They
    // aren't stored, so there's nothing to list, change or delete.
    const design = await app.inject({ url: `/api/sessions/${s.json().id}/design`, headers: { cookie } });
    expect(design.json()).toMatchObject({ markdown: null, questions: [], questionsSource: null });
    const answers = `/api/sessions/${s.json().id}/design/answers`;
    const feedback = { answers: [], feedback: 'Hi' };
    const early = await app.inject({ method: 'POST', url: answers, headers: { cookie }, payload: feedback });
    expect(early.statusCode).toBe(409);
    expect(early.json().error).toMatch(/only be sent while the session is in Design/);
    expect((await app.inject({ url: answers, headers: { cookie } })).statusCode).toBe(404);
    expect((await app.inject({ method: 'PUT', url: `${answers}/_feedback`, headers: { cookie }, payload: { text: 'Hi' } })).statusCode).toBe(404);
    expect((await app.inject({ method: 'POST', url: `${answers}/send`, headers: { cookie } })).statusCode).toBe(404);
    const approve = await app.inject({ method: 'POST', url: `/api/sessions/${s.json().id}/actions/approve-design`, headers: { cookie }, payload: feedback });
    expect(approve.statusCode).toBe(409);
    expect(approve.json().error).toMatch(/past the Design stage/);

    // Image previews: images only, served so the browser never runs them.
    const sid = s.json().id;
    const img = (q: string) => app.inject({ url: `/api/sessions/${sid}/changes/image?${q}`, headers: { cookie } });
    writeFileSync(join(s.json().worktreePath, 'pic.png'), Buffer.from([0x89, 0x50, 0x4e, 0x47, 1, 2, 3]));
    expect((await img('path=pic.png&side=sideways')).statusCode).toBe(400);
    expect((await img('path=README.md&side=new')).statusCode).toBe(400);
    expect((await img('path=pic.png&side=old')).statusCode).toBe(404);
    const pic = await img('path=pic.png&side=new');
    expect(pic.statusCode).toBe(200);
    expect(pic.headers['content-type']).toBe('image/png');
    expect(pic.headers['content-security-policy']).toBe("default-src 'none'; sandbox");
    expect(pic.headers['x-content-type-options']).toBe('nosniff');
    expect(pic.rawPayload).toEqual(Buffer.from([0x89, 0x50, 0x4e, 0x47, 1, 2, 3]));
    // A commit the branch doesn't have: the same 404, with what to do, from the diff and its images.
    const gone = await img(`path=pic.png&side=new&commit=${'0'.repeat(40)}`);
    expect(gone.statusCode).toBe(404);
    expect(gone.json().error).toMatch(/Pick All changes/);
    const goneDiff = await app.inject({ url: `/api/sessions/${sid}/changes?commit=${'0'.repeat(40)}`, headers: { cookie } });
    expect(goneDiff.statusCode).toBe(404);
    expect(goneDiff.json().error).toMatch(/Pick All changes/);

    // Pasted images: behind the origin check, images only, saved for Claude to read by path.
    const png = Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 1, 2, 3]);
    const paste = (payload: Buffer | string | object, headers: Record<string, string> = {}) =>
      app.inject({ method: 'POST', url: `/api/sessions/${sid}/images`, headers: { cookie, 'content-type': 'image/png', ...headers }, payload });
    expect((await paste(png, { origin: 'https://evil.example' })).statusCode).toBe(403);
    expect((await paste('just text', { 'content-type': 'text/plain' })).statusCode).toBe(415);
    expect((await paste({ path: '/etc/passwd' }, { 'content-type': 'application/json' })).statusCode).toBe(415);
    const notImage = await paste(Buffer.from('<svg/>'), { 'content-type': 'image/svg+xml' });
    expect(notImage.statusCode).toBe(415);
    expect(notImage.json().error).toBe('Only PNG, JPEG, GIF and WebP images can be pasted.');
    expect((await paste(png, { 'content-type': 'image/png' })).statusCode).toBe(200);
    // The type comes from the bytes: a browser calling it something else doesn't matter.
    const pasted = await paste(png, { 'content-type': 'image/webp' });
    expect(pasted.statusCode).toBe(200);
    const saved: string = pasted.json().path;
    expect(saved.startsWith(join(env.core.paths.pastes, sid) + '/')).toBe(true);
    expect(saved).toMatch(/\.png$/);
    expect(readFileSync(saved)).toEqual(png);
    expect((await app.inject({ method: 'POST', url: '/api/sessions/nope/images', headers: { cookie, 'content-type': 'image/png' }, payload: png })).statusCode).toBe(404);
    // Archiving the session removes its images, and it takes no more. (The preview's file goes first: it's uncommitted.)
    rmSync(join(s.json().worktreePath, 'pic.png'));
    expect((await app.inject({ method: 'POST', url: `/api/sessions/${sid}/actions/archive`, headers: { cookie } })).statusCode).toBe(200);
    expect(existsSync(join(env.core.paths.pastes, sid))).toBe(false);
    expect((await paste(png)).statusCode).toBe(409);

    // Images added in the New session dialog: behind the origin check, staged under the project.
    const stage = (payload: Buffer | string, headers: Record<string, string> = {}, project = pid) =>
      app.inject({ method: 'POST', url: `/api/projects/${project}/images`, headers: { cookie, 'content-type': 'image/png', ...headers }, payload });
    expect((await stage(png, { origin: 'https://evil.example' })).statusCode).toBe(403);
    expect((await stage('just text', { 'content-type': 'text/plain' })).statusCode).toBe(415);
    expect((await stage(Buffer.from('<svg/>'), { 'content-type': 'image/svg+xml' })).statusCode).toBe(415);
    expect((await stage(png, {}, 'nope')).statusCode).toBe(404);
    const staged = await stage(png);
    expect(staged.statusCode).toBe(200);
    const id: string = staged.json().id;
    expect(id).toMatch(/^\d{8}-\d{6}-[0-9a-f]{8}\.png$/);
    const withImage = await app.inject({
      method: 'POST', url: `/api/projects/${pid}/sessions`, headers: { cookie }, payload: { name: 'With an image', start: 'code', images: [id] },
    });
    expect(withImage.statusCode).toBe(200);
    expect(existsSync(join(env.core.paths.pastes, withImage.json().id, id))).toBe(true);
    // An id is used once: it's moved into the session's folder.
    const reused = await app.inject({
      method: 'POST', url: `/api/projects/${pid}/sessions`, headers: { cookie }, payload: { name: 'Again', start: 'code', images: [id] },
    });
    expect(reused.statusCode).toBe(400);
  });
});

describe('web app: manifest and icons for the installed app', () => {
  const web = fileURLToPath(new URL('../../src/web/', import.meta.url));
  let env: TestEnv;
  let app: FastifyInstance;
  let webDir: string;
  beforeEach(async () => {
    env = await makeEnv();
    // Lay the files out the way `vite build` does: index.html plus public/ at the root.
    webDir = mkdtempSync(join(tmpdir(), 'ak-web-'));
    cpSync(join(web, 'index.html'), join(webDir, 'index.html'));
    cpSync(join(web, 'public'), webDir, { recursive: true });
    app = await buildServer(env.core, { tls: null, webDir, updater: null });
  });
  afterEach(async () => {
    await app.close();
    await env.cleanup();
    rmSync(webDir, { recursive: true, force: true });
  });

  /** Width and height from a PNG's IHDR chunk. */
  const pngSize = (b: Buffer) => ({ png: b.subarray(1, 4).toString() === 'PNG', w: b.readUInt32BE(16), h: b.readUInt32BE(20) });

  it('links the manifest from index.html, and serves it and its square icons without sign-in', async () => {
    expect((await app.inject({ url: '/' })).body).toContain('<link rel="manifest" href="/manifest.webmanifest" />');
    const res = await app.inject({ url: '/manifest.webmanifest' });
    expect(res.statusCode).toBe(200);
    expect(res.headers['content-type']).toMatch(/^application\/manifest\+json/);
    const manifest = res.json() as { name: string; short_name: string; icons: { src: string; sizes: string; purpose: string }[] };
    expect(manifest).toMatchObject({ name: 'AgentK', short_name: 'AgentK', start_url: '/', display: 'standalone' });
    expect(manifest.icons.map((i) => `${i.sizes} ${i.purpose}`)).toEqual(['192x192 any', '512x512 any', '512x512 maskable']);
    for (const icon of manifest.icons) {
      const img = await app.inject({ url: icon.src });
      expect(img.statusCode, icon.src).toBe(200);
      expect(img.headers['content-type']).toBe('image/png');
      const [w, h] = icon.sizes.split('x').map(Number);
      expect(pngSize(img.rawPayload), icon.src).toEqual({ png: true, w, h });
    }
    for (const [url, size] of [['/icons/favicon-32.png', 32], ['/icons/apple-touch-icon.png', 180]] as const) {
      const img = await app.inject({ url });
      expect(img.statusCode, url).toBe(200);
      expect(pngSize(img.rawPayload), url).toEqual({ png: true, w: size, h: size });
    }
  });

  it('serves the service worker as a script at the root, so its scope is the whole app', async () => {
    const res = await app.inject({ url: '/sw.js' });
    expect(res.statusCode).toBe(200);
    expect(res.headers['content-type']).toMatch(/javascript/);
    expect(res.body).toContain('notificationclick');
    // It must never cache: no fetch handler, so the app and API always come from the server.
    expect(res.body).not.toMatch(/addEventListener\(['"]fetch/);
  });

  it('serves the icons phones and browsers look for at the root', async () => {
    const ico = await app.inject({ url: '/favicon.ico' });
    expect(ico.statusCode).toBe(200);
    expect(ico.rawPayload.subarray(0, 4)).toEqual(Buffer.from([0, 0, 1, 0])); // an ICO header
    for (const url of ['/apple-touch-icon.png', '/apple-touch-icon-precomposed.png']) {
      const img = await app.inject({ url });
      expect(img.statusCode, url).toBe(200);
      expect(img.headers['content-type'], url).toBe('image/png');
      expect(pngSize(img.rawPayload), url).toEqual({ png: true, w: 180, h: 180 });
    }
    expect((await app.inject({ url: '/' })).body).toContain('href="/favicon.ico"');
  });

  it('answers a missing file with 404, and app routes with the app', async () => {
    const missing = await app.inject({ url: '/icons/nope.png' });
    expect(missing.statusCode).toBe(404);
    expect(missing.headers['content-type']).not.toMatch(/html/);
    for (const url of ['/p/demo', '/p/demo/s/s1/design', '/p/v1.2/home']) {
      const res = await app.inject({ url });
      expect(res.statusCode, url).toBe(200);
      expect(res.headers['content-type'], url).toMatch(/^text\/html/);
    }
  });

  it('stamps its version into every page, never cached, so an open page can tell it was updated', async () => {
    for (const url of ['/', '/p/demo/s/s1', '/usage']) {
      const res = await app.inject({ url });
      expect(res.statusCode, url).toBe(200);
      expect(res.headers['content-type'], url).toMatch(/^text\/html/);
      expect(res.headers['cache-control'], url).toBe('no-cache');
      expect(res.body, url).toMatch(/<meta name="ak-version" content="0\.0\.0-test" \/>\s*<\/head>/);
      expect(res.body.match(/ak-version/g), url).toHaveLength(1);
    }
    expect((await app.inject({ method: 'HEAD', url: '/' })).statusCode).toBe(200);
    expect((await app.inject({ method: 'POST', url: '/' })).statusCode).toBe(404);
  });

  it("sends every page a policy that keeps other servers' images, connections, plugins and framing out", async () => {
    for (const url of ['/', '/p/demo/s/s1', '/index.html']) {
      const csp = String((await app.inject({ url, headers: { host: 'box:7777' } })).headers['content-security-policy']);
      expect(csp, url).toContain("img-src 'self' data: blob: https://github.com https://*.githubusercontent.com");
      expect(csp, url).toContain("connect-src 'self' wss://box:7777 ws://box:7777;");
      expect(csp, url).toContain("object-src 'none'");
      expect(csp, url).toContain("frame-ancestors 'none'");
      // No script-src: the design panel's Artifact frame takes this policy too, and runs scripts of its own.
      expect(csp, url).not.toContain('script-src');
    }
  });
});

describe('stampVersion', () => {
  it('escapes the version for the attribute', () => {
    expect(stampVersion('<head></head>', '1"<x>&')).toBe('<head>  <meta name="ak-version" content="1&#34;&#60;x&#62;&#38;" />\n  </head>');
  });
});
