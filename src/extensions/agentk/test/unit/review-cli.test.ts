import { mkdirSync, readFileSync, readdirSync, statSync, utimesSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';
import { noCounts, type CodeReviewFinding } from '../../src/shared/codereview.js';
import { reviewStepArgs } from '../../src/server/claude.js';
import { all, openDb, run } from '../../src/server/db.js';
import { handle, onlyTools } from '../../src/server/mcp.js';
import { getPaths } from '../../src/server/paths.js';
import {
  CLI_RECORD_MAX_AGE_MS, CLI_RUN_DIR_MAX_AGE_MS, cliReviewsDir, ingestCliRecords, projectForCheckout, readCliRecord, sweepOldRecords, writeCliRecord, type CliReviewRecord,
} from '../../src/server/review/clirecords.js';
import { HeadlessReview, ReviewFailed, noUsage, readStepResult, type HeadlessStep, type StepRunner } from '../../src/server/review/headless.js';
import { moreSweeps } from '../../src/server/review/merge.js';
import { strategyFor } from '../../src/server/review/strategies.js';
import type { Diff, FileChange, ReviewContext } from '../../src/server/review/types.js';
import { UsageError, dollars, duration, formatFindings, parseReviewArgs, progressText, stepEnv, untrustedPr, type CliPr } from '../../src/server/reviewcli.js';
import { reviewUsage } from '../../src/server/usage.js';
import { tempDir } from '../helpers.js';

describe('meko kode review: the command line', () => {
  it('reads the options, in both forms', () => {
    expect(parseReviewArgs([])).toEqual({ json: false, help: false });
    expect(parseReviewArgs(['--base', 'dev', '--depth=3', '--strategy', 'sirius-1.0', '--json'])).toEqual({ base: 'dev', depth: 3, strategy: 'sirius-1.0', json: true, help: false });
    expect(parseReviewArgs(['--pr', '41'])).toMatchObject({ pr: '41' });
    expect(parseReviewArgs(['--description=Adds a login page.'])).toMatchObject({ description: 'Adds a login page.' });
    expect(parseReviewArgs(['--help']).help).toBe(true);
  });

  it('says what is wrong with a command line it refuses', () => {
    const refused = (args: string[]) => {
      try {
        parseReviewArgs(args);
      } catch (e) {
        expect(e).toBeInstanceOf(UsageError);
        return (e as Error).message;
      }
      throw new Error(`accepted ${args.join(' ')}`);
    };
    expect(refused(['--pr', '41', '--description', 'x'])).toBe('Use --pr or --description, not both.');
    expect(refused(['--depth', '6'])).toBe('--depth takes a number from 1 to 5.');
    expect(refused(['--depth', 'two'])).toBe('--depth takes a number from 1 to 5.');
    expect(refused(['--strategy', 'orion'])).toBe('--strategy takes sagittarius-3.0 or sirius-1.0.');
    expect(refused(['--base'])).toBe('--base needs a value. See: meko kode review --help');
    expect(refused(['--fast'])).toBe('Unknown option: --fast. See: meko kode review --help');
    expect(refused(['src/'])).toContain('reviews the folder it runs in');
  });

  it('gives steps no session markers, so a review run inside a session never reports into it', () => {
    const env = stepEnv({
      PATH: '/usr/bin', HOME: '/home/alice', CLAUDECODE: '1', CLAUDE_CODE_SESSION_ID: 'x', KODE_HOOK_SOCK: '/s', KODE_HOOK_TOKEN: 't', AK_HOOK_SOCK: '/s',
      MEKO_AGENT_ID: 'claude_code:a', MEKO_DATAPACK_ID: 'd', MEKO_DATAPACK_IDS: 'd,e', MEKO_CHECKPOINT_INTERVAL: '2147483', CLAUDE_CODE_USE_BEDROCK: '1',
    });
    expect(env).toEqual({ PATH: '/usr/bin', HOME: '/home/alice', CLAUDE_CODE_USE_BEDROCK: '1' });
  });

  it('prints the findings worst first, then the summary line', () => {
    const f = (severity: CodeReviewFinding['severity'], file: string, line: number, comment: string, credits: string[] = []): CodeReviewFinding => ({
      key: '1:1', round: 1, sweep: 1, file, line, severity, comment, passes: ['diff'], gotchas: credits, credits: credits.map((id) => ({ id, quote: 'q' })),
      verdict: 'confirmed', confidence: 90, reason: null, kept: true, dropped: null, outcome: null,
    });
    const text = formatFindings({
      kept: [f('blocker', 'src/a.ts', 42, 'The token is compared with ==.\nUse a constant-time compare.', ['MG-2']), f('minor', 'README.md', 3, 'Typo.')],
      counts: { blocker: 1, major: 0, minor: 1 }, notes: '',
    }, [duration(363_000), dollars(2.144)], false);
    expect(text).toBe([
      'blocker  src/a.ts:42',
      '         The token is compared with ==.',
      '         Use a constant-time compare.',
      '         Caught with Meko (MG-2)',
      '',
      'minor    README.md:3',
      '         Typo.',
      '',
      '2 findings: 1 blocker, 1 minor · 6m 03s · $2.14',
    ].join('\n'));
    expect(formatFindings({ kept: [], counts: noCounts(), notes: 'The migration has no down step.' }, [duration(45_000), dollars(0)], false))
      .toBe('Note: The migration has no down step.\n\nLGTM · 45s');
  });

  it("uses the repo's MEKO.md only for the user's own work or a PR on a repo only people with access can push to", async () => {
    const pr = (url: string): CliPr => ({ number: 41, url, title: 't', body: '', baseRefName: 'main', headRefOid: 'a' });
    const seen = { 'acme/private': 'private', 'acme/inside': 'internal', 'acme/open': 'public' } as Record<string, string>;
    const visibility = async (_cwd: string, repo: string) => {
      if (!seen[repo]) throw new Error('not found');
      return seen[repo];
    };
    expect(await untrustedPr('/r', null, visibility)).toBeNull();
    expect(await untrustedPr('/r', pr('https://github.com/acme/private/pull/41'), visibility)).toBeNull();
    expect(await untrustedPr('/r', pr('https://github.com/acme/inside/pull/41'), visibility)).toBeNull();
    expect(await untrustedPr('/r', pr('https://github.com/acme/open/pull/41'), visibility)).toBe('acme/open is public');
    expect(await untrustedPr('/r', pr('https://github.com/acme/gone/pull/41'), visibility)).toBe("GitHub didn't say whether acme/gone is public");
  });

  it('says where the review is', () => {
    expect(progressText({ phase: 'sweep', sweep: 1, depth: 1, done: 3, total: 5 })).toBe('Passes 3/5');
    expect(progressText({ phase: 'sweep', sweep: 2, depth: 3, done: 0, total: 5 })).toBe('Passes 0/5 · sweep 2 of up to 3');
    expect(progressText({ phase: 'score', done: 4, total: 9 })).toBe('Scoring 4/9');
  });
});

describe('a review step on Claude Code', () => {
  it('runs in print mode with only the review tools, Meko and its run folder', () => {
    const args = reviewStepArgs({ model: 'opus', mcpConfig: '/run/r/mcp.json', runDir: '/run/r', signIn: { env: { ANTHROPIC_BASE_URL: 'https://llm.example.com' } } });
    const val = (flag: string) => args[args.indexOf(flag) + 1];
    expect(args[0]).toBe('-p');
    expect(val('--model')).toBe('opus');
    expect(val('--tools')).toBe('Read,Grep,Glob');
    expect(val('--mcp-config')).toBe('/run/r/mcp.json');
    expect(args).toContain('--strict-mcp-config');
    expect(val('--allowedTools').split(',')).toEqual([
      'mcp__kode_sessions__review_report', 'mcp__kode_sessions__review_score', 'mcp__meko__knowledgebase_search', 'mcp__meko__context_search',
    ]);
    // No settings file, the user's or the repo's: only the sign-in part of the user's, every hook off.
    expect(val('--setting-sources')).toBe('');
    expect(JSON.parse(val('--settings'))).toEqual({ env: { ANTHROPIC_BASE_URL: 'https://llm.example.com' }, disableAllHooks: true });
    expect(val('--permission-mode')).toBe('default');
    expect(val('--add-dir')).toBe('/run/r');
    expect(reviewStepArgs({ model: '', mcpConfig: '/m', runDir: '/r' })).not.toContain('--model');
  });

  it('reads what a step spent and how it ended', () => {
    const ok = { stdout: JSON.stringify({ is_error: false, result: 'Done.', total_cost_usd: 0.42, usage: { input_tokens: 5, output_tokens: 6, cache_read_input_tokens: 7, cache_creation_input_tokens: 8 } }), stderr: '', code: 0, spawnError: null, timedOut: false };
    expect(readStepResult(ok, 'claude')).toEqual({ usage: { input: 5, output: 6, cacheRead: 7, cacheWrite: 8, costUsd: 0.42 } });
    expect(readStepResult({ ...ok, stdout: '', spawnError: 'spawn claude ENOENT' }, 'claude').fatal).toBe("Claude Code couldn't be run (claude): install it, or set [claude] command in config.toml.");
    expect(readStepResult({ ...ok, code: 1, stdout: JSON.stringify({ is_error: true, result: 'Not logged in · Please run /login' }) }, 'claude').fatal).toContain("isn't signed in");
    expect(readStepResult({ ...ok, code: 1, stdout: '', stderr: 'boom\nmore' }, 'claude')).toEqual({ usage: noUsage(), error: 'Claude Code failed: boom' });
    // A step's own text about authentication is a failed step, not a signed-out Claude Code.
    const authText = readStepResult({ ...ok, code: 1, stdout: JSON.stringify({ is_error: true, result: 'The login handler fails to authenticate when /login is retried.' }) }, 'claude');
    expect(authText.fatal).toBeUndefined();
    expect(authText.error).toContain('Claude Code failed: The login handler');
    expect(readStepResult({ ...ok, timedOut: true, code: null }, 'claude').error).toBe('a step ran past 20 minutes');
  });
});

describe('the MCP server with KODE_TOOLS', () => {
  it('lists and takes only the tools named', async () => {
    expect(onlyTools(' review_report, review_score ')).toEqual(['review_report', 'review_score']);
    expect(onlyTools('')).toBeUndefined();
    const link = { sock: '/nowhere.sock', token: 't', only: ['review_report', 'review_score'] };
    const list = await handle(link, { id: 1, method: 'tools/list' }) as { result: { tools: { name: string }[] } };
    expect(list.result.tools.map((t) => t.name)).toEqual(['review_report', 'review_score']);
    const call = await handle(link, { id: 2, method: 'tools/call', params: { name: 'start_sessions', arguments: {} } }) as { result: { isError: boolean; content: { text: string }[] } };
    expect(call.result).toMatchObject({ isError: true, content: [{ text: "start_sessions isn't one of the tools here." }] });
  });
});

const LEDGERS = { dependents: { state_ledger: [], summary: 'LGTM', findings: [] }, claims: { claims: [], summary: 'LGTM', findings: [] } };

function file(path: string, anchors: number[]): FileChange {
  return { path, oldPath: null, status: 'modified', additions: anchors.length, deletions: 0, patch: '@@', withheld: '', anchors, baseRanges: [], deleted: false, symbols: [], patchPath: null };
}

function ctxFor(dir: string): { ctx: ReviewContext; diff: Diff } {
  const diff: Diff = { files: [file('src/a.ts', [1, 2, 3, 4, 5, 6, 7, 8, 9, 10])], patch: '', inline: true, mergeBase: { ref: 'main', sha: 'a'.repeat(40) } };
  const ctx: ReviewContext = {
    repo: 'acme/demo-repo', branch: 'feat', baseRef: 'main', baseSha: 'a'.repeat(40), headSha: 'b'.repeat(40), dirty: false, title: 'feat', prDescription: null,
    description: '', design: null, commits: [], guides: [], history: '', issues: [], datapacks: [], prior: [], worktree: dir, runDir: dir,
  };
  return { ctx, diff };
}

/**
 * A step runner that answers as a model would: each pass's report from `reports` (by pass name and
 * sweep), each scorer's from `score`, through the review's own tool handler; `skip` steps never report.
 */
function scripted(review: () => HeadlessReview, o: {
  reports?: (pass: string, sweep: number) => unknown; score?: (n: number) => unknown; skip?: Set<string>; log?: string[]; running?: { now: number; max: number };
}): StepRunner {
  return async (s: HeadlessStep) => {
    o.log?.push(s.id);
    if (o.running) o.running.max = Math.max(o.running.max, ++o.running.now);
    await new Promise((r) => setTimeout(r, 5));
    if (o.running) o.running.now--;
    if (o.skip?.has(s.id)) return { usage: noUsage(), error: 'it stopped' };
    const text = readFileSync(s.file, 'utf8');
    expect(text).toContain(`"step": "${s.id}"`);
    const m = /^r1s(\d+):(.+)$/.exec(s.id);
    if (m) {
      const pass = m[2];
      const report = o.reports?.(pass, Number(m[1])) ?? (LEDGERS as Record<string, unknown>)[pass] ?? { summary: 'LGTM', findings: [] };
      expect(review().onTool('review_report', { step: s.id, report })).toMatchObject({ isError: false });
    } else {
      const n = Number(s.id.slice('r1c'.length));
      const score = o.score?.(n) ?? { read: ['src/a.ts:1-5'], refutation: '', verdict: 'confirmed', confidence: 90, reason: 'It holds.' };
      expect(review().onTool('review_score', { step: s.id, score })).toMatchObject({ isError: false });
    }
    return { usage: { input: 1, output: 2, cacheRead: 3, cacheWrite: 4, costUsd: 0.5 } };
  };
}

function headless(dir: string, depth: number, runStep: (r: () => HeadlessReview) => StepRunner, strategy = 'sagittarius-3.0') {
  const { ctx, diff } = ctxFor(dir);
  const review: HeadlessReview = new HeadlessReview({
    strategy: strategyFor(strategy), depth, ctx, diff, base: 'BASE', dir, signal: new AbortController().signal, runStep: runStep(() => review),
  });
  return review;
}

describe('the headless review', () => {
  it('sweeps, scores and keeps what survives, as a task review does', async () => {
    const dir = tempDir('kode-headless-');
    const log: string[] = [];
    const finding = (line: number, severity: string, comment: string) => ({ file: 'src/a.ts', line, severity, comment, gotchas: [] });
    const review = headless(dir, 1, (r) => scripted(r, {
      log,
      reports: (pass) => (pass === 'diff' ? { summary: 'LGTM', findings: [finding(2, 'blocker', 'Off by one.'), finding(9, 'minor', 'Nit.')] }
        : pass === 'guide' ? { summary: 'The migration has no down step.', findings: [finding(2, 'major', 'Same line, other pass.'), finding(40, 'major', 'Outside the diff.')] }
          : undefined),
      // Candidates are numbered in the passes' order (guide's, then diff's new ones): the nit, the
      // third, is refuted with what was read; the rest hold.
      score: (n) => (n === 3 ? { read: ['src/a.ts:9'], refutation: 'It is handled at src/a.ts:8.', verdict: 'refuted', confidence: 10, reason: '' } : undefined),
    }));
    const r = await review.run();
    expect(log.slice(0, 5).sort()).toEqual(['r1s1:claims', 'r1s1:dependents', 'r1s1:diff', 'r1s1:guide', 'r1s1:history']);
    expect(log.slice(5).sort()).toEqual(['r1c1', 'r1c2', 'r1c3']);
    expect(r.candidates).toBe(3);
    expect(r.kept.map((f) => [f.file, f.line, f.severity, f.passes])).toEqual([['src/a.ts', 2, 'blocker', ['guide', 'diff']]]);
    expect(r.findings.find((f) => f.line === 9)).toMatchObject({ kept: false, dropped: 'refuted' });
    expect(r.findings.find((f) => f.line === 40)).toMatchObject({ kept: false, dropped: 'line is outside the diff' });
    expect(r.counts).toEqual({ blocker: 1, major: 0, minor: 0 });
    expect(r.notes).toBe('The migration has no down step.');
    expect(r.sweeps).toBe(1);
    expect(r.usage).toEqual({ input: 8, output: 16, cacheRead: 24, cacheWrite: 32, costUsd: 4 });
  });

  it('runs another sweep while one adds something, past the second', async () => {
    const dir = tempDir('kode-headless-');
    const log: string[] = [];
    // Sweep 1 and 2 each add a finding on a new line; sweep 3 adds nothing new, so 4 never runs.
    const review = headless(dir, 5, (r) => scripted(r, {
      log, reports: (pass, sweep) => (pass === 'diff' && sweep <= 2 ? { summary: 'LGTM', findings: [{ file: 'src/a.ts', line: sweep * 5, severity: 'minor', comment: `Sweep ${sweep}.`, gotchas: [] }] } : undefined),
    }));
    const r = await review.run();
    expect(r.sweeps).toBe(3);
    expect(log.filter((s) => s.startsWith('r1s4'))).toEqual([]);
    expect(r.candidates).toBe(2);
    expect(moreSweeps(5, 2, 0)).toBe(false);
    expect(moreSweeps(5, 1, 0)).toBe(true);
    expect(moreSweeps(1, 1, 3)).toBe(false);
  });

  it('asks once more for a step that did not report, then goes on without it', async () => {
    const dir = tempDir('kode-headless-');
    const log: string[] = [];
    const review = headless(dir, 1, (r) => scripted(r, { log, skip: new Set(['r1s1:history']) }));
    const r = await review.run();
    expect(log.filter((s) => s === 'r1s1:history')).toHaveLength(2);
    expect(r.counts).toEqual(noCounts());
  });

  it('fails when no pass reports in the first sweep, saying why', async () => {
    const dir = tempDir('kode-headless-');
    const review = headless(dir, 1, () => async () => ({ usage: noUsage(), error: 'Claude Code failed: overloaded' }), 'sirius-1.0');
    await expect(review.run()).rejects.toThrow(new ReviewFailed('no review pass reported (Claude Code failed: overloaded)'));
    expect(review.soFar()).toEqual({ sweeps: 1, candidates: 0, usage: noUsage() });
  });

  it('stops at once when no step can run', async () => {
    const dir = tempDir('kode-headless-');
    let runs = 0;
    const review = headless(dir, 1, () => async () => {
      runs++;
      return { usage: noUsage(), fatal: "Claude Code isn't signed in." };
    }, 'sirius-1.0');
    await expect(review.run()).rejects.toThrow("Claude Code isn't signed in.");
    expect(runs).toBe(1);
  });

  it("stops the rest of a batch when a step finds Claude Code can't run, counting what they spent", async () => {
    const dir = tempDir('kode-headless-');
    const ended: string[] = [];
    const review = headless(dir, 1, () => async (s, signal) => {
      if (s.id === 'r1s1:diff') return { usage: { ...noUsage(), costUsd: 0.01 }, fatal: "Claude Code isn't signed in." };
      // The others run until stopped, then report what they spent.
      await new Promise<void>((resolve) => (signal.aborted ? resolve() : signal.addEventListener('abort', () => resolve(), { once: true })));
      ended.push(s.id);
      return { usage: { ...noUsage(), costUsd: 1 }, error: 'stopped' };
    });
    await expect(review.run()).rejects.toThrow("Claude Code isn't signed in.");
    expect(ended).toHaveLength(4);
    expect(review.soFar().usage.costUsd).toBeCloseTo(4.01);
  });

  it('scores at most 5 candidates at once', async () => {
    const dir = tempDir('kode-headless-');
    const running = { now: 0, max: 0 };
    const findings = Array.from({ length: 9 }, (_, i) => ({ file: 'src/a.ts', line: i + 1, severity: 'minor', comment: `Finding ${i + 1}.`, gotchas: [] }));
    const review = headless(dir, 1, (r) => scripted(r, { running, reports: (pass) => (pass === 'diff' ? { summary: 'LGTM', findings } : undefined) }));
    const r = await review.run();
    expect(r.candidates).toBe(9);
    expect(running.max).toBe(5);
  });

  it('takes a report only for a step under way, and only when it fits the schema', async () => {
    const dir = tempDir('kode-headless-');
    const review: HeadlessReview = headless(dir, 1, () => async (s) => {
      if (s.id === 'r1s1:diff') {
        expect(review.onTool('review_report', { step: 'r1s2:diff', report: { summary: 'LGTM', findings: [] } }).text).toBe('"r1s2:diff" isn\'t a pass this review is waiting for. Use the step your file names.');
        expect(review.onTool('review_score', { step: 'r1c1', score: {} }).isError).toBe(true);
        expect(review.onTool('review_report', { step: 'r1s1:diff', report: { summary: 'LGTM' } })).toMatchObject({ isError: true, text: expect.stringContaining("doesn't fit the schema") });
        expect(review.onTool('review_report', { step: 'r1s1:diff', report: { summary: 'LGTM', findings: [] } })).toEqual({ isError: false, text: "Kode has the diff pass's report (0 findings)." });
      }
      return { usage: noUsage() };
    });
    await review.run();
    // Once the review is over, nothing is waiting.
    expect(review.onTool('review_report', { step: 'r1s1:diff', report: { summary: 'LGTM', findings: [] } }).isError).toBe(true);
  });
});

function record(over: Partial<CliReviewRecord> = {}): CliReviewRecord {
  return {
    v: 1, id: 'cli-0a1b2c3d4e', repo: 'acme/demo-repo', checkout: '/nowhere', branch: 'feat', base: 'origin/main', strategy: 'sagittarius-3.0', depth: 1,
    state: 'done', detail: null, pr: false, sweeps: 1, candidates: 4, counts: { blocker: 1, major: 0, minor: 2 },
    usage: { input: 100, output: 200, cacheRead: 300, cacheWrite: 400, costUsd: 1.25 }, createdAt: Date.UTC(2026, 10, 10, 12), finishedAt: Date.UTC(2026, 10, 10, 12, 6),
    ...over,
  };
}

describe('the records of meko kode review', () => {
  it('are left owner-only and read back as written', () => {
    const paths = getPaths(join(tempDir('kode-rec-'), 'ak'));
    writeCliRecord(paths, record());
    const dir = cliReviewsDir(paths);
    expect(readdirSync(dir)).toEqual(['cli-0a1b2c3d4e.json']);
    expect(statSync(join(dir, 'cli-0a1b2c3d4e.json')).mode & 0o777).toBe(0o600);
    expect(readCliRecord(readFileSync(join(dir, 'cli-0a1b2c3d4e.json'), 'utf8'))).toEqual(record());
    expect(readCliRecord('{"id":"../etc","state":"done","createdAt":1}')).toBeNull();
    expect(readCliRecord('not json')).toBeNull();
  });

  it('are taken in once, matched to the project on their repo, and counted with the code reviews', () => {
    const home = tempDir('kode-rec-');
    const paths = getPaths(join(home, 'ak'));
    const repo = join(home, 'repo');
    mkdirSync(repo);
    const db = openDb(':memory:');
    run(db, "INSERT INTO projects(id, name, repo_path, default_branch, created_at) VALUES ('p1', 'Demo', ?, 'main', 1)", repo);
    expect(projectForCheckout(db, repo)).toBe('p1');
    expect(projectForCheckout(db, join(home, 'other'))).toBeNull();
    // A closed project still counts its repo's runs, while no open one is on it.
    const old = join(home, 'old');
    mkdirSync(old);
    run(db, "INSERT INTO projects(id, name, repo_path, default_branch, created_at, deleted_at) VALUES ('p0', 'Old', ?, 'main', 1, 5)", old);
    expect(projectForCheckout(db, old)).toBe('p0');

    writeCliRecord(paths, record({ checkout: repo }));
    writeCliRecord(paths, record({ id: 'cli-ffff', checkout: join(home, 'other'), state: 'failed', sweeps: 0, candidates: 0, counts: noCounts() }));
    writeFileSync(join(cliReviewsDir(paths), 'cli-broken.json'), '{');
    const warned: string[] = [];
    expect(ingestCliRecords(db, paths, (m) => warned.push(m))).toBe(2);
    expect(warned).toEqual(["a code review run's record couldn't be read, so it isn't counted: cli-broken.json"]);
    expect(readdirSync(cliReviewsDir(paths))).toEqual([]);
    // A record read again (a crash before its file went) is still one row.
    writeCliRecord(paths, record({ checkout: repo }));
    expect(ingestCliRecords(db, paths, () => undefined)).toBe(1);
    expect(all<{ id: string; project_id: string | null }>(db, 'SELECT id, project_id FROM cli_reviews ORDER BY id')).toEqual([
      { id: 'cli-0a1b2c3d4e', project_id: 'p1' }, { id: 'cli-ffff', project_id: null },
    ]);

    const p1 = reviewUsage(db, 'p1');
    expect(p1).toMatchObject({
      runs: 1, cli: 1, byOutcome: { done: 1, failed: 0, cancelled: 0, running: 0 }, lgtm: 0, prs: 0, rounds: 1, candidates: 4,
      findings: { blocker: 1, major: 0, minor: 2 }, fixed: 0, declined: 0,
    });
    expect(p1.usage).toMatchObject({ input: 100, output: 200, cacheRead: 300, cacheWrite: 400 });
    expect(reviewUsage(db)).toMatchObject({ runs: 2, cli: 2, byOutcome: { done: 1, failed: 1 }, rounds: 1 });
  });

  it('left a month untaken are deleted, as are run folders a killed run left a day ago', () => {
    const paths = getPaths(join(tempDir('kode-rec-'), 'ak'));
    const dir = cliReviewsDir(paths);
    writeCliRecord(paths, record());
    writeCliRecord(paths, record({ id: 'cli-new' }));
    mkdirSync(join(dir, 'cli-killed'));
    writeFileSync(join(dir, 'cli-killed', 'mcp.json'), '{}');
    mkdirSync(join(dir, 'cli-running'));
    const age = (ms: number) => (Date.now() - ms - 60_000) / 1000;
    utimesSync(join(dir, 'cli-0a1b2c3d4e.json'), age(CLI_RECORD_MAX_AGE_MS), age(CLI_RECORD_MAX_AGE_MS));
    utimesSync(join(dir, 'cli-killed'), age(CLI_RUN_DIR_MAX_AGE_MS), age(CLI_RUN_DIR_MAX_AGE_MS));
    sweepOldRecords(paths);
    expect(readdirSync(dir).sort()).toEqual(['cli-new.json', 'cli-running']);
  });
});
