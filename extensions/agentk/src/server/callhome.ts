import { execFile } from 'node:child_process';
import { randomUUID } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { promisify } from 'node:util';
import {
  type CallHomeKind, type FileTypeCount, type MekoUsage, type Stage, type CallHomeDay, type CallHomeInstall, type CallHomeMeko,
  type CallHomeReport, type CallHomeTokens, type YbCallHome,
} from '../shared/types.js';
import { mekoConfigured, resolveMekoApiUrl, resolveMekoWithSource, type Config } from './config.js';
import { openCodeCommand } from './opencode.js';
import { all, get, kvGet, kvSet, type Db } from './db.js';
import { readFileTypes, sumFileTypes } from './filetypes.js';
import { errMsg, type Logger } from './log.js';
import {
  agentkCalls, captureRuns, learnRuns, mekoCalls, mekoStarts, summarizeAgentk, summarizeCapture, summarizeLearn, summarizeMeko, toolCounts, toolSent,
  type AgentkCall, type CaptureRun, type LearnRun, type MekoStart,
} from './mekousage.js';
import { gapMarks, type GapMark } from './mekogaps.js';
import { usageOverview } from './usage.js';
import { HARNESS_CAPS, HARNESS_IDS, harnessOf, type HarnessId } from '../shared/harnesses.js';
import { priceFor } from '../shared/pricing.js';

// Anonymous usage stats ("Call Home"). Once a day (hourly in an install's first day) each install
// sends counts to a collector run by the AgentK team: no names, paths, prompts, code or
// anything that says who someone is.
// Every string in the report is an enum, a version, a model id or a file extension, and the
// report's shape is the collector's contract (CallHomeReport, docs/call-home-report.schema.json;
// the collector is yugabyte/agent-k's services/ak-local-call-home).
// The same report also goes to YugabyteDB's call home service (diagnostics.yugabyte.com, where yugabyted
// reports), in yugabyted's envelope (YbCallHome): a second CallHomeService with target `yugabyte`.

const execFileP = promisify(execFile);

const STAGES: Stage[] = ['explore', 'design', 'code', 'pr', 'review', 'merge', 'done', 'reviewing-pr'];
const DAY_MS = 24 * 3600 * 1000;

/** Days in a report: the first one an install sends goes back as far as session events are kept. */
export const REPORT_DAYS = 7;
export const FIRST_REPORT_DAYS = 30;
/** How often the server checks whether a report is due. */
export const CHECK_EVERY_MS = 3600 * 1000;
/** A report is due this long after the last one: just under a day, so hourly checks don't drift later each day. */
export const SEND_AFTER_MS = 23 * 3600 * 1000;
/** For a day after its first report, an install reports at every hourly check: just under an hour, for the same reason. */
export const FIRST_DAY_MS = DAY_MS;
export const FIRST_DAY_SEND_AFTER_MS = CHECK_EVERY_MS - 5 * 60 * 1000;
/** The first check after start waits a random time in this range, so a release doesn't make every install report at once. */
export const FIRST_CHECK_MS: [number, number] = [10 * 60 * 1000, 60 * 60 * 1000];
/** A new install (no report yet, or its first one under a day ago) checks sooner: its first report goes out within 5 minutes. */
export const NEW_INSTALL_FIRST_CHECK_MS: [number, number] = [60 * 1000, 5 * 60 * 1000];
/**
 * The largest body the collector takes. A busy install's 30-day first report can be bigger (every
 * day's file types and models full); a report over it drops its oldest days until it fits.
 */
export const MAX_REPORT_BYTES = 64 * 1024;
const SEND_TIMEOUT_MS = 15_000;
const PROBE_TIMEOUT_MS = 10_000;
/** tokensByModel keeps this many models; the rest are summed into `other`. */
const MODEL_LIMIT = 20;

const KV_INSTALL_ID = 'call_home_install_id';
const KV_FIRST_DAY = 'call_home_first_day';

/** YugabyteDB's call home service: release installs send there unless `[call_home] diagnostics_url` names another. */
export const YB_DIAGNOSTICS_URL = 'https://diagnostics.yugabyte.com';

/** Where a CallHomeService sends: AgentK's own collector, or YugabyteDB's call home service. */
export type CallHomeTarget = 'agentk' | 'yugabyte';

/**
 * Each target's schedule: when its last report was received, and its first (installs that reported
 * before the first was kept have none and report daily). The install id and first day are shared.
 */
const KV_SENT: Record<CallHomeTarget, { last: string; first: string }> = {
  agentk: { last: 'call_home_last_sent', first: 'call_home_first_sent' },
  yugabyte: { last: 'yb_call_home_last_sent', first: 'yb_call_home_first_sent' },
};

/** yugabyted's opt-out: YB_DISABLE_CALLHOME set to one of its true words turns off both targets. */
export function ybCallHomeDisabled(env: NodeJS.ProcessEnv = process.env): boolean {
  return /^(true|t|yes|y|1)$/i.test(env.YB_DISABLE_CALLHOME?.trim() ?? '');
}

/** A time as YugabyteDB's call home service parses it: UTC, microseconds, no zone. */
export function ybTimestamp(ms: number): string {
  return new Date(ms).toISOString().slice(0, 23) + '000';
}

/** Plain model ids (`claude-opus-5-5`), as the collector's schema has them. Anything else is `other`. */
export const MODEL_RE = /^[a-z0-9][a-z0-9.-]{0,63}$/;
/**
 * Versions (ours, Node's, Claude Code's), as the collector's schema has them: dotted
 * numbers, one letter after them, and a prerelease that is a lane word or a number, so a version
 * can't spell a name.
 */
export const VERSION_RE = /^v?[0-9]{1,9}(\.[0-9]{1,9}){0,3}[a-z]?(-(alpha|beta|rc|pre|dev|canary|nightly|next|[0-9]{1,9})(\.[0-9]{1,9}){0,3})?(\+[0-9]{1,14})?$/;

/**
 * A version as the report sends it: as is when the collector keeps it, else its leading dotted
 * numbers (a Node nightly `24.0.0-nightly20250101abc` gives `24.0.0`, a build `0.20261001.1-3-gabc`
 * gives `0.20261001.1`), else `0.0.0`.
 */
export function reportVersion(v: string): string {
  if (VERSION_RE.test(v)) return v;
  return /^v?[0-9]{1,9}(\.[0-9]{1,9}){0,3}/.exec(v)?.[0] ?? '0.0.0';
}

/** The UTC day of a time, as YYYY-MM-DD. */
export function utcDay(ms: number): string {
  return new Date(ms).toISOString().slice(0, 10);
}

const byKind = (): Record<CallHomeKind, number> => ({ explore: 0, plan: 0, quick: 0, 'review-pr': 0 });

/** The start a task's first stage stands for, under the old session types' names. */
const START_KINDS: Partial<Record<string, CallHomeKind>> = { explore: 'explore', design: 'plan', code: 'quick' };

/**
 * How the report counts a session: a Review a PR, or a task by the stage it started in. A session
 * with no stage history falls back to its stored kind (the old type, or `plan` for a task).
 */
export function callHomeKind(kind: string, firstStage: string | null): CallHomeKind | null {
  if (kind === 'review-pr') return 'review-pr';
  const start = firstStage != null && Object.hasOwn(START_KINDS, firstStage) ? START_KINDS[firstStage] : undefined;
  if (start) return start;
  return kind === 'explore' || kind === 'plan' || kind === 'quick' ? kind : null;
}
const byStage = (): Record<Stage, number> => Object.fromEntries(STAGES.map((s) => [s, 0])) as Record<Stage, number>;

function emptyDay(day: string): CallHomeDay {
  return {
    day, activeProjects: 0, activeSessions: 0, messages: 0, projectsCreated: 0, projectsDeleted: 0,
    sessionsCreated: byKind(), sessionsArchived: 0, stageEntries: byStage(), prsOpened: 0, prsMerged: 0,
    prCommits: 0, prAdditions: 0, prDeletions: 0, directPushes: 0, directCommits: 0, reviews: 0, ciFailures: 0,
    commentBatches: 0, recoveries: 0, resumes: 0, worktreesReused: 0, fileTypes: [], tokensByModel: [],
  };
}

const n = (v: unknown) => (typeof v === 'number' && Number.isFinite(v) && v > 0 ? v : 0);

function payload(json: string | null): Record<string, unknown> {
  if (!json) return {};
  try {
    const v = JSON.parse(json) as unknown;
    return v && typeof v === 'object' && !Array.isArray(v) ? (v as Record<string, unknown>) : {};
  } catch {
    return {};
  }
}

/** Model ids that aren't plain ids become `other`; after that, the MODEL_LIMIT largest by messages are kept. */
function capModels(rows: (CallHomeTokens & { model: string })[]): (CallHomeTokens & { model: string })[] {
  const by = new Map<string, CallHomeTokens & { model: string }>();
  for (const r of rows) {
    const model = MODEL_RE.test(r.model) ? r.model : 'other';
    const t = by.get(model) ?? { model, input: 0, output: 0, cacheRead: 0, cacheWrite: 0, messages: 0 };
    t.input += r.input;
    t.output += r.output;
    t.cacheRead += r.cacheRead;
    t.cacheWrite += r.cacheWrite;
    t.messages += r.messages;
    by.set(model, t);
  }
  const sorted = [...by.values()].sort((a, b) => b.messages - a.messages || (a.model < b.model ? -1 : 1));
  if (sorted.length <= MODEL_LIMIT) return sorted;
  const keep = sorted.filter((r) => r.model !== 'other').slice(0, MODEL_LIMIT - 1);
  const kept = new Set(keep.map((r) => r.model));
  const other = { model: 'other', input: 0, output: 0, cacheRead: 0, cacheWrite: 0, messages: 0 };
  for (const r of sorted) {
    if (kept.has(r.model)) continue;
    other.input += r.input;
    other.output += r.output;
    other.cacheRead += r.cacheRead;
    other.cacheWrite += r.cacheWrite;
    other.messages += r.messages;
  }
  return [...keep, other];
}

/** What AgentK did with Meko itself, for a day or all time: learn steps, its own calls, transcript captures. */
interface MekoExtras {
  sentByTool: CallHomeMeko['byTool'];
  learns: LearnRun[];
  agentk: AgentkCall[];
  capture: CaptureRun[];
  marks: GapMark[];
}

/**
 * Meko use as the report has it: counts only, calls by tool, the dollars in whole cents, nothing
 * below 0. Learn steps, AgentK's own calls, captures and questions marked only when there were some.
 */
function callHomeMeko(u: MekoUsage, byTool: CallHomeMeko['byTool'], starts: MekoStart[], x: MekoExtras): CallHomeMeko {
  const s: CallHomeMeko['starts'] = { found: 0, empty: 0, error: 0, late: 0, ms: 0 };
  for (const r of starts) {
    s[r.outcome]++;
    s.ms += n(r.ms);
  }
  return {
    calls: u.calls, lookups: u.lookups, found: u.found, empty: u.empty, weak: u.weak, errors: u.errors, saves: u.saves, foundTokens: u.foundTokens,
    byTool,
    savings: {
      searches: u.savings.searches,
      seeds: u.savings.seeds,
      // A very large result can make a saving negative; the schema takes counts.
      tokens: Math.max(0, Math.round(u.savings.tokens)),
      usdCents: Math.max(0, Math.round((u.savings.usd ?? 0) * 100)),
      unpricedSearches: u.savings.unpricedSearches,
      unpricedSeeds: u.savings.unpricedSeeds,
    },
    starts: s,
    sentTokens: u.sentTokens,
    sentByTool: x.sentByTool,
    ...(x.learns.length ? { learn: summarizeLearn(x.learns) } : {}),
    ...(x.agentk.length ? { agentk: summarizeAgentk(x.agentk) } : {}),
    ...(x.capture.length ? { capture: summarizeCapture(x.capture) } : {}),
    ...(x.marks.length ? { gaps: { dismissed: x.marks.filter((m) => m.state === 'dismissed').length, taught: x.marks.filter((m) => m.state === 'taught').length } } : {}),
  };
}

export interface ReportOptions {
  installId: string;
  install: CallHomeInstall;
  /** How many UTC days, ending today. */
  days: number;
  now?: number;
}

/** The report, from the database alone: only numeric columns, enums, model ids and file-type counts are read. */
export function buildReport(db: Db, o: ReportOptions): CallHomeReport {
  const now = o.now ?? Date.now();
  const today = Date.parse(utcDay(now));
  const start = today - (o.days - 1) * DAY_MS;
  const days = new Map<string, CallHomeDay>();
  for (let i = 0; i < o.days; i++) {
    const d = utcDay(start + i * DAY_MS);
    days.set(d, emptyDay(d));
  }
  const dayOf = (at: number) => (at >= start ? days.get(utcDay(at)) : undefined);
  const dayFiles = new Map<string, FileTypeCount[][]>();
  const addDayFiles = (d: CallHomeDay, list: FileTypeCount[] | null) => {
    if (list?.length) dayFiles.set(d.day, [...(dayFiles.get(d.day) ?? []), list]);
  };
  const DAY_SQL = "strftime('%Y-%m-%d', u.at / 1000, 'unixepoch')";

  // Activity and tokens.
  for (const r of all<{ day: string; projects: number; sessions: number; messages: number }>(db,
    `SELECT ${DAY_SQL} AS day, COUNT(DISTINCT s.project_id) AS projects, COUNT(DISTINCT u.session_id) AS sessions, COUNT(*) AS messages
     FROM usage_messages u JOIN sessions s ON s.id = u.session_id WHERE u.at >= ? GROUP BY day`, start)) {
    const d = days.get(r.day);
    if (!d) continue;
    d.activeProjects = r.projects;
    d.activeSessions = r.sessions;
    d.messages = r.messages;
  }
  const models = new Map<string, (CallHomeTokens & { model: string })[]>();
  for (const r of all<{ day: string; model: string; harness: string; input: number; output: number; cache_read: number; cache_write: number; messages: number }>(db,
    `SELECT ${DAY_SQL} AS day, u.model, s.harness, SUM(u.input) AS input, SUM(u.output) AS output, SUM(u.cache_read) AS cache_read,
            SUM(u.cache_write) AS cache_write, COUNT(*) AS messages
     FROM usage_messages u JOIN sessions s ON s.id = u.session_id WHERE u.at >= ? GROUP BY day, u.model, s.harness`, start)) {
    // A harness whose models can be named by the user (OpenCode's own providers) sends only known ones.
    const model = HARNESS_CAPS[harnessOf(r.harness)].userModels && !priceFor(r.model) ? 'other' : r.model;
    models.set(r.day, [...(models.get(r.day) ?? []),
      { model, input: n(r.input), output: n(r.output), cacheRead: n(r.cache_read), cacheWrite: n(r.cache_write), messages: n(r.messages) }]);
  }
  for (const [day, rows] of models) {
    const d = days.get(day);
    if (d) d.tokensByModel = capModels(rows);
  }

  // Projects.
  const projects = all<{ id: string; repo_path: string; created_at: number; deleted_at: number | null }>(db,
    'SELECT id, repo_path, created_at, deleted_at FROM projects');
  // Opened: new projects, and closed ones opened again. Closed: every close, including ones since
  // undone by a reopen (which clears deleted_at).
  for (const p of projects) {
    const c = dayOf(p.created_at);
    if (c) c.projectsCreated++;
  }
  for (const e of all<{ kind: string; at: number }>(db, 'SELECT kind, at FROM project_events')) {
    const d = dayOf(e.at);
    if (d && e.kind === 'reopened') d.projectsCreated++;
    else if (d) d.projectsDeleted++;
  }
  const current = projects.filter((p) => p.deleted_at == null);
  const currentIds = new Set(current.map((p) => p.id));
  const datapacks = all<{ project_id: string; role: string }>(db, 'SELECT project_id, role FROM project_datapacks')
    .filter((r) => currentIds.has(r.project_id));

  // Sessions.
  const sessions = all<{ kind: string; harness: string; first_stage: string | null; stage: string; desired: string; created_at: number; archived_at: number | null }>(db,
    `SELECT kind, harness, (SELECT stage FROM session_stages WHERE session_id = sessions.id ORDER BY entered_at, rowid LIMIT 1) AS first_stage,
            stage, desired, created_at, archived_at FROM sessions`);
  const kinds = byKind();
  const harnesses = Object.fromEntries(HARNESS_IDS.map((h) => [h, 0])) as Record<HarnessId, number>;
  const openByStage = byStage();
  for (const s of sessions) {
    const kind = callHomeKind(s.kind, s.first_stage);
    if (kind) kinds[kind]++;
    const harness = harnessOf(s.harness);
    harnesses[harness]++;
    if (s.desired === 'running' && (STAGES as string[]).includes(s.stage)) openByStage[s.stage as Stage]++;
    const c = dayOf(s.created_at);
    if (c && kind) c.sessionsCreated[kind]++;
    if (c && harness === 'kiro') (c.kiro ??= { sessionsCreated: 0, turns: 0, credits: 0 }).sessionsCreated++;
    const a = s.archived_at != null ? dayOf(s.archived_at) : undefined;
    if (a) a.sessionsArchived++;
  }
  for (const r of all<{ stage: string; entered_at: number }>(db, 'SELECT stage, entered_at FROM session_stages WHERE entered_at >= ?', start)) {
    const d = dayOf(r.entered_at);
    if (d && (STAGES as string[]).includes(r.stage)) d.stageEntries[r.stage as Stage]++;
  }

  // Kiro's turns and the credits they billed, on the day each ended.
  for (const r of all<{ day: string; turns: number; credits: number }>(db,
    `SELECT ${DAY_SQL} AS day, COUNT(*) AS turns, SUM(u.credits) AS credits FROM usage_messages u
     WHERE u.at >= ? AND u.credits IS NOT NULL GROUP BY day`, start)) {
    const d = days.get(r.day);
    if (!d) continue;
    const k = (d.kiro ??= { sessionsCreated: 0, turns: 0, credits: 0 });
    k.turns = n(r.turns);
    k.credits = Math.round((Number(r.credits) || 0) * 1000) / 1000;
  }

  // Session events (kept 30 days, except `merged`).
  for (const e of all<{ kind: string; text: string | null; payload_json: string | null; at: number }>(db,
    `SELECT kind, text, payload_json, at FROM session_events
     WHERE at >= ? AND kind IN ('pr', 'review', 'ci', 'comments', 'recovered', 'resumed', 'created')`, start)) {
    const d = dayOf(e.at);
    if (!d) continue;
    if (e.kind === 'pr') d.prsOpened++;
    // Reviews handed to Claude; not the "Requested another AgentK review" notes.
    else if (e.kind === 'review' && /^(AgentK review:|Review from )/.test(e.text ?? '')) d.reviews++;
    else if (e.kind === 'ci') d.ciFailures++;
    else if (e.kind === 'comments') d.commentBatches++;
    else if (e.kind === 'recovered') d.recoveries++;
    else if (e.kind === 'resumed') d.resumes++;
    else if (e.kind === 'created' && payload(e.payload_json).reused === true) d.worktreesReused++;
  }

  // Merged PRs: the first `merged` event of each session, with the PR's size once it was recorded.
  const shipped = { prCommits: 0, prAdditions: 0, prDeletions: 0 };
  const allFiles: FileTypeCount[][] = [];
  for (const m of all<{ payload_json: string | null; at: number }>(db,
    `SELECT payload_json, MIN(at) AS at FROM session_events WHERE kind = 'merged' GROUP BY session_id`)) {
    const p = payload(m.payload_json);
    const files = readFileTypes(p.fileTypes);
    shipped.prCommits += n(p.commits);
    shipped.prAdditions += n(p.additions);
    shipped.prDeletions += n(p.deletions);
    if (files) allFiles.push(files);
    const d = dayOf(m.at);
    if (!d) continue;
    d.prsMerged++;
    d.prCommits += n(p.commits);
    d.prAdditions += n(p.additions);
    d.prDeletions += n(p.deletions);
    addDayFiles(d, files);
  }

  // Direct pushes to a base branch.
  for (const p of all<{ at: number; commits: number | null; file_types: string | null }>(db, 'SELECT at, commits, file_types FROM pushes')) {
    const files = readFileTypes(p.file_types);
    if (files) allFiles.push(files);
    const d = dayOf(p.at);
    if (!d) continue;
    d.directPushes++;
    d.directCommits += n(p.commits);
    addDayFiles(d, files);
  }
  for (const [day, lists] of dayFiles) days.get(day)!.fileTypes = sumFileTypes(lists);

  // Meko: calls and seeds on the day they were made, searches at start on the day they ran, learn
  // steps on the day they began, AgentK's own calls and captures on the day they ran, and the
  // questions on What Meko doesn't know on the day they were dismissed or taught. A day with none
  // of them leaves `meko` out.
  const calls = mekoCalls(db);
  const starts = mekoStarts(db);
  const learns = learnRuns(db);
  const own = agentkCalls(db);
  const captures = captureRuns(db);
  const marks = gapMarks(db);
  const dayMeko = new Map<string, { calls: typeof calls; starts: MekoStart[]; learns: LearnRun[]; agentk: AgentkCall[]; capture: CaptureRun[]; marks: GapMark[] }>();
  const mekoDay = (at: number) => {
    const d = dayOf(at);
    if (!d) return undefined;
    let m = dayMeko.get(d.day);
    if (!m) dayMeko.set(d.day, (m = { calls: [], starts: [], learns: [], agentk: [], capture: [], marks: [] }));
    return m;
  };
  for (const c of calls) mekoDay(c.at)?.calls.push(c);
  for (const st of starts) mekoDay(st.at)?.starts.push(st);
  for (const l of learns) mekoDay(l.at)?.learns.push(l);
  for (const a of own) mekoDay(a.at)?.agentk.push(a);
  for (const c of captures) mekoDay(c.at)?.capture.push(c);
  for (const g of marks) mekoDay(g.at)?.marks.push(g);
  for (const [day, m] of dayMeko) {
    days.get(day)!.meko = callHomeMeko(summarizeMeko(m.calls), toolCounts(m.calls), m.starts, { sentByTool: toolSent(m.calls), ...m });
  }

  // Ask AgentK: asks on the day they were made; a day with none leaves `askAgentk` out.
  for (const a of all<{ at: number; ok: number; actions: number }>(db, 'SELECT at, ok, actions FROM agentk_asks WHERE at >= ?', start)) {
    const d = dayOf(a.at);
    if (!d) continue;
    const t = (d.askAgentk ??= { asks: 0, failed: 0, actions: 0 });
    t.asks++;
    if (!a.ok) t.failed++;
    t.actions += n(a.actions);
  }

  const overview = usageOverview(db, 1, now, calls);
  return {
    schema: 1,
    installId: o.installId,
    sentAt: new Date(now).toISOString(),
    install: o.install,
    totals: {
      projects: {
        current: current.length,
        deleted: projects.length - current.length,
        withDatapack: new Set(datapacks.filter((r) => r.role === 'dedicated').map((r) => r.project_id)).size,
        sharedDatapacks: datapacks.filter((r) => r.role === 'shared').length,
        repos: new Set(current.map((p) => p.repo_path)).size,
      },
      sessions: {
        total: sessions.length,
        open: sessions.filter((s) => s.desired === 'running').length,
        archived: sessions.filter((s) => s.desired === 'archived').length,
        byKind: kinds,
        byHarness: harnesses,
        openByStage,
      },
      shipped: { mergedPrs: overview.mergedPrs, directPushes: overview.directPushes, directCommits: overview.directCommits, ...shipped },
      fileTypes: sumFileTypes(allFiles),
      tokens: { input: overview.total.input, output: overview.total.output, cacheRead: overview.total.cacheRead, cacheWrite: overview.total.cacheWrite, messages: overview.total.messages },
      worktrees: { idle: get<{ n: number }>(db, 'SELECT COUNT(*) AS n FROM worktrees WHERE session_id IS NULL')?.n ?? 0 },
      meko: callHomeMeko(overview.meko, toolCounts(calls), starts, { sentByTool: toolSent(calls), learns, agentk: own, capture: captures, marks }),
      mekoFirstTurn: overview.mekoFirstTurn,
    },
    days: [...days.values()],
  };
}

/** Runs a command for its version output; rejects on failure or after PROBE_TIMEOUT_MS. */
export type Probe = (cmd: string, args: string[]) => Promise<string>;

const defaultProbe: Probe = async (cmd, args) => {
  const { stdout } = await execFileP(cmd, args, { timeout: PROBE_TIMEOUT_MS, env: { ...process.env, NO_COLOR: '1' } });
  return stdout;
};

/** The version a command prints, or '' when it can't be read. */
async function probeVersion(probe: Probe, cmd: string, args: string[], re: RegExp): Promise<string> {
  try {
    const v = re.exec(await probe(cmd, args))?.[1] ?? '';
    return VERSION_RE.test(v) ? v : '';
  } catch {
    return '';
  }
}

/** Which service manager runs this server, from what systemd and launchd put in its environment. */
function serviceKind(): CallHomeInstall['service'] {
  if (process.platform === 'linux' && process.env.INVOCATION_ID) return 'systemd';
  if (process.platform === 'darwin' && /com\.yugabyte\.(agentk|agent-k-local)\b/.test(process.env.XPC_SERVICE_NAME ?? '')) return 'launchd';
  return 'none';
}

export interface InstallInputs {
  config: Config;
  version: string;
  firstDay: string;
  probe?: Probe;
}

export async function installFacts(o: InstallInputs): Promise<CallHomeInstall> {
  const probe = o.probe ?? defaultProbe;
  const claudeCode = await probeVersion(probe, o.config.claude.command, ['--version'], /(\d+\.\d+\.\d+)/);
  const kiro = o.config.kiro.enabled ? await probeVersion(probe, o.config.kiro.command, ['--version'], /(\d+\.\d+\.\d+)/) : '';
  const opencode = o.config.opencode.enabled ? await probeVersion(probe, openCodeCommand(o.config.opencode.command), ['--version'], /(\d+\.\d+\.\d+)/) : '';
  const meko = mekoConfigured(o.config);
  return {
    version: reportVersion(o.version),
    // Meko installs AgentK's updates by its own [update] channel, which AgentK doesn't read.
    channel: 'stable',
    platform: process.platform,
    arch: process.arch,
    node: reportVersion(process.versions.node),
    claudeCode,
    ...(kiro ? { kiro } : {}),
    ...(opencode ? { opencode } : {}),
    access: 'meko',
    service: serviceKind(),
    meko: {
      enabled: meko,
      keySource: (meko && resolveMekoWithSource(o.config).keySource) || 'none',
      sharing: meko && resolveMekoApiUrl(o.config) ? 'api' : 'none',
    },
    firstDay: o.firstDay,
  };
}

/** The collector URL release builds carry: a CALL_HOME_URL file next to VERSION (see package-release.sh). */
export function bakedCallHomeUrl(appDir = process.env.AK_APP_DIR): string {
  if (!appDir) return '';
  try {
    return readFileSync(join(appDir, 'CALL_HOME_URL'), 'utf8').trim();
  } catch {
    return '';
  }
}

/** An http(s) URL, or null. */
function collectorUrl(raw: string): string | null {
  const s = raw.trim();
  try {
    const u = new URL(s);
    return u.protocol === 'https:' || u.protocol === 'http:' ? s : null;
  } catch {
    return null;
  }
}

export interface CallHomeOptions {
  /** Where reports go; `agentk` by default. */
  target?: CallHomeTarget;
  db: Db;
  config: () => Config;
  version: string;
  log: Logger;
  fetch?: typeof fetch;
  now?: () => number;
  /** The collector URL release builds carry; by default read from AK_APP_DIR. */
  bakedUrl?: () => string;
  /** Whether this is a release install, which sends to YugabyteDB with no URL configured; by default AK_APP_DIR is set. */
  release?: () => boolean;
  /** Where YB_DISABLE_CALLHOME is read; process.env by default. */
  env?: NodeJS.ProcessEnv;
  probe?: Probe;
  random?: () => number;
  /** MAX_REPORT_BYTES, for tests. */
  maxReportBytes?: number;
}

/** Sends the report (hourly in an install's first day, then daily) while `[call_home] enabled` is on and there is a collector URL. */
export class CallHomeService {
  private readonly target: CallHomeTarget;
  private readonly kv: { last: string; first: string };
  private readonly fetchImpl: typeof fetch;
  private readonly now: () => number;
  private baked: string | null = null;
  private sending: Promise<boolean> | null = null;
  /** The last send failed: failures are logged once until a send works again. */
  private failing = false;
  private firstCheck: NodeJS.Timeout | null = null;

  constructor(private o: CallHomeOptions) {
    this.target = o.target ?? 'agentk';
    this.kv = KV_SENT[this.target];
    this.fetchImpl = o.fetch ?? fetch;
    this.now = o.now ?? Date.now;
  }

  /**
   * Where reports go. AgentK's collector (its full report URL): `[call_home] url`, else the one this build
   * carries. YugabyteDB's: `[call_home] diagnostics_url`, else YB_DIAGNOSTICS_URL from a release install. Null when there's none.
   */
  endpoint(): string | null {
    if (this.target === 'yugabyte') {
      const configured = this.o.config().callHome.diagnosticsUrl;
      if (configured.trim()) return collectorUrl(configured);
      return (this.o.release ?? (() => !!process.env.AK_APP_DIR))() ? YB_DIAGNOSTICS_URL : null;
    }
    const configured = this.o.config().callHome.url;
    if (configured.trim()) return collectorUrl(configured);
    this.baked ??= (this.o.bakedUrl ?? bakedCallHomeUrl)();
    return this.baked ? collectorUrl(this.baked) : null;
  }

  enabled(): boolean {
    return this.o.config().callHome.enabled && !ybCallHomeDisabled(this.o.env);
  }

  installId(): string {
    let id = kvGet(this.o.db, KV_INSTALL_ID);
    if (!id) {
      id = randomUUID();
      kvSet(this.o.db, KV_INSTALL_ID, id);
    }
    return id;
  }

  /** When the last report was received; 0 before the first. */
  private lastSent(): number {
    return Number(kvGet(this.o.db, this.kv.last) ?? 0);
  }

  /** True until a day after the first report: no report yet, or the first one under FIRST_DAY_MS ago. */
  private firstDay(last: number, now: number): boolean {
    if (!last) return true;
    const first = Number(kvGet(this.o.db, this.kv.first) ?? 0);
    return first > 0 && now - first < FIRST_DAY_MS;
  }

  /**
   * The body the next send carries. One over the collector's limit would be refused every hour from
   * then on, so it drops its oldest days until it fits (the totals don't depend on the days). YugabyteDB's
   * gets the same report, as the payload of yugabyted's envelope.
   */
  async body(): Promise<string> {
    const now = this.now();
    let firstDay = kvGet(this.o.db, KV_FIRST_DAY);
    if (!firstDay) {
      firstDay = utcDay(now);
      kvSet(this.o.db, KV_FIRST_DAY, firstDay);
    }
    const install = await installFacts({ config: this.o.config(), version: this.o.version, firstDay, probe: this.o.probe });
    const first = !kvGet(this.o.db, this.kv.last);
    let r = buildReport(this.o.db, { installId: this.installId(), install, days: first ? FIRST_REPORT_DAYS : REPORT_DAYS, now });
    const max = this.o.maxReportBytes ?? MAX_REPORT_BYTES;
    let body = JSON.stringify(r);
    for (let drop = 1, all = r.days; Buffer.byteLength(body) > max && drop <= all.length; drop++) {
      r = { ...r, days: all.slice(drop) };
      body = JSON.stringify(r);
    }
    if (this.target === 'agentk') return body;
    const yb: YbCallHome = {
      callhome_type: 'agentk', command: 'heartbeat', timestamp: ybTimestamp(now), install_id: r.installId, version: r.install.version, payload: r,
    };
    return JSON.stringify(yb);
  }

  /** Sends a report if one is due. Never throws; true when one was sent. */
  tick(): Promise<boolean> {
    this.sending ??= this.sendIfDue().finally(() => {
      this.sending = null;
    });
    return this.sending;
  }

  private async sendIfDue(): Promise<boolean> {
    if (!this.enabled()) return false;
    const url = this.endpoint();
    if (!url) return false;
    // Reports are stamped with the check's time, not the send's, so time spent building and
    // sending one doesn't push the next hourly check under FIRST_DAY_SEND_AFTER_MS.
    const now = this.now();
    const last = this.lastSent();
    if (last && now - last < (this.firstDay(last, now) ? FIRST_DAY_SEND_AFTER_MS : SEND_AFTER_MS)) return false;
    try {
      const body = await this.body();
      // Turned off while the report was being built.
      if (!this.enabled()) return false;
      const res = await this.fetchImpl(url, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'User-Agent': `${this.target === 'agentk' ? 'agent-k-local' : 'agentk'}/${this.o.version}` },
        body,
        signal: AbortSignal.timeout(SEND_TIMEOUT_MS),
      });
      if (!res.ok) throw new Error(`the collector answered ${res.status}`);
      if (!last) kvSet(this.o.db, this.kv.first, String(now));
      kvSet(this.o.db, this.kv.last, String(now));
      if (this.failing) this.o.log.info(`usage stats sent${this.to()}`);
      this.failing = false;
      return true;
    } catch (e) {
      if (!this.failing) this.o.log.warn(`sending usage stats${this.to()} failed; trying again every hour`, errMsg(e));
      this.failing = true;
      return false;
    }
  }

  /** Where reports go, for log lines: nothing for AgentK's collector, as before there were two. */
  private to(): string {
    return this.target === 'yugabyte' ? ' to YugabyteDB' : '';
  }

  /**
   * Checks after a random first delay, then through `every` (Core's timers) once an hour. A new
   * install's first delay is short, and no earlier than its next hourly report is due, so a restart
   * in its first day doesn't skip one.
   */
  start(every: (ms: number, fn: () => Promise<unknown>) => void): void {
    const now = this.now();
    const last = this.lastSent();
    const first = this.firstDay(last, now);
    const [lo, hi] = first ? NEW_INSTALL_FIRST_CHECK_MS : FIRST_CHECK_MS;
    let delay = lo + Math.floor((this.o.random ?? Math.random)() * (hi - lo));
    if (first && last) delay = Math.max(delay, last + FIRST_DAY_SEND_AFTER_MS - now);
    this.firstCheck = setTimeout(() => {
      this.firstCheck = null;
      void this.tick();
      every(CHECK_EVERY_MS, () => this.tick());
    }, delay);
    this.firstCheck.unref();
  }

  stop(): void {
    if (this.firstCheck) clearTimeout(this.firstCheck);
    this.firstCheck = null;
  }
}
