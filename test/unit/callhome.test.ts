import { readFileSync, writeFileSync } from 'node:fs';
import { hostname, userInfo } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { CallHomeInstall, CallHomeReport } from '../../src/shared/types.js';
import { DEFAULT_CONFIG, type Config } from '../../src/server/config.js';
import { kvGet, kvSet, openDb, run, type Db } from '../../src/server/db.js';
import { FILE_TYPE_LIMIT, countFileTypes, extOf, parseNumstatZ, readFileTypes, sumFileTypes } from '../../src/server/filetypes.js';
import type { Logger } from '../../src/server/log.js';
import {
  CHECK_EVERY_MS, FIRST_CHECK_MS, FIRST_DAY_MS, FIRST_DAY_SEND_AFTER_MS, FIRST_REPORT_DAYS, MAX_REPORT_BYTES, NEW_INSTALL_FIRST_CHECK_MS, REPORT_DAYS,
  SEND_AFTER_MS, VERSION_RE, CallHomeService, reportVersion, bakedCallHomeUrl, buildReport, callHomeKind,
  installFacts, type Probe,
} from '../../src/server/callhome.js';
import { markGap } from '../../src/server/mekogaps.js';
import { REPORTED_TOOLS, firstTurnMeko } from '../../src/server/mekousage.js';
import { usageOverview } from '../../src/server/usage.js';
import { tempDir } from '../helpers.js';

const SCHEMA = JSON.parse(readFileSync(fileURLToPath(new URL('../../docs/call-home-report.schema.json', import.meta.url)), 'utf8')) as Schema;

// ---- a small JSON Schema checker for the keywords docs/call-home-report.schema.json uses ----
// (The collector, yugabyte/agent-k services/ak-local-call-home, has an identical copy.)

type Schema = { [k: string]: unknown };
const KEYWORDS = new Set([
  '$schema', '$id', 'title', 'description', '$defs', '$ref', 'type', 'const', 'enum', 'pattern', 'format', 'minimum', 'maximum',
  'properties', 'propertyNames', 'required', 'additionalProperties', 'items', 'minItems', 'maxItems', 'maxProperties', 'anyOf',
]);

/** Every keyword in the schema, so the checker below is known to cover all of them. */
function keywords(s: unknown, out = new Set<string>()): Set<string> {
  if (!s || typeof s !== 'object' || Array.isArray(s)) return out;
  for (const [k, v] of Object.entries(s)) {
    out.add(k);
    if (k === 'properties' || k === '$defs') for (const sub of Object.values(v as object)) keywords(sub, out);
    else if (k === 'items' || k === 'propertyNames' || k === 'additionalProperties') keywords(v, out);
    else if (k === 'anyOf') for (const sub of v as unknown[]) keywords(sub, out);
  }
  return out;
}

function typeOf(v: unknown): string {
  if (v === null) return 'null';
  if (Array.isArray(v)) return 'array';
  if (typeof v === 'number') return Number.isInteger(v) ? 'integer' : 'number';
  return typeof v;
}

/** The errors in `v` against `s` (empty when it's valid). */
function validate(v: unknown, s: Schema, at = '$', root: Schema = s): string[] {
  if (typeof s.$ref === 'string') return validate(v, (root.$defs as Record<string, Schema>)[s.$ref.replace('#/$defs/', '')], at, root);
  const errs: string[] = [];
  if (s.anyOf) {
    if (!(s.anyOf as Schema[]).some((sub) => !validate(v, sub, at, root).length)) errs.push(`${at}: matches none of anyOf`);
    return errs;
  }
  if ('const' in s && v !== s.const) errs.push(`${at}: should be ${JSON.stringify(s.const)}`);
  if (s.enum && !(s.enum as unknown[]).includes(v)) errs.push(`${at}: ${JSON.stringify(v)} is not one of ${JSON.stringify(s.enum)}`);
  const t = typeOf(v);
  if (s.type && !(s.type === t || (s.type === 'number' && t === 'integer'))) return [...errs, `${at}: should be ${String(s.type)}, is ${t}`];
  if (typeof s.pattern === 'string' && typeof v === 'string' && !new RegExp(s.pattern).test(v)) errs.push(`${at}: ${JSON.stringify(v)} doesn't match ${s.pattern}`);
  if (typeof s.minimum === 'number' && typeof v === 'number' && v < s.minimum) errs.push(`${at}: below ${s.minimum}`);
  if (typeof s.maximum === 'number' && typeof v === 'number' && v > s.maximum) errs.push(`${at}: above ${s.maximum}`);
  if (s.format === 'date-time' && typeof v === 'string' && !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$/.test(v)) {
    errs.push(`${at}: not a date-time`);
  }
  if (t === 'object') {
    const o = v as Record<string, unknown>;
    const props = (s.properties ?? {}) as Record<string, Schema>;
    for (const r of (s.required as string[]) ?? []) if (!(r in o)) errs.push(`${at}: missing ${r}`);
    if (typeof s.maxProperties === 'number' && Object.keys(o).length > s.maxProperties) errs.push(`${at}: more than ${s.maxProperties} properties`);
    for (const [k, val] of Object.entries(o)) {
      if (s.propertyNames) errs.push(...validate(k, s.propertyNames as Schema, `${at}{${k}}`, root));
      if (props[k]) errs.push(...validate(val, props[k], `${at}.${k}`, root));
      else if (s.additionalProperties === false) errs.push(`${at}: unexpected ${k}`);
      else if (s.additionalProperties && typeof s.additionalProperties === 'object') errs.push(...validate(val, s.additionalProperties as Schema, `${at}.${k}`, root));
    }
  }
  if (t === 'array') {
    const a = v as unknown[];
    if (typeof s.minItems === 'number' && a.length < s.minItems) errs.push(`${at}: fewer than ${s.minItems} items`);
    if (typeof s.maxItems === 'number' && a.length > s.maxItems) errs.push(`${at}: more than ${s.maxItems} items`);
    if (s.items) a.forEach((x, i) => errs.push(...validate(x, s.items as Schema, `${at}[${i}]`, root)));
  }
  return errs;
}

// ---- seeded database ----

const NOW = Date.parse('2026-09-28T12:00:00Z');
const at = (iso: string) => Date.parse(iso);

const INSTALL: CallHomeInstall = {
  version: '0.20260928.0', channel: 'stable', platform: 'linux', arch: 'x64', node: '22.18.0', claudeCode: '2.4.1',
  access: 'remote', service: 'none', meko: { enabled: true, keySource: 'claude', sharing: 'api' }, firstDay: '2026-07-02',
};
const INSTALL_ID = '5f0c7a8e-3b1d-4c2e-9f6a-1b2c3d4e5f60';

/** Names, paths and URLs that must never reach a report. */
const SECRETS = ['Secret Project Zeta', 'zeta', 'alice', 'secret-branch', 'Secret task', 'adlc/proj', 'github.com', '123456789012', '/srv/gone', 'wt-1', 'Gone for good'];

function seeded(): Db {
  const db = openDb(':memory:');
  const project = (id: string, name: string, repo: string, created: number, deleted: number | null = null) => {
    run(db, 'INSERT INTO projects(id, name, repo_path, default_branch, created_at, deleted_at) VALUES (?, ?, ?, ?, ?, ?)', id, name, repo, 'main', created, deleted);
    if (deleted !== null) run(db, `INSERT INTO project_events(project_id, kind, at) VALUES (?, 'closed', ?)`, id, deleted);
  };
  project('p1', 'Secret Project Zeta', '/home/alice/zeta-repo', at('2026-09-27T10:00:00Z'));
  project('p2', 'Other', '/home/alice/zeta-repo', at('2026-01-01T00:00:00Z'));
  project('p3', 'Gone for good', '/srv/gone', at('2026-01-01T00:00:00Z'), at('2026-09-26T05:00:00Z'));
  for (const [p, dp, name, role] of [['p1', 'dp-1', 'adlc/proj/zeta/secret', 'dedicated'], ['p1', 'dp-2', 'team/zeta-kb', 'shared'], ['p3', 'dp-3', 'adlc/proj/gone', 'dedicated']]) {
    run(db, 'INSERT INTO project_datapacks(project_id, datapack_id, name, role) VALUES (?, ?, ?, ?)', p, dp, name, role);
  }
  const session = (id: string, p: string, kind: string, desired: string, stage: string, created: number, archived: number | null = null) =>
    run(db, `INSERT INTO sessions(id, project_id, slug, name, claude_session_id, branch, base_branch, worktree_path, tmux_name, hook_token,
             desired, status, stage, kind, created_at, archived_at) VALUES (?, ?, ?, 'Secret task', 'c', 'ak/secret-branch', 'main', '/home/alice/wt-1', 't', ?, ?, 'idle', ?, ?, ?, ?)`,
      id, p, id, `tok-${id}`, desired, stage, kind, created, archived);
  session('s1', 'p1', 'plan', 'running', 'review', at('2026-09-27T11:00:00Z'));
  session('s2', 'p1', 'quick', 'archived', 'done', at('2026-09-20T09:00:00Z'), at('2026-09-28T01:00:00Z'));
  session('s3', 'p2', 'explore', 'running', 'explore', at('2026-01-01T00:00:00Z'));
  session('s4', 'p3', 'plan', 'archived', 'done', at('2026-01-01T00:00:00Z'), at('2026-01-02T00:00:00Z'));
  const stage = (s: string, st: string, iso: string) =>
    run(db, "INSERT INTO session_stages(session_id, stage, entered_at, moved_by) VALUES (?, ?, ?, 'auto')", s, st, at(iso));
  stage('s1', 'design', '2026-09-27T11:00:00Z');
  stage('s1', 'code', '2026-09-27T12:00:00Z');
  stage('s1', 'pr', '2026-09-27T13:00:00Z');
  stage('s1', 'review', '2026-09-27T14:00:00Z');
  stage('s2', 'done', '2026-09-28T01:00:00Z');
  stage('s4', 'done', '2026-01-02T00:00:00Z');
  const event = (s: string, p: string, kind: string, text: string, iso: string, payload?: unknown) =>
    run(db, 'INSERT INTO session_events(session_id, project_id, kind, text, payload_json, at) VALUES (?, ?, ?, ?, ?, ?)',
      s, p, kind, text, payload === undefined ? null : JSON.stringify(payload), at(iso));
  event('s1', 'p1', 'created', 'Started ak/secret-branch from main in a reused worktree (wt-1)', '2026-09-27T11:00:00Z', { reused: true });
  event('s1', 'p1', 'pr', 'Opened PR #7', '2026-09-27T13:00:00Z', { url: 'https://github.com/alice/zeta/pull/7' });
  event('s1', 'p1', 'pr', 'Opened PR #3', '2026-09-01T13:00:00Z', { url: 'https://github.com/alice/zeta/pull/3' });
  event('s1', 'p1', 'review', 'AgentK review: 2 findings in secret-branch', '2026-09-27T14:30:00Z');
  event('s1', 'p1', 'review', 'Requested another AgentK review', '2026-09-27T14:40:00Z');
  event('s1', 'p1', 'ci', 'CI failed: build', '2026-09-27T14:50:00Z');
  event('s1', 'p1', 'comments', 'Sent 3 to AgentK', '2026-09-27T15:10:00Z');
  event('s1', 'p1', 'recovered', 'Relaunched', '2026-09-27T16:10:00Z');
  event('s1', 'p1', 'resumed', 'Resumed', '2026-09-27T16:20:00Z');
  event('s2', 'p1', 'created', 'Started', '2026-09-20T09:00:00Z', { reused: false });
  const pr = {
    url: 'https://github.com/alice/zeta/pull/9', commits: 3, additions: 100, deletions: 20,
    fileTypes: [{ ext: 'ts', files: 2, additions: 90, deletions: 15 }, { ext: 'md', files: 1, additions: 10, deletions: 5 }],
  };
  event('s2', 'p1', 'merged', 'PR #9 merged', '2026-09-28T00:30:00Z', pr);
  // Logged again after a restart: still one merge.
  event('s2', 'p1', 'merged', 'PR #9 merged', '2026-09-28T00:40:00Z', pr);
  event('s4', 'p3', 'merged', 'PR #2 merged', '2026-01-02T00:00:00Z', { url: 'https://github.com/alice/gone/pull/2' });
  run(db, 'INSERT INTO pushes(session_id, range, ref, at, commits, file_types) VALUES (?, ?, ?, ?, ?, ?)', 's1', 'aaa1111..bbb2222', 'main',
    at('2026-09-26T08:00:00Z'), 2, JSON.stringify([{ ext: 'ts', files: 1, additions: 5, deletions: 1 }, { ext: 'none', files: 1, additions: 1, deletions: 0 }]));
  run(db, 'INSERT INTO pushes(session_id, range, ref, at, commits, file_types) VALUES (?, ?, ?, ?, ?, ?)', 's3', 'ccc3333..ddd4444', 'main',
    at('2026-01-05T08:00:00Z'), -1, null);
  const usage = (s: string, id: string, model: string, iso: string, u: [number, number, number, number]) =>
    run(db, `INSERT INTO usage_messages(session_id, message_id, agent, model, at, input, output, cache_read, cache_write)
             VALUES (?, ?, NULL, ?, ?, ?, ?, ?, ?)`, s, id, model, at(iso), ...u);
  usage('s1', 'm1', 'claude-opus-5-5', '2026-09-27T15:00:00Z', [10, 100, 1000, 50]);
  usage('s1', 'm2', 'claude-haiku-4-5', '2026-09-27T16:00:00Z', [1, 10, 0, 0]);
  usage('s2', 'm3', 'claude-opus-5-5', '2026-09-28T00:10:00Z', [2, 20, 200, 10]);
  usage('s3', 'm4', 'arn:aws:bedrock:us-east-1:123456789012:application-inference-profile/abc', '2026-09-26T09:00:00Z', [3, 30, 300, 0]);
  usage('s4', 'm5', 'claude-opus-5-5', '2026-01-02T00:00:00Z', [4, 40, 400, 0]);
  const meko = (s: string, id: string, tool: string, kind: string, iso: string, outcome: string, resultChars: number | null, model: string | null, context: number | null) =>
    run(db, `INSERT INTO meko_calls(session_id, tool_use_id, agent, tool, kind, at, outcome, results, input_chars, result_chars, model, context_tokens)
             VALUES (?, ?, NULL, ?, ?, ?, ?, NULL, 40, ?, ?, ?)`, s, id, tool, kind, at(iso), outcome, resultChars, model, context);
  meko('s1', 'tu1', 'context_search', 'lookup', '2026-09-27T14:00:00Z', 'found', 4000, 'claude-opus-5-5', 10_000);
  meko('s1', 'tu2', 'memory_search', 'lookup', '2026-09-27T14:10:00Z', 'empty', 20, 'claude-opus-5-5', 10_000);
  meko('s1', 'tu3', 'memory_add', 'save', '2026-09-27T14:20:00Z', 'ok', 30, 'claude-opus-5-5', 10_000);
  meko('s1', 'tu4', 'datapack_list', 'other', '2026-09-27T14:30:00Z', 'ok', 30, 'claude-opus-5-5', 10_000);
  meko('s2', 'tu5', 'memory_get_by_id', 'lookup', '2026-09-28T02:00:00Z', 'error', 10, 'claude-opus-5-5', 5000);
  // Before the window, on a model with no known price.
  meko('s3', 'tu6', 'knowledgebase_search', 'lookup', '2026-01-05T10:00:00Z', 'found', 1000, null, 5000);
  // What Meko doesn't know: a search that found nothing relevant, and questions taught and dismissed. Only counts are sent.
  run(db, `UPDATE meko_calls SET query = 'Secret task retention', query_key = 'secret task retention', quality = 'weak' WHERE tool_use_id = 'tu1'`);
  markGap(db, 'p1', 'secret task retention', { kind: 'teach', text: 'Secret task notes', datapack: 'adlc/proj/zeta' }, at('2026-09-28T03:00:00Z'));
  markGap(db, 'p1', 'zeta', { kind: 'dismiss' }, at('2026-09-27T15:00:00Z'));
  // Restored: its dismissal no longer counts.
  markGap(db, 'p1', 'restored', { kind: 'dismiss' }, at('2026-09-27T15:50:00Z'));
  markGap(db, 'p1', 'restored', { kind: 'restore' }, at('2026-09-27T16:00:00Z'));
  // Dismissed by a person, asked again, then taught and dismissed by a learn step: the person's
  // dismissal still counts; the learn step's marks are counted with the learn step, not here.
  markGap(db, 'p1', 'by agentk', { kind: 'dismiss' }, at('2026-09-27T14:00:00Z'));
  markGap(db, 'p1', 'by agentk', { kind: 'teach', text: 'Notes', datapack: 'adlc/proj/zeta', session: 's1' }, at('2026-09-27T15:30:00Z'));
  markGap(db, 'p1', 'by agentk', { kind: 'dismiss', session: 's1', memory: 'Notes' }, at('2026-09-28T03:30:00Z'));
  const preload = (s: string, iso: string, outcome: string, chars: number, ms: number) =>
    run(db, 'INSERT INTO meko_preloads(session_id, at, outcome, results, chars, ms) VALUES (?, ?, ?, ?, ?, ?)', s, at(iso), outcome, chars ? 2 : 0, chars, ms);
  preload('s1', '2026-09-27T11:00:00Z', 'found', 800, 1200);
  preload('s2', '2026-09-20T09:00:00Z', 'empty', 0, 300);
  preload('s3', '2026-09-26T09:00:00Z', 'late', 0, 5000);
  run(db, 'UPDATE sessions SET first_turn_end_at = ? WHERE id = ?', at('2026-09-27T15:30:00Z'), 's1');
  run(db, 'INSERT INTO worktrees(path, repo, session_id, created_at) VALUES (?, ?, NULL, ?)', '/home/alice/wt-2', '/home/alice/zeta-repo', NOW);
  run(db, 'INSERT INTO worktrees(path, repo, session_id, created_at) VALUES (?, ?, ?, ?)', '/home/alice/wt-1', '/home/alice/zeta-repo', 's1', NOW);
  return db;
}

const report = (db: Db, days = REPORT_DAYS) => buildReport(db, { installId: INSTALL_ID, install: INSTALL, days, now: NOW });

describe('file types', () => {
  it('counts a file under its last suffix, lowercased', () => {
    expect(extOf('src/App.TSX')).toBe('tsx');
    expect(extOf('a.test.ts')).toBe('ts');
    expect(extOf('dir.v2/Makefile')).toBe('none');
    expect(extOf('.gitignore')).toBe('none');
    expect(extOf('notes.')).toBe('none');
    expect(extOf('archive.tar.gz')).toBe('gz');
    // Anything that could carry a name counts as other.
    expect(extOf('report.my-secret-name')).toBe('other');
    expect(extOf('x.averyveryverylongsuffix')).toBe('other');
    expect(extOf('x.ünï')).toBe('other');
  });

  it('sums files per extension, most files first', () => {
    expect(countFileTypes([
      { path: 'a.ts', additions: 3, deletions: 1 }, { path: 'b.ts', additions: 2, deletions: 0 },
      { path: 'README.md', additions: 9, deletions: 9 }, { path: 'Makefile', additions: 1, deletions: 0 },
    ])).toEqual([
      { ext: 'ts', files: 2, additions: 5, deletions: 1 },
      { ext: 'md', files: 1, additions: 9, deletions: 9 },
      { ext: 'none', files: 1, additions: 1, deletions: 0 },
    ]);
  });

  it(`keeps the ${FILE_TYPE_LIMIT} largest and sums the rest into other`, () => {
    const files = Array.from({ length: 40 }, (_, i) => Array.from({ length: 40 - i }, () => ({ path: `f.e${i}`, additions: 1, deletions: 0 }))).flat();
    const out = countFileTypes([...files, { path: 'x.my-secret', additions: 1, deletions: 0 }]);
    expect(out).toHaveLength(FILE_TYPE_LIMIT);
    expect(out.slice(0, 3).map((c) => c.ext)).toEqual(['e0', 'e1', 'e2']);
    const other = out.at(-1)!;
    expect(other.ext).toBe('other');
    // e24 … e39 (16 down to 1 files each), plus the odd one.
    expect(other.files).toBe((16 * 17) / 2 + 1);
    expect(out.reduce((n, c) => n + c.files, 0)).toBe(files.length + 1);
  });

  it('adds lists together and never passes an unknown extension through', () => {
    expect(sumFileTypes([
      [{ ext: 'ts', files: 1, additions: 1, deletions: 1 }],
      [{ ext: 'ts', files: 2, additions: 3, deletions: 0 }, { ext: 'Secret Name', files: 1, additions: 0, deletions: 0 }],
    ])).toEqual([{ ext: 'ts', files: 3, additions: 4, deletions: 1 }, { ext: 'other', files: 1, additions: 0, deletions: 0 }]);
    expect(readFileTypes('[{"ext":"ts","files":1,"additions":0,"deletions":0}, 5]')).toEqual([{ ext: 'ts', files: 1, additions: 0, deletions: 0 }]);
    expect(readFileTypes('not json')).toBeNull();
    expect(readFileTypes(null)).toBeNull();
  });

  it('reads git diff --numstat -z, binaries and renames included', () => {
    const out = '3\t1\tsrc/a.ts\0-\t-\tlogo.png\0' + '2\t0\t\0old/name.js\0new/name.ts\0' + '1\t1\tweird\tname\nwith lines.md\0';
    expect(parseNumstatZ(out)).toEqual([
      { path: 'src/a.ts', additions: 3, deletions: 1 },
      { path: 'logo.png', additions: 0, deletions: 0 },
      { path: 'new/name.ts', additions: 2, deletions: 0 },
      { path: 'weird\tname\nwith lines.md', additions: 1, deletions: 1 },
    ]);
    expect(parseNumstatZ('')).toEqual([]);
  });
});

describe('the usage stats report', () => {
  it('buckets activity by UTC day, oldest first', () => {
    const r = report(seeded());
    expect(r.days.map((d) => d.day)).toEqual(['2026-09-22', '2026-09-23', '2026-09-24', '2026-09-25', '2026-09-26', '2026-09-27', '2026-09-28']);
    const day = (d: string) => r.days.find((x) => x.day === d)!;

    expect(day('2026-09-27')).toMatchObject({
      activeProjects: 1, activeSessions: 1, messages: 2, projectsCreated: 1, projectsDeleted: 0,
      sessionsCreated: { plan: 1, quick: 0, explore: 0 },
      stageEntries: { explore: 0, design: 1, code: 1, pr: 1, review: 1, merge: 0, done: 0 },
      prsOpened: 1, prsMerged: 0, reviews: 1, ciFailures: 1, commentBatches: 1, recoveries: 1, resumes: 1, worktreesReused: 1,
      tokensByModel: [
        { model: 'claude-haiku-4-5', input: 1, output: 10, cacheRead: 0, cacheWrite: 0, messages: 1 },
        { model: 'claude-opus-5-5', input: 10, output: 100, cacheRead: 1000, cacheWrite: 50, messages: 1 },
      ],
    });
    expect(day('2026-09-28')).toMatchObject({
      activeSessions: 1, messages: 1, sessionsArchived: 1, stageEntries: { done: 1 },
      prsMerged: 1, prCommits: 3, prAdditions: 100, prDeletions: 20,
      fileTypes: [{ ext: 'ts', files: 2, additions: 90, deletions: 15 }, { ext: 'md', files: 1, additions: 10, deletions: 5 }],
    });
    expect(day('2026-09-26')).toMatchObject({
      activeProjects: 1, projectsDeleted: 1, directPushes: 1, directCommits: 2,
      fileTypes: [{ ext: 'ts', files: 1, additions: 5, deletions: 1 }, { ext: 'none', files: 1, additions: 1, deletions: 0 }],
      // A model id that isn't a plain id (this one carries an account number) counts as other.
      tokensByModel: [{ model: 'other', input: 3, output: 30, cacheRead: 300, cacheWrite: 0, messages: 1 }],
    });
    expect(day('2026-09-22')).toMatchObject({ messages: 0, prsOpened: 0, fileTypes: [], tokensByModel: [] });
    // The older PR is outside the window.
    expect(r.days.reduce((n, d) => n + d.prsOpened, 0)).toBe(1);
  });

  it('counts a session by the stage it started in, under the names the session types had', () => {
    // New tasks are stored as plan: the first stage says where they started.
    expect(callHomeKind('plan', 'explore')).toBe('explore');
    expect(callHomeKind('plan', 'design')).toBe('plan');
    expect(callHomeKind('plan', 'code')).toBe('quick');
    expect(callHomeKind('review-pr', 'reviewing-pr')).toBe('review-pr');
    // A task changed type later still counts where it started.
    expect(callHomeKind('quick', 'design')).toBe('plan');
    // No stage history: the stored kind.
    expect(callHomeKind('quick', null)).toBe('quick');
    expect(callHomeKind('task', null)).toBeNull();
    expect(callHomeKind('plan', 'constructor')).toBe('plan');
  });

  it('counts a close that a reopen undid, and the reopen as a project opened', () => {
    const db = seeded();
    // p3 was closed on the 26th and opened again on the 28th: deleted_at is cleared.
    run(db, 'UPDATE projects SET deleted_at = NULL WHERE id = ?', 'p3');
    run(db, `INSERT INTO project_events(project_id, kind, at) VALUES ('p3', 'reopened', ?)`, at('2026-09-28T09:00:00Z'));
    const r = report(db);
    const day = (d: string) => r.days.find((x) => x.day === d)!;
    expect(day('2026-09-26').projectsDeleted).toBe(1);
    expect(day('2026-09-28').projectsCreated).toBe(1);
    expect(r.totals.projects).toMatchObject({ current: 3, deleted: 0 });
  });

  it('sends 30 days in a first report', () => {
    const r = report(seeded(), FIRST_REPORT_DAYS);
    expect(r.days).toHaveLength(30);
    expect(r.days[0].day).toBe('2026-08-30');
    expect(r.days.reduce((n, d) => n + d.prsOpened, 0)).toBe(2);
  });

  it('adds up all-time totals, deleted projects and archived sessions included', () => {
    const db = seeded();
    const r = report(db);
    const overview = usageOverview(db);
    expect(r.totals).toEqual({
      projects: { current: 2, deleted: 1, withDatapack: 1, sharedDatapacks: 1, repos: 1 },
      sessions: {
        total: 4, open: 2, archived: 2, byKind: { explore: 1, plan: 2, quick: 1, 'review-pr': 0 }, byHarness: { claude: 4, kiro: 0 },
        openByStage: { explore: 1, design: 0, code: 0, pr: 0, review: 1, merge: 0, done: 0, 'reviewing-pr': 0 },
      },
      shipped: { mergedPrs: 2, directPushes: 2, directCommits: 2, prCommits: 3, prAdditions: 100, prDeletions: 20 },
      fileTypes: [
        { ext: 'ts', files: 3, additions: 95, deletions: 16 },
        { ext: 'md', files: 1, additions: 10, deletions: 5 },
        { ext: 'none', files: 1, additions: 1, deletions: 0 },
      ],
      tokens: overview.total,
      worktrees: { idle: 1 },
      meko: r.totals.meko,
      mekoFirstTurn: firstTurnMeko(db),
    });
    expect(r.totals.shipped).toMatchObject({ mergedPrs: overview.mergedPrs, directPushes: overview.directPushes, directCommits: overview.directCommits });
    expect(r).toMatchObject({ schema: 1, installId: INSTALL_ID, sentAt: '2026-09-28T12:00:00.000Z', install: INSTALL });
  });

  it("counts sessions by harness, and Kiro's turns and the credits they billed on their day, apart from tokens", () => {
    const db = seeded();
    run(db, `UPDATE sessions SET harness = 'kiro' WHERE id = 's1'`);
    run(db, `INSERT INTO usage_messages(session_id, message_id, agent, model, at, input, output, cache_read, cache_write, credits)
             VALUES ('s1', 'kiro:c:1', NULL, 'auto', ?, 0, 0, 0, 0, 0.25)`, at('2026-09-27T17:00:00Z'));
    const r = report(db);
    expect(r.totals.sessions.byHarness).toEqual({ claude: 3, kiro: 1 });
    expect(r.days.find((d) => d.day === '2026-09-27')!.kiro).toEqual({ sessionsCreated: 1, turns: 1, credits: 0.25 });
    expect(r.days.find((d) => d.day === '2026-09-26')!.kiro).toBeUndefined();
    expect(r.totals.tokens).not.toHaveProperty('credits');
  });

  it('counts Meko use, all time and on the day of each call and search at start', () => {
    const db = seeded();
    const r = report(db);
    const saved = usageOverview(db).meko.savings;
    // Credited: the found context_search, the found knowledgebase_search (no price) and s1's seed.
    expect(saved).toMatchObject({ searches: 2, seeds: 1, unpricedSearches: 1, unpricedSeeds: 0 });
    expect(r.totals.meko).toEqual({
      calls: 6, lookups: 4, found: 2, empty: 1, weak: 1, errors: 1, saves: 1, foundTokens: 1250,
      byTool: { context_search: 1, memory_search: 1, memory_add: 1, other: 1, memory_get_by_id: 1, knowledgebase_search: 1 },
      savings: {
        searches: 2, seeds: 1, tokens: Math.round(saved.tokens), usdCents: Math.round(saved.usd! * 100), unpricedSearches: 1, unpricedSeeds: 0,
      },
      starts: { found: 1, empty: 1, error: 0, late: 1, ms: 6500 },
      // Each call sent 40 characters; seeds send nothing.
      sentTokens: 60,
      sentByTool: { context_search: 10, memory_search: 10, memory_add: 10, other: 10, memory_get_by_id: 10, knowledgebase_search: 10 },
      gaps: { dismissed: 2, taught: 1 },
    });
    expect(r.totals.meko.savings.usdCents).toBeGreaterThan(0);
    expect(r.totals.mekoFirstTurn).toEqual({ sessions: 1, startSearches: 1, searchedTokens: 1000, seedTokens: 200 });

    const day = (d: string) => r.days.find((x) => x.day === d)!;
    expect(day('2026-09-27').meko).toMatchObject({
      calls: 4, lookups: 2, found: 1, empty: 1, weak: 1, errors: 0, saves: 1, foundTokens: 1000,
      byTool: { context_search: 1, memory_search: 1, memory_add: 1, other: 1 },
      savings: { searches: 1, seeds: 1, unpricedSearches: 0 },
      starts: { found: 1, empty: 0, error: 0, late: 0, ms: 1200 },
      gaps: { dismissed: 2, taught: 0 },
    });
    expect(day('2026-09-28').meko).toEqual({
      calls: 1, lookups: 1, found: 0, empty: 0, weak: 0, errors: 1, saves: 0, foundTokens: 0, byTool: { memory_get_by_id: 1 },
      savings: { searches: 0, seeds: 0, tokens: 0, usdCents: 0, unpricedSearches: 0, unpricedSeeds: 0 },
      starts: { found: 0, empty: 0, error: 0, late: 0, ms: 0 },
      sentTokens: 10, sentByTool: { memory_get_by_id: 10 },
      gaps: { dismissed: 0, taught: 1 },
    });
    expect(day('2026-09-26').meko).toMatchObject({ calls: 0, byTool: {}, starts: { late: 1, ms: 5000 } });
    // A day with no Meko activity leaves it out.
    expect(r.days.filter((d) => d.meko).map((d) => d.day)).toEqual(['2026-09-26', '2026-09-27', '2026-09-28']);
    // The 30-day report reaches the search at start on the 20th; the January call is in no day.
    const first = report(db, FIRST_REPORT_DAYS);
    expect(first.days.find((d) => d.day === '2026-09-20')!.meko).toMatchObject({ calls: 0, starts: { empty: 1, ms: 300 } });
    expect(first.days.reduce((n, d) => n + (d.meko?.calls ?? 0), 0)).toBe(5);
  });

  it("counts learn steps, AgentK's own Meko calls, transcript captures and Ask AgentK's asks, on the day each began", () => {
    const db = seeded();
    const learn = (id: string, s: string, trigger: string, path: string, start: string, end: string, outcome: string, c: number[]) =>
      run(db, `INSERT INTO meko_learns(id, session_id, trigger, path, started_at, ended_at, outcome, candidates, saves, updates, skips,
               questions, taught, dismissed, lookups, errors, sent_chars) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
      id, s, trigger, path, at(start), at(end), outcome, ...c);
    const usage = (s: string, id: string, agent: string | null, model: string, iso: string, u: number[]) =>
      run(db, `INSERT INTO usage_messages(session_id, message_id, agent, model, at, input, output, cache_read, cache_write) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
        s, id, agent, model, at(iso), ...u);
    // Through the mod at Merge & Close: the fork and the duplicate check, recorded by learn.ts. It was
    // offered 3 questions Meko couldn't answer, taught 2 and dismissed 1.
    learn('L1', 's1', 'merge', 'mod', '2026-09-27T17:00:00Z', '2026-09-27T17:01:00Z', 'done', [4, 2, 1, 1, 3, 2, 1, 4, 0, 400]);
    usage('s1', 'learn:L1:extract', 'agentk:learn', 'claude-opus-5-5', '2026-09-27T17:00:30Z', [12, 300, 150000, 0]);
    usage('s1', 'learn:L1:dedup', 'agentk:learn', 'claude-haiku-4-5-20251001', '2026-09-27T17:00:50Z', [800, 100, 0, 0]);
    // As a typed turn when asked for: the session's responses while it ran.
    learn('L2', 's1', 'manual', 'turn', '2026-09-28T03:00:00Z', '2026-09-28T03:05:00Z', 'done', [1, 1, 0, 0, 0, 0, 0, 1, 0, 80]);
    usage('s1', 'm6', null, 'claude-opus-5-5', '2026-09-28T03:02:00Z', [5, 50, 2000, 0]);
    // Skipped at a Discard: Claude was busy.
    learn('L3', 's2', 'discard', 'none', '2026-09-28T04:00:00Z', '2026-09-28T04:00:00Z', 'skipped', [0, 0, 0, 0, 0, 0, 0, 0, 0, 0]);
    const own = (iso: string, tool: string, chars: number, ok: number, ms: number) =>
      run(db, 'INSERT INTO meko_agentk_calls(at, tool, input_chars, result_chars, ok, ms, learn_id) VALUES (?, ?, ?, 10, ?, ?, NULL)', at(iso), tool, chars, ok, ms);
    own('2026-09-27T17:00:40Z', 'context_search', 120, 1, 200);
    own('2026-09-27T17:00:55Z', 'memory_add', 200, 0, 900);
    own('2026-09-28T05:00:00Z', 'Weird.Tool', 40, 1, 100);
    run(db, 'INSERT INTO meko_capture_runs(at, ok, ms) VALUES (?, 1, 300), (?, 0, 100)', at('2026-09-27T18:00:00Z'), at('2026-09-28T06:00:00Z'));
    // Ask AgentK: two asks one day (one timed out), one the next.
    run(db, 'INSERT INTO agentk_asks(at, ok, actions, ms) VALUES (?, 1, 2, 2100), (?, 0, 0, 45000), (?, 1, 0, 1900)',
      at('2026-09-27T19:00:00Z'), at('2026-09-27T19:05:00Z'), at('2026-09-28T07:00:00Z'));

    const r = report(db);
    const tokens = (input: number, output: number, cacheRead: number) => ({ input, output, cacheRead, cacheWrite: 0, messages: 1 });
    expect(r.totals.meko.learn).toEqual({
      runs: 3, byTrigger: { merge: 1, manual: 1, discard: 1 }, byPath: { mod: 1, turn: 1, none: 1 },
      byOutcome: { done: 2, failed: 0, interrupted: 0, skipped: 1 },
      candidates: 5, saves: 3, updates: 1, skips: 1, empty: 0, taught: 2, dismissed: 1, lookups: 5, errors: 0, sentTokens: 120,
      tokens: { extract: tokens(12, 300, 150000), dedup: tokens(800, 100, 0), turn: tokens(5, 50, 2000) },
      // Opus 5.5 for the fork and the turn, Haiku 4.5 for the check: $0.0388.
      usdCents: 4,
    });
    expect(r.totals.meko.agentk).toEqual({ calls: 3, errors: 1, sentTokens: 90, ms: 1200, byTool: { context_search: 1, memory_add: 1, other: 1 } });
    expect(r.totals.meko.capture).toEqual({ runs: 2, failures: 1, ms: 400 });
    const day = (d: string) => r.days.find((x) => x.day === d)!;
    expect(day('2026-09-27').meko?.learn).toMatchObject({ runs: 1, saves: 2, updates: 1 });
    expect(day('2026-09-28').meko?.learn).toMatchObject({ runs: 2, byOutcome: { done: 1, skipped: 1 } });
    expect(day('2026-09-28').meko?.agentk).toMatchObject({ calls: 1, byTool: { other: 1 } });
    expect(day('2026-09-26').meko?.learn).toBeUndefined();
    expect(day('2026-09-27').askAgentk).toEqual({ asks: 2, failed: 1, actions: 2 });
    expect(day('2026-09-28').askAgentk).toEqual({ asks: 1, failed: 0, actions: 0 });
    expect(day('2026-09-26').askAgentk).toBeUndefined();
    expect(validate(r, SCHEMA)).toEqual([]);
  });

  it("names AgentK's nine most used tools, and the rest as other", () => {
    const db = openDb(':memory:');
    for (let i = 0; i < 12; i++) {
      for (let n = 0; n <= i; n++) {
        run(db, 'INSERT INTO meko_agentk_calls(at, tool, input_chars, result_chars, ok, ms) VALUES (?, ?, 4, 0, 1, 1)', NOW - 1000, `tool_${String.fromCharCode(97 + i)}`);
      }
    }
    const byTool = report(db).totals.meko.agentk!.byTool;
    expect(Object.keys(byTool)).toHaveLength(10);
    expect(byTool.tool_l).toBe(12);
    expect(byTool.tool_d).toBe(4);
    expect(byTool.other).toBe(1 + 2 + 3);
  });

  it('counts no Meko use without any', () => {
    const r = report(openDb(':memory:'));
    expect(r.days.some((d) => d.meko)).toBe(false);
    expect(r.totals.meko).toEqual({
      calls: 0, lookups: 0, found: 0, empty: 0, weak: 0, errors: 0, saves: 0, foundTokens: 0, byTool: {},
      savings: { searches: 0, seeds: 0, tokens: 0, usdCents: 0, unpricedSearches: 0, unpricedSeeds: 0 },
      starts: { found: 0, empty: 0, error: 0, late: 0, ms: 0 },
      sentTokens: 0, sentByTool: {},
    });
    expect(r.totals.mekoFirstTurn).toEqual({ sessions: 0, startSearches: 0, searchedTokens: 0, seedTokens: 0 });
  });

  it('never carries a name, path or URL', () => {
    const text = JSON.stringify(report(seeded(), FIRST_REPORT_DAYS)).toLowerCase();
    const mine = [userInfo().username, hostname()].filter((s) => s.length >= 4);
    for (const s of [...SECRETS, ...mine]) expect(text, s).not.toContain(s.toLowerCase());
  });

  it('matches docs/call-home-report.schema.json', () => {
    expect([...keywords(SCHEMA)].filter((k) => !KEYWORDS.has(k))).toEqual([]);
    for (const days of [REPORT_DAYS, FIRST_REPORT_DAYS]) {
      expect(validate(report(seeded(), days), SCHEMA)).toEqual([]);
      expect(validate(report(openDb(':memory:'), days), SCHEMA)).toEqual([]);
    }
    const r = report(seeded()) as unknown as Record<string, unknown> & CallHomeReport;
    expect(validate({ ...r, extra: 1 }, SCHEMA)).toEqual(['$: unexpected extra']);
    const { worktrees, ...totals } = r.totals;
    expect(validate({ ...r, totals: { ...totals, worktree: worktrees } }, SCHEMA)).toEqual(['$.totals: missing worktrees', '$.totals: unexpected worktree']);
    expect(validate({ ...r, install: { ...r.install, platform: 'Alice’s Mac' } }, SCHEMA)).toHaveLength(1);
    expect(validate({ ...r, totals: { ...r.totals, meko: { ...r.totals.meko, query: 1 } } }, SCHEMA)).toEqual(['$.totals.meko: unexpected query']);
    expect(validate({ ...r, totals: { ...r.totals, meko: { ...r.totals.meko, byTool: { 'alice/zeta': 1 } } } }, SCHEMA)).toHaveLength(1);
    // A report from before Discard ran learn steps has no byTrigger.discard, and still checks.
    const learn = { runs: 0, byTrigger: { merge: 0, manual: 0 }, byPath: { mod: 0, turn: 0, none: 0 }, byOutcome: { done: 0, failed: 0, interrupted: 0, skipped: 0 },
      candidates: 0, saves: 0, updates: 0, skips: 0, empty: 0, lookups: 0, errors: 0, sentTokens: 0,
      tokens: Object.fromEntries(['extract', 'dedup', 'turn'].map((p) => [p, { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, messages: 0 }])), usdCents: 0 };
    expect(validate({ ...r, totals: { ...r.totals, meko: { ...r.totals.meko, learn } } }, SCHEMA)).toEqual([]);
    // The schema names exactly the tools the report does.
    const tools = (SCHEMA.$defs as Record<string, Schema>).toolCounts.propertyNames as { enum: string[] };
    expect([...tools.enum].sort()).toEqual([...REPORTED_TOOLS, 'other'].sort());
  });

  it('fits the collector\'s 64 KB in 7 days with every list full', () => {
    const r = report(seeded(), REPORT_DAYS);
    const big = 123_456_789_012;
    const meko = {
      ...r.totals.meko, calls: big, lookups: big, found: big, empty: big, errors: big, saves: big, foundTokens: big,
      byTool: Object.fromEntries([...REPORTED_TOOLS, 'other'].map((t) => [t, big])),
      savings: { searches: big, seeds: big, tokens: big, usdCents: big, unpricedSearches: big, unpricedSeeds: big },
      starts: { found: big, empty: big, error: big, late: big, ms: big },
      sentTokens: big,
      sentByTool: Object.fromEntries([...REPORTED_TOOLS, 'other'].map((t) => [t, big])),
      learn: {
        runs: big, byTrigger: { merge: big, manual: big, discard: big }, byPath: { mod: big, turn: big, none: big },
        byOutcome: { done: big, failed: big, interrupted: big, skipped: big },
        candidates: big, saves: big, updates: big, skips: big, empty: big, taught: big, dismissed: big, lookups: big, errors: big, sentTokens: big,
        tokens: Object.fromEntries(['extract', 'dedup', 'turn'].map((p) => [p, { input: big, output: big, cacheRead: big, cacheWrite: big, messages: big }])) as never,
        usdCents: big,
      },
      agentk: {
        calls: big, errors: big, sentTokens: big, ms: big,
        byTool: Object.fromEntries(Array.from({ length: 10 }, (_, i) => [`meko_tool_name_${String.fromCharCode(97 + i)}`, big])),
      },
      capture: { runs: big, failures: big, ms: big },
    };
    r.totals.fileTypes = Array.from({ length: 26 }, (_, i) => ({ ext: `ext${i}`, files: big, additions: big, deletions: big }));
    r.totals.meko = meko;
    for (const d of r.days) {
      d.fileTypes = Array.from({ length: 26 }, (_, i) => ({ ext: `ext${i}`, files: big, additions: big, deletions: big }));
      d.tokensByModel = Array.from({ length: 20 }, (_, i) => ({ model: `claude-model-${i}-5`, input: big, output: big, cacheRead: big, cacheWrite: big, messages: big }));
      d.meko = meko;
    }
    expect(validate(r, SCHEMA)).toEqual([]);
    expect(JSON.stringify(r).length).toBeLessThan(MAX_REPORT_BYTES);
  });
});

describe('what the report says about the install', () => {
  const probe = (answers: Record<string, string | Error>): Probe => async (cmd) => {
    const a = answers[cmd];
    if (a instanceof Error || a === undefined) throw a ?? new Error('not found');
    return a;
  };

  it('reads the Claude Code version, and Meko and access from the config', async () => {
    const cfg = structuredClone(DEFAULT_CONFIG);
    cfg.server.bind = '127.0.0.1';
    cfg.meko.apiKey = 'key';
    cfg.update.channel = 'prerelease';
    const facts = await installFacts({ config: cfg, version: '0.20260928.0', firstDay: '2026-09-28', probe: probe({ claude: '2.4.1 (Claude Code)\n' }) });
    expect(facts).toMatchObject({
      version: '0.20260928.0', channel: 'prerelease', platform: process.platform, arch: process.arch, node: process.versions.node,
      claudeCode: '2.4.1', access: 'local', meko: { enabled: true, keySource: 'config', sharing: 'api' }, firstDay: '2026-09-28',
    });
    // A self-hosted Meko with no web app or API URL set: AgentK can't share.
    cfg.meko.url = 'https://meko.example.com/mcp';
    const noApi = await installFacts({ config: cfg, version: '0.20260928.0', firstDay: '2026-09-28', probe: probe({}) });
    expect(noApi.meko).toEqual({ enabled: true, keySource: 'config', sharing: 'none' });
  });

  it('takes only versions the collector keeps: numbers, one letter after them, and a lane or number as prerelease', () => {
    for (const v of ['0.20261001.1', 'v0.20261001.1', '0.0.0-dev', '1.2.3-beta.2', '22.18.0', '3.2a', '2.4.1', '1.0.0+20261001']) expect(VERSION_RE.test(v), v).toBe(true);
    for (const v of ['1.2.3-alice', '1.2.3-foo.1', '3.2-next-x', 'next-3.4', '1.2.3.4.5', '']) expect(VERSION_RE.test(v), v).toBe(false);
  });

  it('cuts a version the collector would not keep to its leading numbers', () => {
    expect(reportVersion('0.20261001.1')).toBe('0.20261001.1');
    expect(reportVersion('24.0.0-nightly20250101abc')).toBe('24.0.0');
    expect(reportVersion('0.20261001.1-3-gabc1234')).toBe('0.20261001.1');
    expect(reportVersion('1.2.3.4.5')).toBe('1.2.3.4');
    expect(reportVersion('not a version!')).toBe('0.0.0');
    for (const v of ['24.0.0-nightly20250101abc', '0.20261001.1-3-gabc1234', '1.2.3.4.5', 'x']) expect(VERSION_RE.test(reportVersion(v)), v).toBe(true);
  });

  it('leaves out versions it cannot read, and Meko details when Meko is off', async () => {
    const cfg = structuredClone(DEFAULT_CONFIG);
    const facts = await installFacts({ config: cfg, version: 'not a version!', firstDay: '2026-09-28', probe: probe({ claude: 'no digits here' }) });
    expect(facts).not.toHaveProperty('tmux');
    expect(facts).toMatchObject({ version: '0.0.0', claudeCode: '', access: 'remote', meko: { enabled: false, keySource: 'none', sharing: 'none' } });
  });

  it('reads the collector URL a release build carries', () => {
    const dir = tempDir('akl-baked-');
    expect(bakedCallHomeUrl(dir)).toBe('');
    writeFileSync(join(dir, 'CALL_HOME_URL'), 'https://collector.test\n');
    expect(bakedCallHomeUrl(dir)).toBe('https://collector.test');
    expect(bakedCallHomeUrl(undefined)).toBe('');
  });
});

describe('sending the report', () => {
  afterEach(() => vi.useRealTimers());

  function setup(o: { url?: string; baked?: string; status?: number; maxReportBytes?: number } = {}) {
    const db = seeded();
    const cfg: Config = structuredClone(DEFAULT_CONFIG);
    cfg.callHome.url = o.url ?? 'https://collector.test/v1/report';
    const clock = { now: NOW };
    const calls: { url: string; init: RequestInit }[] = [];
    const answer = { status: o.status ?? 204, error: null as Error | null };
    const fetchImpl = (async (url: string | URL | Request, init?: RequestInit) => {
      calls.push({ url: String(url), init: init ?? {} });
      if (answer.error) throw answer.error;
      return new Response(null, { status: answer.status });
    }) as typeof fetch;
    const logs: string[] = [];
    const log: Logger = { info: (m) => logs.push(`info ${m}`), warn: (m) => logs.push(`warn ${m}`), error: (m) => logs.push(`error ${m}`) };
    const service = () => new CallHomeService({
      db, config: () => cfg, version: '0.20260928.0', log,
      fetch: fetchImpl, now: () => clock.now, bakedUrl: () => o.baked ?? '', probe: async () => { throw new Error('no probes in tests'); },
      maxReportBytes: o.maxReportBytes,
    });
    const body = (i: number) => JSON.parse(String(calls[i].init.body)) as CallHomeReport;
    return { db, cfg, clock, calls, answer, logs, service, body };
  }

  it('sends a first report of 30 days, then 7 days every hour for a day, then once a day', async () => {
    const t = setup();
    const svc = t.service();
    expect(await svc.tick()).toBe(true);
    expect(t.calls).toHaveLength(1);
    expect(t.calls[0].url).toBe('https://collector.test/v1/report');
    expect(t.calls[0].init.method).toBe('POST');
    expect(t.calls[0].init.headers).toMatchObject({ 'Content-Type': 'application/json', 'User-Agent': 'agent-k-local/0.20260928.0' });
    const first = t.body(0);
    expect(first.days).toHaveLength(FIRST_REPORT_DAYS);
    expect(first.install.firstDay).toBe('2026-09-28');
    expect(validate(first, SCHEMA)).toEqual([]);
    expect(kvGet(t.db, 'call_home_first_sent')).toBe(String(NOW));

    // The first day: a report at every hourly check, not between them.
    t.clock.now = NOW + FIRST_DAY_SEND_AFTER_MS - 60_000;
    expect(await svc.tick()).toBe(false);
    t.clock.now = NOW + CHECK_EVERY_MS;
    expect(await svc.tick()).toBe(true);
    const second = t.body(1);
    expect(second.days).toHaveLength(REPORT_DAYS);
    expect(second.installId).toBe(first.installId);
    expect(second.install.firstDay).toBe('2026-09-28');
    while (t.clock.now + CHECK_EVERY_MS < NOW + FIRST_DAY_MS) {
      t.clock.now += CHECK_EVERY_MS;
      expect(await svc.tick()).toBe(true);
    }
    expect(t.calls).toHaveLength(24);
    // The first report's time stays put.
    expect(kvGet(t.db, 'call_home_first_sent')).toBe(String(NOW));

    // Then once a day.
    const lastHourly = t.clock.now;
    t.clock.now += CHECK_EVERY_MS;
    expect(t.clock.now - NOW).toBeGreaterThanOrEqual(FIRST_DAY_MS);
    expect(await svc.tick()).toBe(false);
    t.clock.now = lastHourly + SEND_AFTER_MS - 60_000;
    expect(await svc.tick()).toBe(false);
    t.clock.now += 60_000;
    expect(await svc.tick()).toBe(true);
    expect(t.calls).toHaveLength(25);
  });

  it("stamps a report with its check's time, however long the send takes", async () => {
    const t = setup();
    const svc = new CallHomeService({
      db: t.db, config: () => t.cfg, version: '0.20260928.0',
      log: { info() {}, warn() {}, error() {} }, now: () => t.clock.now, probe: async () => '',
      fetch: (async () => {
        t.clock.now += 6 * 60_000;
        return new Response(null, { status: 204 });
      }) as typeof fetch,
    });
    expect(await svc.tick()).toBe(true);
    expect(kvGet(t.db, 'call_home_first_sent')).toBe(String(NOW));
    expect(kvGet(t.db, 'call_home_last_sent')).toBe(String(NOW));
    t.clock.now = NOW + CHECK_EVERY_MS;
    expect(await svc.tick()).toBe(true);
    expect(kvGet(t.db, 'call_home_last_sent')).toBe(String(NOW + CHECK_EVERY_MS));
  });

  it('reports once a day from an install that reported before the first report was kept', async () => {
    const t = setup();
    kvSet(t.db, 'call_home_last_sent', String(NOW - CHECK_EVERY_MS));
    const svc = t.service();
    expect(await svc.tick()).toBe(false);
    t.clock.now += SEND_AFTER_MS - CHECK_EVERY_MS;
    expect(await svc.tick()).toBe(true);
    expect(kvGet(t.db, 'call_home_first_sent')).toBeNull();
  });

  it('drops the oldest days of a report too big for the collector until it fits', async () => {
    const size = (days: number) => JSON.stringify(report(seeded(), days)).length;
    const max = Math.round((size(REPORT_DAYS) + size(FIRST_REPORT_DAYS)) / 2);
    const t = setup({ maxReportBytes: max });
    expect(await t.service().tick()).toBe(true);
    const sent = String(t.calls[0].init.body);
    const days = t.body(0).days;
    const all = report(seeded(), FIRST_REPORT_DAYS).days;
    expect(Buffer.byteLength(sent)).toBeLessThanOrEqual(max);
    expect(days.length).toBeGreaterThan(REPORT_DAYS);
    expect(days.length).toBeLessThan(FIRST_REPORT_DAYS);
    // The newest days, as many as fit: one more wouldn't have.
    expect(days).toEqual(all.slice(-days.length));
    expect(Buffer.byteLength(JSON.stringify({ ...t.body(0), days: all.slice(-days.length - 1) }))).toBeGreaterThan(max);

    // A later report too: a byte over drops its oldest day.
    const sendLater = async (maxReportBytes?: number) => {
      const s = setup({ maxReportBytes });
      kvSet(s.db, 'call_home_last_sent', String(NOW - SEND_AFTER_MS));
      expect(await s.service().tick()).toBe(true);
      return s;
    };
    const full = await sendLater();
    expect(full.body(0).days).toHaveLength(REPORT_DAYS);
    const later = await sendLater(Buffer.byteLength(String(full.calls[0].init.body)) - 1);
    expect(later.body(0).days).toEqual(full.body(0).days.slice(1));
  });

  it("doesn't send again after a restart", async () => {
    const t = setup();
    await t.service().tick();
    t.clock.now += FIRST_DAY_SEND_AFTER_MS - 60_000;
    expect(await t.service().tick()).toBe(false);
    // Past the first day, a restart sends nothing until a day after the last report.
    t.clock.now = NOW + FIRST_DAY_MS;
    expect(await t.service().tick()).toBe(true);
    t.clock.now += CHECK_EVERY_MS;
    expect(await t.service().tick()).toBe(false);
    expect(t.calls).toHaveLength(2);
  });

  it('sends nothing, and builds nothing, while turned off', async () => {
    const t = setup();
    t.cfg.callHome.enabled = false;
    expect(await t.service().tick()).toBe(false);
    expect(t.calls).toHaveLength(0);
    expect(kvGet(t.db, 'call_home_install_id')).toBeNull();
    // Turned back on: sent at the next check, with no restart.
    t.cfg.callHome.enabled = true;
    expect(await t.service().tick()).toBe(true);
  });

  it("sends nothing without a collector URL, and uses a release build's URL when config.toml has none", async () => {
    expect(await setup({ url: '' }).service().tick()).toBe(false);
    expect(await setup({ url: 'not a url' }).service().tick()).toBe(false);
    expect(await setup({ url: 'ftp://collector.test' }).service().tick()).toBe(false);

    // The URL is the collector's full report URL, used as given.
    const baked = setup({ url: '', baked: 'https://baked.test/v1/report' });
    expect(await baked.service().tick()).toBe(true);
    expect(baked.calls[0].url).toBe('https://baked.test/v1/report');

    const both = setup({ url: 'https://mine.test/collect', baked: 'https://baked.test/v1/report' });
    await both.service().tick();
    expect(both.calls[0].url).toBe('https://mine.test/collect');
  });

  it('tries again every hour after a failure, logging it once', async () => {
    const t = setup({ status: 500 });
    const svc = t.service();
    expect(await svc.tick()).toBe(false);
    t.answer.status = 204;
    t.answer.error = new Error('ECONNREFUSED');
    t.clock.now += CHECK_EVERY_MS;
    expect(await svc.tick()).toBe(false);
    expect(t.logs).toEqual(['warn sending usage stats failed; trying again every hour']);
    expect(kvGet(t.db, 'call_home_last_sent')).toBeNull();

    t.answer.error = null;
    t.clock.now += CHECK_EVERY_MS;
    expect(await svc.tick()).toBe(true);
    expect(t.logs.at(-1)).toBe('info usage stats sent');
    // Still the first report: nothing had been received.
    expect(t.body(2).days).toHaveLength(FIRST_REPORT_DAYS);
  });

  it('runs one send at a time', async () => {
    const t = setup();
    const svc = t.service();
    const [a, b] = await Promise.all([svc.tick(), svc.tick()]);
    expect([a, b]).toEqual([true, true]);
    expect(t.calls).toHaveLength(1);
  });

  it('checks first after a random delay, sooner for a new install, then every hour', async () => {
    vi.useFakeTimers();
    const t = setup();
    const service = () => new CallHomeService({
      db: t.db, config: () => t.cfg, version: '0.20260928.0',
      log: { info() {}, warn() {}, error() {} }, fetch: (async () => new Response(null, { status: 204 })) as typeof fetch,
      now: () => Date.now(), probe: async () => '', random: () => 0.5,
    });
    vi.setSystemTime(NOW);
    const firstCheck = async ([lo, hi]: [number, number]) => {
      const svc = service();
      const every: number[] = [];
      const tick = vi.spyOn(svc, 'tick');
      svc.start((ms) => void every.push(ms));
      const delay = lo + Math.floor(0.5 * (hi - lo));
      await vi.advanceTimersByTimeAsync(delay - 1);
      expect(tick).not.toHaveBeenCalled();
      await vi.advanceTimersByTimeAsync(1);
      expect(tick).toHaveBeenCalledTimes(1);
      expect(every).toEqual([CHECK_EVERY_MS]);
      await tick.mock.results[0].value;
    };

    // No report yet: the first one goes out within 5 minutes.
    expect(NEW_INSTALL_FIRST_CHECK_MS[1]).toBeLessThanOrEqual(5 * 60 * 1000);
    await firstCheck(NEW_INSTALL_FIRST_CHECK_MS);
    expect(kvGet(t.db, 'call_home_first_sent')).not.toBeNull();
    // Restarted in its first day: still soon, but not before the next report is due, so the
    // hourly checks that follow don't skip one.
    const sent = Number(kvGet(t.db, 'call_home_first_sent'));
    vi.setSystemTime(sent + 50 * 60_000);
    await firstCheck([FIRST_DAY_SEND_AFTER_MS - 50 * 60_000, FIRST_DAY_SEND_AFTER_MS - 50 * 60_000]);
    expect(Number(kvGet(t.db, 'call_home_last_sent'))).toBe(sent + FIRST_DAY_SEND_AFTER_MS);
    vi.setSystemTime(sent + FIRST_DAY_MS - 60_000);
    await firstCheck(NEW_INSTALL_FIRST_CHECK_MS);
    // Past its first day: the usual spread.
    vi.setSystemTime(sent + FIRST_DAY_MS);
    await firstCheck(FIRST_CHECK_MS);

    // Stopped before the first check: it never runs.
    const other = setup();
    const svc2 = other.service();
    const tick2 = vi.spyOn(svc2, 'tick');
    svc2.start(() => undefined);
    svc2.stop();
    await vi.advanceTimersByTimeAsync(FIRST_CHECK_MS[1]);
    expect(tick2).not.toHaveBeenCalled();
  });
});

