import { execFileSync } from 'node:child_process';
import { existsSync, mkdirSync, readFileSync, readdirSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { all, openDb, run } from '../../src/server/db.js';
import { getPaths, type Paths } from '../../src/server/paths.js';
import { cliReviewsDir, ingestCliRecords, readCliRecord, type CliReviewRecord } from '../../src/server/review/clirecords.js';
import { runReview, type ReviewIo } from '../../src/server/reviewcli.js';
import { reviewUsage } from '../../src/server/usage.js';
import { FAKE_CLAUDE, gitRepo, readJsonl, tempDir, waitFor } from '../helpers.js';

const LEDGERS = { dependents: { state_ledger: [], summary: 'LGTM', findings: [] }, claims: { claims: [], summary: 'LGTM', findings: [] } };

interface Step {
  args: string[];
  cwd: string;
  pid: number;
  input: string;
  file: string;
  env: { KODE_HOOK_SOCK: string | null; CLAUDECODE: string | null; MEKO_AGENT_ID: string | null };
}

describe('meko kode review', () => {
  let home: string, paths: Paths, repo: string, fakeDir: string;
  let out: string[], err: string[];
  let interrupt: (() => void) | null;

  const fake = (name: string, value: unknown) => writeFileSync(join(fakeDir, name), typeof value === 'string' ? value : JSON.stringify(value));
  const g = (...a: string[]) => execFileSync('git', a, { cwd: repo, stdio: 'ignore' });
  const steps = () => readJsonl<Step>(join(fakeDir, 'review-steps.jsonl'));
  const records = (): CliReviewRecord[] => {
    const dir = cliReviewsDir(paths);
    return existsSync(dir) ? readdirSync(dir).filter((f) => f.endsWith('.json')).map((f) => readCliRecord(readFileSync(join(dir, f), 'utf8'))!) : [];
  };
  const io = (over: Partial<ReviewIo> = {}): ReviewIo => ({
    cwd: repo,
    // As if run from inside a Kode session: none of it may reach the steps.
    env: { ...process.env, FAKE_CLAUDE_DIR: fakeDir, KODE_HOOK_SOCK: join(home, 'session-hook.sock'), KODE_HOOK_TOKEN: 'session-token', CLAUDECODE: '1', MEKO_AGENT_ID: 'claude_code:task' },
    out: (s) => out.push(s),
    err: (s) => err.push(s),
    errTTY: false,
    color: false,
    now: Date.now,
    onInterrupt: (stop) => {
      interrupt = stop;
      return () => {
        interrupt = null;
      };
    },
    meko: async () => null,
    claudeVersion: async () => '2.1.300 (Claude Code)',
    ...over,
  });

  beforeEach(() => {
    home = tempDir('kode-review-cli-');
    paths = getPaths(join(home, 'ak'));
    mkdirSync(paths.home, { recursive: true });
    writeFileSync(paths.config, `[claude]\ncommand = ${JSON.stringify(FAKE_CLAUDE)}\n\n[code_review]\nstrategy = "sagittarius-3.0"\n`);
    fakeDir = join(home, 'fake-claude');
    mkdirSync(fakeDir, { recursive: true });
    repo = gitRepo(join(home, 'demo-repo'));
    g('checkout', '-q', '-b', 'feat/login');
    mkdirSync(join(repo, 'src'));
    writeFileSync(join(repo, 'src', 'a.ts'), ['export function login() {', '  return 1;', '}', '', 'export const x = 2;', ''].join('\n'));
    g('add', '.');
    g('commit', '-q', '-m', 'Add login');
    // Not committed yet: reviewed too.
    writeFileSync(join(repo, 'src', 'b.ts'), 'export const y = 3;\n');
    out = [];
    err = [];
    interrupt = null;
  });

  afterEach(() => {
    execFileSync('rm', ['-rf', home]);
  });

  it('reviews the change against main and prints LGTM when nothing is kept', async () => {
    fake('review-reports.json', LEDGERS);
    expect(await runReview(paths, [], io())).toBe(0);
    expect(err.join('')).toContain('Reviewing feat/login against main: 2 files, +');
    expect(err.join('')).toContain('(Sagittarius 3.0)');
    expect(out.join('\n')).toMatch(/^LGTM · \d+s · \$1\.25$/);
    const s = steps();
    expect(s.map((x) => x.file.match(/"step": "([^"]+)"/)![1]).sort()).toEqual(['r1s1:claims', 'r1s1:dependents', 'r1s1:diff', 'r1s1:guide', 'r1s1:history']);
    for (const x of s) {
      expect(x.cwd).toBe(repo);
      expect(x.env).toEqual({ KODE_HOOK_SOCK: null, CLAUDECODE: null, MEKO_AGENT_ID: null });
      expect(x.args).toContain('--strict-mcp-config');
      expect(x.file).toContain('src/b.ts');
    }
    // Each pass on its tier's model: guide, dependents, history and claims large; diff small.
    expect(s.filter((x) => x.args[x.args.indexOf('--model') + 1] === 'opus')).toHaveLength(4);
    // Recorded for the usage counts; the run's own folder is gone.
    const [rec] = records();
    expect(rec).toMatchObject({ state: 'done', repo: 'demo-repo', branch: 'feat/login', base: 'main', strategy: 'sagittarius-3.0', depth: 1, sweeps: 1, candidates: 0, pr: false });
    expect(rec.usage.costUsd).toBeCloseTo(1.25);
    expect(readdirSync(cliReviewsDir(paths))).toEqual([`${rec.id}.json`]);
  });

  it('prints what it kept, with --json for scripts, and is counted under the repo\'s project', async () => {
    fake('review-reports.json', {
      ...LEDGERS,
      diff: { summary: 'LGTM', findings: [{ file: 'src/a.ts', line: 2, severity: 'major', comment: 'Returns a number where a session is expected.', gotchas: [] }] },
      // Outside the diff: Validate withholds it.
      history: { summary: 'LGTM', findings: [{ file: 'README.md', line: 1, severity: 'minor', comment: 'Not this change.', gotchas: [] }] },
    });
    expect(await runReview(paths, ['--json', '--description', 'Adds the login page.'], io())).toBe(0);
    const r = JSON.parse(out.join('\n'));
    expect(r).toMatchObject({
      repo: 'demo-repo', branch: 'feat/login', base: 'main', strategy: 'sagittarius-3.0', depth: 1, files: 2, counts: { blocker: 0, major: 1, minor: 0 },
      findings: [{ key: '1:1', file: 'src/a.ts', line: 2, severity: 'major', comment: 'Returns a number where a session is expected.', confidence: 90, passes: ['diff'] }],
    });
    // The description reached the passes, quoted as data.
    expect(steps()[0].file).toContain('> Adds the login page.');
    // Two scorers: one per candidate.
    expect(steps().filter((x) => /"step": "r1c\d+"/.test(x.file))).toHaveLength(2);

    const db = openDb(':memory:');
    run(db, "INSERT INTO projects(id, name, repo_path, default_branch, created_at) VALUES ('p1', 'Demo', ?, 'main', 1)", repo);
    expect(ingestCliRecords(db, paths, () => undefined)).toBe(1);
    expect(all(db, 'SELECT project_id, state, candidates, counts_json FROM cli_reviews')).toEqual([
      { project_id: 'p1', state: 'done', candidates: 2, counts_json: JSON.stringify({ blocker: 0, major: 1, minor: 0 }) },
    ]);
    expect(reviewUsage(db, 'p1')).toMatchObject({ runs: 1, cli: 1, findings: { blocker: 0, major: 1, minor: 0 } });
  });

  it("checks the change against --pr's title and description, against its base, without a public repo's MEKO.md", async () => {
    fake('review-reports.json', LEDGERS);
    // gh as --pr reads the PR: its head is the commit before this folder's.
    const bin = join(home, 'bin');
    mkdirSync(bin);
    const pr = { number: 41, url: 'https://github.com/acme/demo-repo/pull/41', title: 'Add the login page', body: 'Users sign in with a token.', baseRefName: 'main', headRefOid: execFileSync('git', ['rev-parse', 'HEAD~1'], { cwd: repo }).toString().trim() };
    writeFileSync(join(bin, 'gh'), `#!/bin/sh\necho "$@" >> ${JSON.stringify(join(home, 'gh-calls'))}\ncat <<'EOF'\n${JSON.stringify(pr)}\nEOF\n`, { mode: 0o755 });
    let mekoAsked = false;
    const env = { ...io().env, PATH: `${bin}:${process.env.PATH}` };
    expect(await runReview(paths, ['--pr', '41'], io({ env, visibility: async () => 'public', meko: async () => ((mekoAsked = true), null) }))).toBe(0);
    expect(readFileSync(join(home, 'gh-calls'), 'utf8')).toBe('pr view 41 --json number,url,title,body,baseRefName,headRefOid\n');
    const said = err.join('');
    expect(said).toContain('Reviewing feat/login against main:');
    expect(said).toContain("This folder has commits PR #41 doesn't have; they're reviewed too.");
    expect(said).toContain("acme/demo-repo is public, so anyone can have written PR #41: the review runs without the repo's MEKO.md datapacks.");
    expect(mekoAsked).toBe(false);
    const file = steps()[0].file;
    expect(file).toContain('# The change: Add the login page');
    expect(file).toContain('> Add the login page\n>\n> Users sign in with a token.');
    expect(records()).toMatchObject([{ state: 'done', pr: true }]);
  });

  it('says so when nothing changed', async () => {
    g('checkout', '-q', 'main');
    execFileSync('rm', [join(repo, 'src', 'b.ts')]);
    expect(await runReview(paths, [], io())).toBe(0);
    expect(out).toEqual(['Nothing changed against main.']);
    expect(steps()).toEqual([]);
    expect(records()).toMatchObject([{ state: 'done', detail: 'nothing changed against the base', sweeps: 0 }]);
  });

  it("refuses outside a git repo, without Claude Code, or with a base that isn't there, recording nothing", async () => {
    expect(await runReview(paths, [], io({ cwd: home }))).toBe(1);
    expect(err.join('')).toContain("This folder isn't in a git repo.");
    err = [];
    expect(await runReview(paths, [], io({ claudeVersion: async () => null }))).toBe(1);
    expect(err.join('')).toContain("meko kode review runs on Claude Code, which isn't installed here");
    err = [];
    expect(await runReview(paths, ['--base', 'nope'], io())).toBe(1);
    expect(err.join('')).toBe("There's no branch or commit nope here. Fetch it, or name another with --base.\n");
    err = [];
    expect(await runReview(paths, ['--pr', '1', '--description', 'x'], io())).toBe(1);
    expect(err.join('')).toBe('Use --pr or --description, not both.\n');
    expect(records()).toEqual([]);
  });

  it("fails when Claude Code isn't signed in, and records the run", async () => {
    fake('review-step-signed-out', '');
    expect(await runReview(paths, [], io())).toBe(1);
    expect(err.join('')).toContain("The review couldn't finish: Claude Code isn't signed in.");
    // Its passes had started: one round, as the usage counts read it.
    expect(records()).toMatchObject([{ state: 'failed', detail: expect.stringContaining("isn't signed in"), sweeps: 1 }]);
  });

  it('stops on Ctrl+C, ending its steps and recording the run as cancelled', async () => {
    fake('review-step-hang', '');
    const done = runReview(paths, [], io());
    await waitFor(() => steps().length, (n) => n >= 5, 20000, 'the passes to start');
    interrupt!();
    expect(await done).toBe(1);
    expect(err.join('')).toContain('Stopped the review.');
    const [rec] = records();
    expect(rec).toMatchObject({ state: 'cancelled', sweeps: 1 });
    expect(readdirSync(cliReviewsDir(paths))).toEqual([`${rec.id}.json`]);
    // No step is left running.
    const alive = (pid: number) => {
      try {
        process.kill(pid, 0);
        return true;
      } catch {
        return false;
      }
    };
    await waitFor(() => steps().filter((x) => alive(x.pid)).length, (n) => n === 0, 10000, 'the steps to end');
  });
});
