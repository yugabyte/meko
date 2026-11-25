/**
 * `meko kode review`'s driver (docs/design/code-review.md, "From the command line"): Kode's code
 * review run headless, in the CLI's own process, with no task, session or server. Each pass and
 * each scorer is a step of its own, a fresh `claude -p` on its tier's model (`StepRunner`), which
 * reports through the review's tools (`review_report`, `review_score`) over a private socket this
 * process answers (`serveTools`), checked against the same schemas as a task's review. The
 * algorithm is the one `runner.ts` drives (`merge.ts`, `prompt.ts`): sweeps up to the depth,
 * ask-again once for a step that didn't report, scorers in batches, then `judgeRound`. It reports
 * only: one round, no fixes, no PR.
 */
import { spawn } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { rmSync, writeFileSync } from 'node:fs';
import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import { join } from 'node:path';
import { SEVERITIES, noCounts, type CodeReviewFinding, type SeverityCounts } from '../../shared/codereview.js';
import { listenPrivate } from '../hooks.js';
import { claudePath } from '../paths.js';
import { TOOLS_SERVER } from '../tooldefs.js';
import { reviewStepArgs } from '../claude.js';
import { ASK_AGAIN, SCORE_BATCH, SCORER_TIER, countsOf, judgeRound, mergeNotes, mergeSweep, moreSweeps } from './merge.js';
import { passPrompt, scoreBase, scorePrompt, sweepContext } from './prompt.js';
import { checkReport, checkScore } from './schemas.js';
import type { Candidate, Diff, Pass, PassReport, ReviewContext, ScoreReport, Strategy, Tier } from './types.js';

/** The report tools as a step's Claude Code calls them. */
export const REPORT_TOOL = `mcp__${TOOLS_SERVER}__review_report`;
export const SCORE_TOOL = `mcp__${TOOLS_SERVER}__review_score`;

/** How long one step may run before it's stopped and counted as one that didn't report. */
export const STEP_TIMEOUT_MS = 20 * 60 * 1000;

/** The most of a step's output kept (its JSON result is a few kilobytes). */
const OUTPUT_MAX = 1024 * 1024;

/** One step: a pass of a sweep (`r1s<sweep>:<pass>`) or a scorer (`r1c<n>`), the tier it runs on and the prompt file it reads. */
export interface HeadlessStep {
  id: string;
  tier: Tier;
  file: string;
}

/** What steps spent, as Claude Code's result reports it. */
export interface StepUsage {
  input: number;
  output: number;
  cacheRead: number;
  cacheWrite: number;
  costUsd: number;
}

export const noUsage = (): StepUsage => ({ input: 0, output: 0, cacheRead: 0, cacheWrite: 0, costUsd: 0 });

function addStepUsage(a: StepUsage, b: StepUsage): StepUsage {
  return { input: a.input + b.input, output: a.output + b.output, cacheRead: a.cacheRead + b.cacheRead, cacheWrite: a.cacheWrite + b.cacheWrite, costUsd: a.costUsd + b.costUsd };
}

/**
 * How one step's run ended: what it spent, and why it failed when it did (`error`: the step goes
 * unreported; `fatal`: no step can run, so the review stops, as for Claude Code missing or signed out).
 */
export interface StepOutcome {
  usage: StepUsage;
  error?: string;
  fatal?: string;
}

/** Runs one step to its end; aborted with `signal`. Never throws: a failure is the outcome's. */
export type StepRunner = (step: HeadlessStep, signal: AbortSignal) => Promise<StepOutcome>;

/** Where a review is, for the progress line. */
export type HeadlessProgress =
  | { phase: 'sweep'; sweep: number; depth: number; done: number; total: number }
  | { phase: 'score'; done: number; total: number };

/** A review that couldn't finish, with why in words. */
export class ReviewFailed extends Error {}

export interface HeadlessResult {
  /** Every candidate as a finding, kept or dropped. */
  findings: CodeReviewFinding[];
  /** The kept ones, worst first, then by file and line. */
  kept: CodeReviewFinding[];
  counts: SeverityCounts;
  /** Candidates the passes raised, before scoring and Validate. */
  candidates: number;
  /** Sweeps that ran. */
  sweeps: number;
  /** The passes' summaries worth showing (`mergeNotes`), or ''. */
  notes: string;
  usage: StepUsage;
}

const rank = (f: CodeReviewFinding) => SEVERITIES.indexOf(f.severity);

/**
 * One review of one change: `run()` once. The steps' reports reach it through `onTool` (the tools
 * socket), which takes a report only for a step of the sweep or scoring under way.
 */
export class HeadlessReview {
  /** The steps of the sweep or scoring under way, by id: a pass's, or a candidate's number. */
  private open = new Map<string, { pass: Pass } | { candidate: number }>();
  private reports = new Map<string, PassReport | ScoreReport>();
  private usage = noUsage();
  /** The last step's error, for a review no pass reported in. */
  private lastError = '';
  /** How far it got: the sweeps whose passes ran and the candidates they raised (for a run that stops). */
  private sweeps = 0;
  private candidates = 0;

  constructor(private o: {
    strategy: Strategy;
    depth: number;
    ctx: ReviewContext;
    diff: Diff;
    /** The passes' shared base prompt (`basePrompt`), as written to the run folder. */
    base: string;
    /** The run folder: each step's prompt file goes there. */
    dir: string;
    runStep: StepRunner;
    signal: AbortSignal;
    progress?: (p: HeadlessProgress) => void;
  }) {}

  /** How far the review got, and what its steps spent: all of it once `run` is done, else so far. */
  soFar(): { sweeps: number; candidates: number; usage: StepUsage } {
    return { sweeps: this.sweeps, candidates: this.candidates, usage: this.usage };
  }

  /** A report tool's call from a step: checked against its schema and kept. The text goes back to the step. */
  onTool(tool: string, input: unknown): { text: string; isError: boolean } {
    const i = input && typeof input === 'object' ? (input as Record<string, unknown>) : {};
    const step = typeof i.step === 'string' ? i.step.trim() : '';
    const want = this.open.get(step);
    if (tool === 'review_report') {
      if (!want || !('pass' in want)) return { text: `"${step}" isn't a pass this review is waiting for. Use the step your file names.`, isError: true };
      const checked = checkReport(want.pass, i.report);
      if ('error' in checked) return { text: `The report doesn't fit the schema: ${checked.error}. Fix it and call ${tool} again.`, isError: true };
      const m = /^r1s(\d+):/.exec(step);
      this.reports.set(step, { pass: want.pass.name, round: 1, sweep: Number(m?.[1] ?? 1), summary: checked.summary, findings: checked.findings });
      return { text: `Kode has the ${want.pass.name} pass's report (${checked.findings.length} finding${checked.findings.length === 1 ? '' : 's'}).`, isError: false };
    }
    if (tool === 'review_score') {
      if (!want || !('candidate' in want)) return { text: `"${step}" isn't a finding this review is scoring. Use the step your file names.`, isError: true };
      const checked = checkScore(i.score);
      if ('error' in checked) return { text: `The score doesn't fit the schema: ${checked.error}. Fix it and call ${tool} again.`, isError: true };
      this.reports.set(step, checked);
      return { text: `Kode has the score for finding ${want.candidate}.`, isError: false };
    }
    return { text: `${tool} isn't one of this review's tools.`, isError: true };
  }

  async run(): Promise<HeadlessResult> {
    const { strategy, depth, dir } = this.o;
    let cands: Candidate[] = [];
    const notes: string[] = [];
    let sweeps = 0;
    for (let sweep = 1; sweep <= depth; sweep++) {
      const ctx = sweepContext(sweep, depth, cands);
      const steps = strategy.passes.map((pass) => {
        const id = `r1s${sweep}:${pass.name}`;
        const file = join(dir, `s${sweep}-${pass.name}.md`);
        writeFileSync(file, passPrompt(this.o.base, pass, ctx, { tool: REPORT_TOOL, step: id }));
        this.open.set(id, { pass });
        return { id, tier: pass.tier, file };
      });
      this.sweeps = sweep;
      await this.runSteps(steps, steps.length, (done) => this.o.progress?.({ phase: 'sweep', sweep, depth, done, total: steps.length }));
      this.open.clear();
      sweeps = sweep;
      const reports = steps.map((s) => this.reports.get(s.id)).filter((r): r is PassReport => !!r);
      if (!reports.length) {
        // Nothing from this sweep: a later one ends the sweeping; the first fails the review.
        if (sweep === 1) throw new ReviewFailed(`no review pass reported${this.lastError ? ` (${this.lastError})` : ''}`);
        break;
      }
      const merged = mergeSweep(cands, reports, sweep);
      cands = merged.candidates;
      this.candidates = cands.length;
      const note = mergeNotes(reports).trim();
      if (note && note !== 'LGTM') notes.push(note);
      if (!moreSweeps(depth, sweep, merged.added)) break;
    }
    const scores = new Map<number, ScoreReport>();
    if (strategy.scored && cands.length) {
      const base = scoreBase(this.o.ctx, this.o.diff);
      const steps = cands.map((c, i) => {
        const n = i + 1;
        const id = `r1c${n}`;
        const file = join(dir, `score-${n}.md`);
        writeFileSync(file, scorePrompt(base, c, { tool: SCORE_TOOL, step: id }));
        this.open.set(id, { candidate: n });
        return { id, tier: SCORER_TIER, file };
      });
      await this.runSteps(steps, SCORE_BATCH, (done) => this.o.progress?.({ phase: 'score', done, total: steps.length }));
      this.open.clear();
      steps.forEach((s, i) => {
        const r = this.reports.get(s.id);
        if (r) scores.set(i + 1, r as ScoreReport);
      });
    }
    const { findings, post } = judgeRound({ round: 1, cands, scores, diff: this.o.diff, scored: strategy.scored });
    const kept = findings.filter((f) => f.kept).sort((a, b) => rank(a) - rank(b) || a.file.localeCompare(b.file) || a.line - b.line);
    return { findings, kept, counts: post.length ? countsOf(post) : noCounts(), candidates: cands.length, sweeps, notes: notes.join('\n'), usage: this.usage };
  }

  /**
   * Runs the steps, `batch` at a time, each batch once the one before has finished; then the ones
   * that didn't report once more (ASK_AGAIN), as a task's review asks again. A step that finds no
   * step can run (`fatal`) stops the rest of its batch; every step of a batch has ended, its spend
   * counted, before a batch's failure (or a stop) is thrown, so `soFar()` then holds all of it.
   */
  private async runSteps(steps: HeadlessStep[], batch: number, progress: (done: number) => void): Promise<void> {
    const reported = () => steps.filter((s) => this.reports.has(s.id)).length;
    progress(0);
    let todo = steps;
    for (let ask = 0; ask <= ASK_AGAIN && todo.length; ask++) {
      for (let i = 0; i < todo.length; i += batch) {
        const stop = new AbortController();
        const onAbort = () => stop.abort();
        this.o.signal.addEventListener('abort', onAbort, { once: true });
        try {
          const ended = await Promise.allSettled(todo.slice(i, i + batch).map(async (s) => {
            await this.runStep(s, stop);
            progress(reported());
          }));
          if (this.o.signal.aborted) throw abortError();
          const failed = ended.find((r): r is PromiseRejectedResult => r.status === 'rejected');
          if (failed) throw failed.reason;
        } finally {
          this.o.signal.removeEventListener('abort', onAbort);
        }
      }
      todo = steps.filter((s) => !this.reports.has(s.id));
    }
  }

  /** One step, stopped with `stop` (the review's stop, or a fatal step of its batch, which stops the rest). */
  private async runStep(s: HeadlessStep, stop: AbortController): Promise<void> {
    if (stop.signal.aborted || this.o.signal.aborted) throw abortError();
    const out = await this.o.runStep(s, stop.signal);
    this.usage = addStepUsage(this.usage, out.usage);
    if (this.o.signal.aborted) throw abortError();
    if (out.fatal) {
      stop.abort();
      throw new ReviewFailed(out.fatal);
    }
    if (out.error) this.lastError = out.error;
  }
}

function abortError(): Error {
  const e = new Error('The review was stopped.');
  e.name = 'AbortError';
  return e;
}

/** How Claude Code's sign-in errors start ("Not logged in · Please run /login", "Invalid API key · …"). */
const SIGNED_OUT = /^\s*(not logged in|invalid api key|please run \/login|oauth token (has )?expired)\b/i;

/** What a step's Claude Code is told: the step's file to read, in the spelling it runs in. */
export const stepPrompt = (file: string) => `Read ${claudePath(file)} and do what it says.`;

/** Claude Code's `-p --output-format json` result, read: what it spent and whether it failed. */
export function readStepResult(r: { stdout: string; stderr: string; code: number | null; spawnError: string | null; timedOut: boolean }, cmd: string): StepOutcome {
  if (r.spawnError) return { usage: noUsage(), fatal: `Claude Code couldn't be run (${cmd}): install it, or set [claude] command in config.toml.` };
  let out: Record<string, unknown> | null = null;
  try {
    const v = JSON.parse(r.stdout) as unknown;
    if (v && typeof v === 'object' && !Array.isArray(v)) out = v as Record<string, unknown>;
  } catch {
    /* not JSON: said below */
  }
  const u = (out?.usage && typeof out.usage === 'object' ? out.usage : {}) as Record<string, unknown>;
  const num = (v: unknown) => (typeof v === 'number' && Number.isFinite(v) && v > 0 ? v : 0);
  const usage: StepUsage = {
    input: num(u.input_tokens), output: num(u.output_tokens), cacheRead: num(u.cache_read_input_tokens), cacheWrite: num(u.cache_creation_input_tokens),
    costUsd: num(out?.total_cost_usd),
  };
  if (r.timedOut) return { usage, error: `a step ran past ${STEP_TIMEOUT_MS / 60_000} minutes` };
  const said = (typeof out?.result === 'string' ? out.result : '') || r.stderr.trim().split('\n')[0] || '';
  if (out?.is_error === true || r.code !== 0) {
    // Only Claude Code's own sign-in errors, which start its message: a step's own text may well be
    // about authentication (a finding on auth code), and must not stop the review.
    if (SIGNED_OUT.test(said)) {
      return { usage, fatal: "Claude Code isn't signed in. Run claude in a terminal and log in, then run meko kode review again." };
    }
    return { usage, error: `Claude Code failed${said ? `: ${said.replace(/\s+/g, ' ').slice(0, 300)}` : ` (exit ${r.code ?? 'unknown'})`}` };
  }
  return { usage };
}

/**
 * Runs each step as one `claude -p` (`reviewStepArgs`) in `cwd`, its prompt on stdin, on its tier's
 * model, with `env` (the caller's, without any session's markers). Stopped by the signal or past
 * STEP_TIMEOUT_MS: SIGTERM, then SIGKILL 5 seconds on.
 */
export function claudeSteps(o: {
  command: string; models: Record<Tier, string>; mcpConfig: string; runDir: string; cwd: string; env: NodeJS.ProcessEnv; timeoutMs?: number;
  /** The sign-in part of the user's Claude Code settings (`signInSettings`). */
  signIn?: Record<string, unknown>;
}): StepRunner {
  const timeoutMs = o.timeoutMs ?? STEP_TIMEOUT_MS;
  return (step, signal) => new Promise((resolve) => {
    const args = reviewStepArgs({ model: o.models[step.tier] ?? '', mcpConfig: o.mcpConfig, runDir: o.runDir, signIn: o.signIn });
    let stdout = '', stderr = '', spawnError: string | null = null, timedOut = false, settled = false;
    // In the spelling Claude is given every other path in (the prompts, --add-dir, the MCP config),
    // as a session's launcher cds there: under /net, Read would otherwise be outside the working folder.
    const child = spawn(o.command, args, { cwd: claudePath(o.cwd), env: { ...o.env, NO_COLOR: '1' }, stdio: ['pipe', 'pipe', 'pipe'] });
    const kill = () => {
      if (child.exitCode !== null || child.signalCode !== null) return;
      child.kill('SIGTERM');
      setTimeout(() => {
        if (child.exitCode === null && child.signalCode === null) child.kill('SIGKILL');
      }, 5000).unref();
    };
    const timer = setTimeout(() => {
      timedOut = true;
      kill();
    }, timeoutMs);
    const onAbort = () => kill();
    signal.addEventListener('abort', onAbort, { once: true });
    child.stdout.setEncoding('utf8');
    child.stderr.setEncoding('utf8');
    child.stdout.on('data', (c: string) => {
      if (stdout.length < OUTPUT_MAX) stdout += c;
    });
    child.stderr.on('data', (c: string) => {
      if (stderr.length < OUTPUT_MAX) stderr += c;
    });
    const done = (code: number | null) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      signal.removeEventListener('abort', onAbort);
      resolve(readStepResult({ stdout, stderr, code, spawnError, timedOut }, o.command));
    };
    child.on('error', (e: NodeJS.ErrnoException) => {
      if (e.syscall?.startsWith('spawn')) spawnError = e.message;
      done(null);
    });
    child.on('close', (code) => done(code));
    // A Claude that exits before reading it closes the pipe; that's for the exit status to say.
    child.stdin.on('error', () => undefined);
    child.stdin.end(stepPrompt(step.file));
  });
}

/**
 * The review tools' socket: `POST /tools` with the token, as `me_kode mcp` sends a call
 * (`callServer`), answered by `onTool`. Owner-only (`listenPrivate`). Returns how to close it.
 */
export async function serveTools(sock: string, token: string, onTool: (tool: string, input: unknown) => { text: string; isError: boolean }): Promise<() => Promise<void>> {
  const server = createServer((req: IncomingMessage, res: ServerResponse) => {
    const answer = (status: number, body: unknown) => {
      res.writeHead(status, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify(body));
    };
    if (req.method !== 'POST' || req.url !== '/tools') return answer(404, { error: 'not found' });
    if (req.headers['x-ak-token'] !== token) return answer(403, { error: 'forbidden' });
    let body = '';
    req.setEncoding('utf8');
    req.on('data', (c: string) => {
      body += c;
      if (body.length > OUTPUT_MAX) req.destroy();
    });
    req.on('end', () => {
      let msg: { tool?: unknown; input?: unknown } = {};
      try {
        msg = JSON.parse(body) as typeof msg;
      } catch {
        return answer(400, { error: 'not JSON' });
      }
      answer(200, onTool(typeof msg.tool === 'string' ? msg.tool : '', msg.input));
    });
  });
  await listenPrivate(server, sock, "the code review's tools socket");
  return () => new Promise((resolve) => {
    server.close(() => resolve());
    server.closeAllConnections();
    rmSync(sock, { force: true });
  });
}

/** A one-time token for the tools socket. */
export const toolsToken = () => randomBytes(24).toString('base64url');
