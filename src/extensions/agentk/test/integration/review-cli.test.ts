import { execFileSync } from 'node:child_process';
import { existsSync, mkdirSync, readFileSync, readdirSync, rmSync, writeFileSync } from 'node:fs';
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
    // Each pass's run and the scorers', what each spent ($0.25 a step here).
    expect(r.passes).toEqual(['guide', 'diff', 'dependents', 'history', 'claims'].map((name) => ({
      name, sweep: 1, reported: true, findings: name === 'diff' || name === 'history' ? 1 : 0, costUsd: 0.25, error: null,
    })));
    expect(r.scoring).toEqual({ steps: 2, reported: 2, costUsd: 0.5 });
    expect(r.costUsd).toBe(1.75);
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

  it("tells the passes about --prior's earlier rounds, and refuses a file it can't read, recording nothing", async () => {
    fake('review-reports.json', LEDGERS);
    const file = join(home, 'rounds.json');
    writeFileSync(file, JSON.stringify([
      { round: 1, findings: [{ file: 'src/a.ts', line: 2, severity: 'major', comment: 'Returns a number where a session is expected.', outcome: { status: 'declined', note: 'Callers read a number: see src/main.ts:4.' } }] },
    ]));
    expect(await runReview(paths, ['--prior', file], io())).toBe(0);
    const prompt = steps()[0].file;
    expect(prompt).toContain('## Already reviewed \u2014 earlier rounds of this review');
    expect(prompt).toContain('### Round 1');
    expect(prompt).toContain('- **src/a.ts:2** (major)');
    expect(prompt).toContain('  > Returns a number where a session is expected.');
    expect(prompt).toContain('  - the author declined it:\n    > Callers read a number: see src/main.ts:4.');
    expect(records()).toMatchObject([{ state: 'done' }]);

    err = [];
    expect(await runReview(paths, ['--prior', join(home, 'nope.json')], io())).toBe(1);
    expect(err.join('')).toContain(`Couldn't read --prior's file ${join(home, 'nope.json')}: `);
    expect(records()).toHaveLength(1);
  });

  it("gives the passes --issue's issues to read, or their links when gh can't read them", async () => {
    fake('review-reports.json', LEDGERS);
    const asked: string[] = [];
    const issue = async (_cwd: string, repo: string, n: number) => {
      asked.push(`${repo}#${n}`);
      if (n === 8) throw new Error('HTTP 404: Not Found\n(more)');
      return { title: 'Sign in with a token', body: 'Tokens expire after an hour.', url: `https://github.com/${repo}/issues/${n}` };
    };
    expect(await runReview(paths, ['--json', '--issue', 'acme/demo-repo#7', '--issue', 'https://github.com/acme/auth/issues/8'], io({ issue }))).toBe(0);
    expect(asked).toEqual(['acme/demo-repo#7', 'acme/auth#8']);
    expect(err.join('')).toContain("Couldn't read acme/auth#8 with gh (HTTP 404: Not Found); the review gets its link only.");
    const prompt = steps()[0].file;
    expect(prompt).toContain('## Linked issues');
    expect(prompt).toMatch(/ {2}- `[^`]*issues\/acme-demo-repo-7\.md` \u2014 Sign in with a token/);
    expect(prompt).toContain('  - acme/auth#8 (https://github.com/acme/auth/issues/8)');
    expect(JSON.parse(out.join('\n')).issues).toEqual([
      { url: 'https://github.com/acme/demo-repo/issues/7', title: 'Sign in with a token', read: true },
      { url: 'https://github.com/acme/auth/issues/8', title: 'acme/auth#8', read: false },
    ]);

    // A bare number is this repo's, and this one's origin isn't on GitHub.
    err = [];
    expect(await runReview(paths, ['--issue', '7'], io({ issue }))).toBe(1);
    expect(err.join('')).toBe("--issue 7 names no repo, and this folder's origin isn't on GitHub: use owner/name#number.\n");
  });

  it('keeps the run with --out-dir: each step streamed to its own transcript, what the steps read, the result, never the tokens', async () => {
    fake('review-reports.json', {
      ...LEDGERS, diff: { summary: 'LGTM', findings: [{ file: 'src/a.ts', line: 2, severity: 'major', comment: 'Returns a number.', gotchas: [] }] },
    });
    const kept = join(home, 'kept', 'run1');
    expect(await runReview(paths, ['--json', '--out-dir', kept], io())).toBe(0);
    expect(err.join('')).toContain(`Kept the run in ${kept}.`);
    for (const x of steps()) expect(x.args[x.args.indexOf('--output-format') + 1]).toBe('stream-json');
    const files = readdirSync(kept).sort();
    expect(files).toEqual(expect.arrayContaining([
      'base.md', 'r1c1.jsonl', 'r1s1-claims.jsonl', 'r1s1-dependents.jsonl', 'r1s1-diff.jsonl', 'r1s1-guide.jsonl', 'r1s1-history.jsonl',
      'review.json', 's1-claims.md', 's1-diff.md', 'score-1.md',
    ]));
    expect(files).not.toContain('mcp.json');
    const transcript = readFileSync(join(kept, 'r1s1-guide.jsonl'), 'utf8').split('\n');
    expect(JSON.parse(transcript[0])).toMatchObject({ type: 'assistant' });
    expect(JSON.parse(transcript[1])).toMatchObject({ type: 'result', total_cost_usd: 0.25 });
    // The result read from the stream's last line: what each step spent still adds up.
    const r = JSON.parse(out.join('\n'));
    expect(r.costUsd).toBe(1.5);
    expect(JSON.parse(readFileSync(join(kept, 'review.json'), 'utf8'))).toEqual(r);

    // A folder with a run in it already, a file, or a folder in the reviewed repo is refused before anything is recorded.
    err = [];
    expect(await runReview(paths, ['--out-dir', kept], io())).toBe(1);
    expect(err.join('')).toBe(`--out-dir's folder ${kept} isn't empty: name a new or empty one, so one run's files don't mix with another's.\n`);
    err = [];
    expect(await runReview(paths, ['--out-dir', join(kept, 'review.json')], io())).toBe(1);
    expect(err.join('')).toBe(`--out-dir ${join(kept, 'review.json')} isn't a folder: name a new or empty one.\n`);
    for (const inRepo of ['logs/run1', '..runs']) {
      err = [];
      expect(await runReview(paths, ['--out-dir', inRepo], io())).toBe(1);
      expect(err.join('')).toBe(`--out-dir's folder ${inRepo} is in the reviewed repo, whose next review would read it as part of the change: name one outside it.\n`);
    }
    expect(records()).toHaveLength(1);

    // A folder gone by the end (a file in its place): the review still prints its result and is recorded as done.
    const gone = join(home, 'kept', 'run2');
    out = [];
    err = [];
    const meko = async () => {
      rmSync(gone, { recursive: true });
      writeFileSync(gone, '');
      return null;
    };
    expect(await runReview(paths, ['--json', '--out-dir', gone], io({ meko }))).toBe(0);
    expect(JSON.parse(out.join('\n')).costUsd).toBe(1.5);
    expect(err.join('')).toContain(`Couldn't keep the result in ${gone}: `);
    expect(records().map((x) => x.state)).toEqual(['done', 'done']);
  });

  it("runs each tier's steps on --model's model, the config's for the rest", async () => {
    fake('review-reports.json', {
      ...LEDGERS, diff: { summary: 'LGTM', findings: [{ file: 'src/a.ts', line: 2, severity: 'major', comment: 'Returns a number.', gotchas: [] }] },
    });
    expect(await runReview(paths, ['--json', '--model', 'sonnet'], io())).toBe(0);
    const model = (x: Step) => x.args[x.args.indexOf('--model') + 1];
    const byStep = Object.fromEntries(steps().map((x) => [x.file.match(/"step": "([^"]+)"/)![1], model(x)]));
    // The four large passes on sonnet; diff and the scorer stay on the small tier's haiku.
    expect(byStep).toEqual({
      'r1s1:guide': 'sonnet', 'r1s1:dependents': 'sonnet', 'r1s1:history': 'sonnet', 'r1s1:claims': 'sonnet', 'r1s1:diff': 'haiku', r1c1: 'haiku',
    });
    expect(JSON.parse(out.join('\n')).models).toEqual({ large: 'sonnet', mid: 'sonnet', small: 'haiku' });
  });

  it('says which pass never reported and exits 2, so a review short of one is not taken for a whole one', async () => {
    fake('review-reports.json', LEDGERS);
    fake('review-skip', ['r1s1:history']);
    expect(await runReview(paths, [], io())).toBe(2);
    expect(out.join('\n')).toContain("Note: The history pass didn't report, so the review went on without it.");
    out = [];
    expect(await runReview(paths, ['--json'], io())).toBe(2);
    const r = JSON.parse(out.join('\n'));
    expect(r.passes.filter((p: { reported: boolean }) => !p.reported)).toEqual([{ name: 'history', sweep: 1, reported: false, findings: 0, costUsd: 0.5, error: null }]);
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
