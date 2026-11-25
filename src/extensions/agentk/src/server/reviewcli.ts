/**
 * `meko kode review`: Kode's code review of the change in the folder it runs in, from a terminal,
 * with no task, session or server (docs/design/code-review.md, "From the command line"). It reads
 * the change from where it left its base to the working tree, runs the review headless
 * (`review/headless.ts`: a `claude -p` per step), prints what it kept and exits 0; 1 when it
 * couldn't review. Every run that got as far as reading the change leaves a record the server
 * counts (`review/clirecords.ts`).
 */
import { execFile } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { mkdirSync, realpathSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { basename, dirname, isAbsolute, join, resolve } from 'node:path';
import { promisify } from 'node:util';
import {
  DEFAULT_STRATEGY, MAX_DEPTH, STRATEGIES, countsText, noCounts, strategyName, totalCount, type CodeReviewFinding, type ReviewStrategyId,
} from '../shared/codereview.js';
import { parsePrInput } from '../shared/github-refs.js';
import { REVIEW_MEKO_SERVER, signInSettings } from './claude.js';
import { loadConfig, type Config } from './config.js';
import { git } from './git.js';
import { repoVisibility } from './github.js';
import { SESSION_MARKERS } from './launcher.js';
import { errMsg } from './log.js';
import { probeLocalMeko, type LocalMeko } from './mekolocal.js';
import { mekoDirOf } from './movehome.js';
import { claudePath, type Paths } from './paths.js';
import { cliReviewsDir, sweepOldRecords, writeCliRecord, CLI_RECORD_V, type CliReviewRecord } from './review/clirecords.js';
import { baseRefIn, commitRef, readChange, repoNameOf } from './review/context.js';
import {
  HeadlessReview, ReviewFailed, claudeSteps, noUsage, serveTools, toolsToken, type HeadlessProgress, type HeadlessResult, type StepRunner, type StepUsage,
} from './review/headless.js';
import { basePrompt } from './review/prompt.js';
import { strategyFor } from './review/strategies.js';
import type { Tier } from './review/types.js';
import { TOOLS_SERVER } from './tooldefs.js';
import { toolsCommand } from './tools.js';

const exec = promisify(execFile);

export const REVIEW_HELP = `Usage: meko kode review [options]

Reviews the change in this folder: from where it left its base branch to the working tree,
uncommitted and untracked files included. It runs Kode's code review on Claude Code, prints what
it found and exits 0; 1 when it couldn't review.

  --base <branch>        The base to review against (default: the PR's base with --pr, else the
                         branch origin/HEAD names, else main or master)
  --pr <pr>              The PR this change is for (a number, owner/name#number or a link): its
                         title and description go to the review, which checks the change against them
  --description <text>   What the change is meant to do, for the review to check it against
  --depth <1-5>          Sweeps in a deep review (default: [code_review] depth)
  --strategy <id>        sagittarius-3.0 or sirius-1.0 (default: [code_review] strategy)
  --json                 Print the result as JSON`;

/** What the command line asked for. */
export interface ReviewArgs {
  base?: string;
  pr?: string;
  description?: string;
  depth?: number;
  strategy?: ReviewStrategyId;
  json: boolean;
  help: boolean;
}

/** A mistake on the command line, in words. */
export class UsageError extends Error {}

const VALUED = ['--base', '--pr', '--description', '--depth', '--strategy'] as const;

/** The command line read, or a UsageError saying what's wrong. `--flag value` and `--flag=value` both work. */
export function parseReviewArgs(args: string[]): ReviewArgs {
  const a: ReviewArgs = { json: false, help: false };
  for (let i = 0; i < args.length; i++) {
    const arg = args[i];
    if (arg === '--json') a.json = true;
    else if (arg === '--help' || arg === '-h') a.help = true;
    else if (arg.startsWith('--')) {
      const eq = arg.indexOf('=');
      const name = eq >= 0 ? arg.slice(0, eq) : arg;
      if (!(VALUED as readonly string[]).includes(name)) throw new UsageError(`Unknown option: ${name}. See: meko kode review --help`);
      const value = eq >= 0 ? arg.slice(eq + 1) : args[++i];
      if (value === undefined || value.trim() === '') throw new UsageError(`${name} needs a value. See: meko kode review --help`);
      if (name === '--base') a.base = value.trim();
      else if (name === '--pr') a.pr = value.trim();
      else if (name === '--description') a.description = value;
      else if (name === '--depth') {
        const n = Number(value);
        if (!Number.isInteger(n) || n < 1 || n > MAX_DEPTH) throw new UsageError(`--depth takes a number from 1 to ${MAX_DEPTH}.`);
        a.depth = n;
      } else {
        const s = STRATEGIES.find((x) => x.id === value.trim());
        if (!s) throw new UsageError(`--strategy takes ${STRATEGIES.map((x) => x.id).reverse().join(' or ')}.`);
        a.strategy = s.id;
      }
    } else {
      throw new UsageError(`meko kode review reviews the folder it runs in, and takes no ${arg.startsWith('-') ? `option ${arg}` : `argument (${arg})`}: cd there and run it again.`);
    }
  }
  if (a.pr !== undefined && a.description !== undefined) throw new UsageError('Use --pr or --description, not both.');
  return a;
}

/**
 * A step's environment: the caller's, without any session's markers or any of Kode's and Meko's
 * variables (`KODE_*`, `AK_*`, `MEKO_*`: a session's hook socket, agent id, datapacks, checkpoint
 * interval), so a review run from inside a session never reports into it. A step needs none of them:
 * its tools socket and Meko server are in its own MCP config.
 */
export function stepEnv(env: NodeJS.ProcessEnv): NodeJS.ProcessEnv {
  const out: NodeJS.ProcessEnv = { ...env };
  for (const k of SESSION_MARKERS) delete out[k];
  for (const k of Object.keys(out)) {
    if (k.startsWith('KODE_') || k.startsWith('AK_') || k.startsWith('MEKO_')) delete out[k];
  }
  return out;
}

/** A PR as `--pr` reads it. */
export interface CliPr {
  number: number;
  url: string;
  title: string;
  body: string;
  baseRefName: string;
  headRefOid: string;
}

/** Reads `--pr` with gh, in the reviewed folder (so a bare number is that repo's PR). */
async function readPr(folder: string, input: string, env: NodeJS.ProcessEnv): Promise<CliPr> {
  const ref = parsePrInput(input);
  if (!ref) throw new UsageError('--pr takes a PR number, owner/name#number or a PR link.');
  try {
    const { stdout } = await exec('gh', ['pr', 'view', String(ref.number), ...(ref.repo ? ['--repo', ref.repo] : []), '--json', 'number,url,title,body,baseRefName,headRefOid'],
      { cwd: folder, env: { ...env, GH_PROMPT_DISABLED: '1', NO_COLOR: '1' }, maxBuffer: 8 * 1024 * 1024, timeout: 30_000 });
    return JSON.parse(stdout) as CliPr;
  } catch (e) {
    if ((e as NodeJS.ErrnoException).code === 'ENOENT') {
      throw new UsageError("gh isn't installed: --pr reads the PR with GitHub's command-line tool (https://cli.github.com). Install it, or say what the change does with --description.");
    }
    const said = String((e as { stderr?: unknown }).stderr ?? errMsg(e)).trim().split('\n')[0];
    throw new UsageError(`Couldn't read PR ${input} with gh${said ? `: ${said}` : ''}`);
  }
}

/**
 * The base the change is read against, as a ref that names a commit here: `--base` (origin's copy
 * of a branch first, as a task's review reads it), else the PR's base, else the branch origin/HEAD
 * names, else main or master.
 */
export async function pickBase(folder: string, a: { base?: string; prBase?: string; pr?: string }): Promise<string> {
  if (a.base) {
    const ref = (await commitRef(folder, `origin/${a.base}`)) ?? (await commitRef(folder, a.base));
    if (!ref) throw new UsageError(`There's no branch or commit ${a.base} here. Fetch it, or name another with --base.`);
    return ref;
  }
  if (a.prBase) {
    const ref = await baseRefIn(folder, a.prBase);
    if (!(await commitRef(folder, ref))) throw new UsageError(`PR ${a.pr}'s base, ${a.prBase}, isn't here. Fetch it (git fetch origin ${a.prBase}), or name the base with --base.`);
    return ref;
  }
  const head = await git(folder, 'symbolic-ref', '--quiet', '--short', 'refs/remotes/origin/HEAD').catch(() => '');
  if (head && (await commitRef(folder, head))) return head;
  for (const b of ['main', 'master']) {
    const ref = await baseRefIn(folder, b);
    if (await commitRef(folder, ref)) return ref;
  }
  throw new UsageError("Kode couldn't tell this repo's base branch. Name it with --base.");
}

/**
 * Why the repo's own MEKO.md files can't be used, or null when they can. Without `--pr` the folder is
 * the user's own work, as a task on its own branch is. A PR on a public repo (or one whose
 * visibility gh can't say) is anyone's: its MEKO.md could point the passes, with the user's Meko
 * token, at datapacks of the PR author's choosing, so the review runs without them, as Kode runs a
 * session on such a PR's head without the repo's config (`headtrust.ts`).
 */
export async function untrustedPr(folder: string, pr: CliPr | null, visibility: (cwd: string, repo: string) => Promise<string> = repoVisibility): Promise<string | null> {
  if (!pr) return null;
  const repo = parsePrInput(pr.url)?.repo;
  if (!repo) return `Kode doesn't know which repo PR #${pr.number} is on`;
  const v = await visibility(folder, repo).catch(() => null);
  if (v === 'private' || v === 'internal') return null;
  return v === 'public' ? `${repo} is public` : `GitHub didn't say whether ${repo} is public`;
}

/** The repo's main checkout, its real path (a worktree's too): what the server matches to a project. */
async function mainCheckout(folder: string): Promise<string> {
  const common = await git(folder, 'rev-parse', '--path-format=absolute', '--git-common-dir').catch(() => '');
  const dir = common ? (basename(common) === '.git' ? dirname(common) : common) : folder;
  try {
    return realpathSync(isAbsolute(dir) ? dir : resolve(folder, dir));
  } catch {
    return dir;
  }
}

/** "6m 03s", "45s". */
export function duration(ms: number): string {
  const s = Math.max(0, Math.round(ms / 1000));
  return s < 60 ? `${s}s` : `${Math.floor(s / 60)}m ${String(s % 60).padStart(2, '0')}s`;
}

/** "$2.14"; '' for nothing reported. */
export const dollars = (usd: number) => (usd > 0 ? `$${usd < 0.01 ? usd.toFixed(3) : usd.toFixed(2)}` : '');

/** How the command writes: stdout for the result, stderr for progress and errors. */
export interface ReviewIo {
  cwd: string;
  env: NodeJS.ProcessEnv;
  out: (s: string) => void;
  err: (s: string) => void;
  /** stderr is a terminal: progress redraws one line. */
  errTTY: boolean;
  /** Colors on stdout. */
  color: boolean;
  now: () => number;
  /** Stops the review (Ctrl+C); called once with the function that does. Returns how to stop listening. */
  onInterrupt: (stop: () => void) => () => void;
  /** The Meko on this computer, or null. */
  meko: (paths: Paths, cfg: Config) => Promise<LocalMeko | null>;
  /** Claude Code's version, or null when it can't be run. */
  claudeVersion: (cmd: string) => Promise<string | null>;
  /** Who can see a GitHub repo (`repoVisibility`), for `--pr`. */
  visibility?: (cwd: string, repo: string) => Promise<string>;
  /** The step runner (tests swap it). */
  steps?: (o: { command: string; models: Record<Tier, string>; mcpConfig: string; runDir: string; cwd: string; env: NodeJS.ProcessEnv; signIn?: Record<string, unknown> }) => StepRunner;
}

export function processIo(): ReviewIo {
  const color = !!process.stdout.isTTY && !process.env.NO_COLOR && process.env.TERM !== 'dumb';
  return {
    cwd: process.cwd(),
    env: process.env,
    out: (s) => process.stdout.write(`${s}\n`),
    err: (s) => process.stderr.write(s),
    errTTY: !!process.stderr.isTTY && process.env.TERM !== 'dumb',
    color,
    now: Date.now,
    onInterrupt: (stop) => {
      const on = () => stop();
      process.on('SIGINT', on);
      process.on('SIGTERM', on);
      return () => {
        process.off('SIGINT', on);
        process.off('SIGTERM', on);
      };
    },
    meko: async (paths, cfg) => (await probeLocalMeko(mekoDirOf(paths.home) ?? (cfg.meko.home || null))).meko,
    claudeVersion: async (cmd) => {
      try {
        const { stdout } = await exec(cmd, ['--version'], { timeout: 30_000, env: { ...stepEnv(process.env), NO_COLOR: '1' } });
        return stdout.trim() || 'unknown';
      } catch (e) {
        return (e as NodeJS.ErrnoException).code === 'ENOENT' ? null : 'unknown';
      }
    },
  };
}

const paint = (on: boolean, code: string, s: string) => (on ? `\x1b[${code}m${s}\x1b[0m` : s);

/** The findings as text: each with its severity, place and comment, then the summary line. */
export function formatFindings(r: Pick<HeadlessResult, 'kept' | 'counts' | 'notes'>, extra: string[], color: boolean): string {
  const lines: string[] = [];
  const sev = { blocker: '31', major: '33', minor: '2' } as const;
  for (const f of r.kept) {
    lines.push(`${paint(color, sev[f.severity], f.severity.padEnd(8))} ${paint(color, '1', `${f.file}:${f.line}`)}`);
    for (const l of f.comment.trim().split('\n')) lines.push(l.trim() ? `         ${l}` : '');
    if (f.credits.length) lines.push(`         ${paint(color, '2', `Caught with Meko (${f.credits.map((c) => c.id).join(', ')})`)}`);
    lines.push('');
  }
  if (r.notes) lines.push(...r.notes.split('\n').map((l) => `Note: ${l.replace(/^- /, '')}`), '');
  const n = totalCount(r.counts);
  const head = n ? `${n} finding${n === 1 ? '' : 's'}: ${countsText(r.counts)}` : 'LGTM';
  lines.push([head, ...extra].filter(Boolean).join(paint(color, '2', ' · ')));
  return lines.join('\n');
}

/** A finding as `--json` gives it. */
const jsonFinding = (f: CodeReviewFinding) => ({
  key: f.key, file: f.file, line: f.line, severity: f.severity, comment: f.comment, confidence: f.confidence, passes: f.passes, credits: f.credits,
});

/** The progress line's text. */
export function progressText(p: HeadlessProgress): string {
  return p.phase === 'sweep'
    ? `Passes ${p.done}/${p.total}${p.depth > 1 ? ` · sweep ${p.sweep} of up to ${p.depth}` : ''}`
    : `Scoring ${p.done}/${p.total}`;
}

/**
 * Runs `meko kode review` with `args`; returns the exit code: 0 when the review ran (findings or
 * none), 1 when it couldn't.
 */
export async function runReview(paths: Paths, args: string[], io: ReviewIo = processIo()): Promise<number> {
  const fail = (msg: string) => {
    io.err(`${msg}\n`);
    return 1;
  };
  let a: ReviewArgs;
  try {
    a = parseReviewArgs(args);
  } catch (e) {
    return fail(errMsg(e));
  }
  if (a.help) {
    io.out(REVIEW_HELP);
    return 0;
  }
  const cfg = loadConfig(paths, (m) => io.err(`${m}\n`));
  const strategy = strategyFor(a.strategy ?? (cfg.codeReview.strategy === 'off' ? DEFAULT_STRATEGY : cfg.codeReview.strategy));
  const depth = a.depth ?? cfg.codeReview.depth;

  // Before anything is recorded: what to review, and what to review it with.
  let folder: string, base: string, pr: CliPr | null = null;
  try {
    folder = await git(io.cwd, 'rev-parse', '--show-toplevel').catch(() => {
      throw new UsageError("This folder isn't in a git repo. Run meko kode review in the repo whose change you want reviewed.");
    });
    if (!(await io.claudeVersion(cfg.claude.command))) {
      throw new UsageError(`meko kode review runs on Claude Code, which isn't installed here (${cfg.claude.command}). Install it, or set [claude] command in config.toml.`);
    }
    if (a.pr) pr = await readPr(folder, a.pr, io.env);
    base = await pickBase(folder, { base: a.base, prBase: pr?.baseRefName, pr: a.pr });
  } catch (e) {
    return fail(errMsg(e));
  }

  const started = io.now();
  const id = `cli-${randomBytes(5).toString('hex')}`;
  const dir = join(cliReviewsDir(paths), id);
  const head = await git(folder, 'rev-parse', '--abbrev-ref', 'HEAD').catch(() => 'HEAD');
  const sha = await git(folder, 'rev-parse', 'HEAD').catch(() => '');
  const branch = head === 'HEAD' ? `detached at ${sha.slice(0, 12)}` : head;
  const rec: CliReviewRecord = {
    v: CLI_RECORD_V, id, repo: await repoNameOf(folder), checkout: await mainCheckout(folder), branch, base, strategy: strategy.id, depth,
    state: 'failed', detail: null, pr: !!pr, sweeps: 0, candidates: 0, counts: noCounts(), usage: noUsage(), createdAt: started, finishedAt: started,
  };
  // `got`: how far a run that stopped got (`HeadlessReview.soFar`); a finished one has its result.
  const finish = (state: CliReviewRecord['state'], o: { detail?: string | null; result?: HeadlessResult; got?: { sweeps: number; candidates: number; usage: StepUsage } } = {}) => {
    const r = o.result ?? o.got;
    Object.assign(rec, {
      state, detail: o.detail ?? null, finishedAt: io.now(), sweeps: r?.sweeps ?? 0, candidates: r?.candidates ?? 0,
      counts: o.result?.counts ?? noCounts(), usage: r?.usage ?? noUsage(),
    });
    try {
      writeCliRecord(paths, rec);
    } catch (e) {
      io.err(`Kode couldn't record this run for its usage counts: ${errMsg(e)}\n`);
    }
  };

  const controller = new AbortController();
  const stopListening = io.onInterrupt(() => controller.abort());
  let closeTools: (() => Promise<void>) | null = null;
  let review: HeadlessReview | null = null;
  let progressShown = false;
  const clearProgress = () => {
    if (progressShown && io.errTTY) io.err('\r\x1b[K');
    progressShown = false;
  };
  try {
    sweepOldRecords(paths);
    mkdirSync(dir, { recursive: true, mode: 0o700 });
    const untrusted = await untrustedPr(folder, pr, io.visibility);
    if (untrusted && pr) io.err(`${untrusted}, so anyone can have written PR #${pr.number}: the review runs without the repo's MEKO.md datapacks.\n`);
    const meko = untrusted ? null : await io.meko(paths, cfg).catch(() => null);
    const prDescription = pr ? `${pr.title}\n\n${pr.body ?? ''}`.trim() : a.description?.trim() || null;
    const { ctx, diff } = await readChange({
      folder, repo: folder, base, dir, branch, title: pr?.title || branch, prDescription, description: '', design: null,
      meko: !!meko, issues: async () => [], prior: [],
    });
    if (!diff.files.length) {
      finish('done', { detail: 'nothing changed against the base' });
      if (a.json) {
        io.out(JSON.stringify({
          repo: rec.repo, branch, base, head: sha, strategy: strategy.id, depth, files: 0, counts: noCounts(), findings: [], notes: '',
          seconds: Math.round((io.now() - started) / 1000), costUsd: 0,
        }));
      }
      else io.out(`Nothing changed against ${base}.`);
      return 0;
    }
    const adds = diff.files.reduce((n, f) => n + f.additions, 0);
    const dels = diff.files.reduce((n, f) => n + f.deletions, 0);
    io.err(`Reviewing ${branch} against ${base}: ${diff.files.length} file${diff.files.length === 1 ? '' : 's'}, +${adds} −${dels} (${strategyName(strategy.id)}${depth > 1 ? `, depth ${depth}` : ''})\n`);
    if (pr && sha && pr.headRefOid !== sha) {
      const ahead = await git(folder, 'merge-base', '--is-ancestor', pr.headRefOid, 'HEAD').then(() => true, () => false);
      io.err(ahead
        ? `This folder has commits PR #${pr.number} doesn't have; they're reviewed too.\n`
        : `This folder isn't at PR #${pr.number}'s head commit; the review reads the folder as it is.\n`);
    }

    const prompt = basePrompt(ctx, diff);
    writeFileSync(join(dir, 'base.md'), prompt);
    // The steps report on a socket of this run's own, with a token only they get.
    const token = toolsToken();
    const uid = typeof process.getuid === 'function' ? process.getuid() : 0;
    const sock = join(tmpdir(), `kode-review-${uid}`, `${id}.sock`);
    const mcpConfig = join(dir, 'mcp.json');
    const { command, args: cmdArgs } = toolsCommand(paths);
    const servers: Record<string, unknown> = {
      [TOOLS_SERVER]: { type: 'stdio', command, args: cmdArgs, env: { KODE_HOOK_SOCK: claudePath(sock), KODE_HOOK_TOKEN: token, KODE_TOOLS: 'review_report,review_score' } },
    };
    if (meko && ctx.datapacks.length) servers[REVIEW_MEKO_SERVER] = { type: 'http', url: meko.url, headers: { Authorization: `Bearer ${meko.token}` } };
    writeFileSync(mcpConfig, `${JSON.stringify({ mcpServers: servers }, null, 2)}\n`, { mode: 0o600 });
    const make = io.steps ?? claudeSteps;
    const runStep = make({ command: cfg.claude.command, models: cfg.codeReview.models.claude, mcpConfig, runDir: dir, cwd: folder, env: stepEnv(io.env), signIn: signInSettings() });
    const r = new HeadlessReview({
      strategy, depth, ctx, diff, base: prompt, dir, signal: controller.signal, runStep,
      progress: (p) => {
        const text = progressText(p);
        if (io.errTTY) io.err(`\r  ${text}\x1b[K`);
        else if (p.done === 0) io.err(`  ${text}\n`);
        progressShown = true;
      },
    });
    review = r;
    closeTools = await serveTools(sock, token, (tool, input) => r.onTool(tool, input));

    const result = await r.run();
    clearProgress();
    finish('done', { result });
    const seconds = Math.round((io.now() - started) / 1000);
    if (a.json) {
      io.out(JSON.stringify({
        repo: rec.repo, branch, base, head: sha, strategy: strategy.id, depth, files: diff.files.length, counts: result.counts,
        findings: result.kept.map(jsonFinding), notes: result.notes, seconds, costUsd: Math.round(result.usage.costUsd * 100) / 100,
      }));
    } else {
      io.out(formatFindings(result, [duration(io.now() - started), dollars(result.usage.costUsd)], io.color));
    }
    return 0;
  } catch (e) {
    clearProgress();
    const stopped = controller.signal.aborted;
    const got = review?.soFar();
    if (stopped) {
      finish('cancelled', { detail: 'stopped', got });
      return fail('Stopped the review.');
    }
    const why = e instanceof ReviewFailed || e instanceof UsageError ? e.message : errMsg(e);
    finish('failed', { detail: why, got });
    return fail(`The review couldn't finish: ${why}`);
  } finally {
    stopListening();
    // Whatever still runs (a step of a review that failed) stops with it.
    controller.abort();
    await closeTools?.().catch(() => undefined);
    rmSync(dir, { recursive: true, force: true });
  }
}
