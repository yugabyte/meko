import { appendFileSync, mkdirSync, mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';
import { all, openDb, run } from '../../src/server/db.js';
import { pruneSessionEvents } from '../../src/server/store.js';
import { ingestSessionUsage, parsePushLine, parseUsageLine, projectUsage, sessionUsage, totalTokens, usageByProject, usageBySession, usageOverview } from '../../src/server/usage.js';

function line(id: string, usage: Record<string, number>, opts: { model?: string; at?: string; type?: string } = {}): string {
  return JSON.stringify({
    type: opts.type ?? 'assistant',
    timestamp: opts.at ?? new Date().toISOString(),
    message: { id, model: opts.model ?? 'claude-opus-5-5', usage },
  }) + '\n';
}

function setup() {
  const dir = mkdtempSync(join(tmpdir(), 'akl-usage-'));
  const db = openDb(':memory:');
  const now = Date.now();
  run(db, `INSERT INTO projects(id, name, repo_path, default_branch, created_at) VALUES ('p', 'P', '/r', 'main', ?)`, now);
  for (const [id, desired] of [['s1', 'running'], ['s2', 'archived']]) {
    run(db, `INSERT INTO sessions(id, project_id, slug, name, claude_session_id, branch, base_branch, worktree_path, tmux_name,
             hook_token, desired, status, stage, created_at) VALUES (?, 'p', ?, ?, 'c', 'b', 'main', '/w', 't', ?, ?, 'idle', 'code', ?)`,
      id, id, id.toUpperCase(), `tok-${id}`, desired, now);
  }
  return { dir, db };
}

describe('token usage from transcripts', () => {
  it('parses model responses and ignores everything else', () => {
    expect(parseUsageLine(line('m1', { input_tokens: 2, output_tokens: 5, cache_read_input_tokens: 7, cache_creation_input_tokens: 11 }))).toMatchObject({
      messageId: 'm1', model: 'claude-opus-5-5', input: 2, output: 5, cacheRead: 7, cacheWrite: 11,
    });
    expect(parseUsageLine(line('m2', { input_tokens: 0 }, { model: '<synthetic>' }))).toBeNull();
    expect(parseUsageLine(line('m3', { input_tokens: 1 }, { type: 'user' }))).toBeNull();
    expect(parseUsageLine('{"type":"assistant","usage":')).toBeNull();
  });

  it('dedupes by message id, includes subagents, and reads incrementally', () => {
    const { dir, db } = setup();
    const main = join(dir, 'c.jsonl');
    const u = { input_tokens: 3, output_tokens: 50, cache_read_input_tokens: 1000, cache_creation_input_tokens: 200 };
    // A response spans several lines with the same id and usage.
    writeFileSync(main, line('a', u) + line('a', u) + '{"type":"user"}\n' + line('b', u));
    mkdirSync(join(dir, 'c', 'subagents'), { recursive: true });
    writeFileSync(join(dir, 'c', 'subagents', 'agent-x1.jsonl'), line('sub1', { input_tokens: 1, output_tokens: 9, cache_read_input_tokens: 0, cache_creation_input_tokens: 0 }, { model: 'claude-haiku-4-5' }));

    expect(ingestSessionUsage(db, 's1', main)).toBe(true);
    let s = usageBySession(db).get('s1')!;
    expect(s).toEqual({ input: 7, output: 109, cacheRead: 2000, cacheWrite: 400, messages: 3 });
    expect(ingestSessionUsage(db, 's1', main)).toBe(false); // nothing new
    expect(sessionUsage(db, 's1')).toEqual(s);
    expect(sessionUsage(db, 'nope')).toEqual({ input: 0, output: 0, cacheRead: 0, cacheWrite: 0, messages: 0 });

    // A half-written line isn't counted until it is complete.
    const next = line('c', u);
    appendFileSync(main, next.slice(0, 20));
    ingestSessionUsage(db, 's1', main);
    expect(usageBySession(db).get('s1')!.messages).toBe(3);
    appendFileSync(main, next.slice(20));
    ingestSessionUsage(db, 's1', main);
    s = usageBySession(db).get('s1')!;
    expect(s.messages).toBe(4);
    expect(totalTokens(s)).toBe(3 * 1253 + 10); // a, b, c in the main transcript + one subagent response

    // A rewritten (shorter) transcript is re-read from the start without double counting.
    writeFileSync(main, line('a', u));
    ingestSessionUsage(db, 's1', main);
    expect(usageBySession(db).get('s1')!.messages).toBe(4);
  });

  it('aggregates per project with models, sessions (archived included) and a zero-filled daily series', () => {
    const { dir, db } = setup();
    const now = new Date(2026, 8, 24, 15, 0, 0).getTime();
    const yesterday = new Date(2026, 8, 23, 10, 0, 0).toISOString();
    const u = { input_tokens: 1, output_tokens: 10, cache_read_input_tokens: 100, cache_creation_input_tokens: 0 };
    writeFileSync(join(dir, 's1.jsonl'), line('a', u, { at: new Date(now).toISOString() }) + line('b', u, { at: yesterday, model: 'claude-sonnet-5' }));
    writeFileSync(join(dir, 's2.jsonl'), line('c', u, { at: yesterday }));
    ingestSessionUsage(db, 's1', join(dir, 's1.jsonl'));
    ingestSessionUsage(db, 's2', join(dir, 's2.jsonl'));

    expect(usageByProject(db).get('p')!.messages).toBe(3);
    const pu = projectUsage(db, 'p', 14, now);
    expect(pu.total.output).toBe(30);
    expect(pu.byModel.map((m) => m.model)).toEqual(['claude-opus-5-5', 'claude-sonnet-5']);
    expect(pu.bySession.map((s) => [s.sessionId, s.archived])).toEqual([['s1', false], ['s2', true]]);
    expect(pu.daily).toHaveLength(14);
    expect(pu.daily.at(-1)).toEqual({ day: '2026-09-24', usage: { input: 1, output: 10, cacheRead: 100, cacheWrite: 0, messages: 1 } });
    expect(pu.daily.at(-2)!.usage.messages).toBe(2);
    expect(pu.daily[0].usage.messages).toBe(0);
  });
});

/** A Bash tool result line, as Claude Code writes it to the transcript. */
function toolResult(stdout: string, stderr = '', at = new Date().toISOString(), extra: Record<string, unknown> = {}): string {
  return JSON.stringify({ type: 'user', timestamp: at, message: { role: 'user', content: [] }, toolUseResult: { stdout, stderr, interrupted: false, ...extra } }) + '\n';
}

describe('pushes to the base branch', () => {
  const push = 'To https://github.com/acme/demo.git\n   1dbcae1..7535b4b  HEAD -> main\n';

  it('reads the ref update from git push output, stdout or stderr', () => {
    expect(parsePushLine(toolResult(push), 'main')).toEqual([{ range: '1dbcae1..7535b4b', ref: 'main', at: expect.any(Number) }]);
    expect(parsePushLine(toolResult('', push), 'main')).toHaveLength(1);
    expect(parsePushLine(toolResult('To origin\n   aaaaaaa..bbbbbbb  ak/x -> refs/heads/main\n'), 'main')).toEqual([
      { range: 'aaaaaaa..bbbbbbb', ref: 'main', at: expect.any(Number) },
    ]);
  });

  it('counts output cut short (git push | tail -1) when Claude Code marked it as a push', () => {
    const lastLine = '   5b968f3..4a0f1dc  HEAD -> main';
    expect(parsePushLine(toolResult(lastLine, '', undefined, { gitOperation: { push: { branch: 'main' } } }), 'main')).toEqual([
      { range: '5b968f3..4a0f1dc', ref: 'main', at: expect.any(Number) },
    ]);
    expect(parsePushLine(toolResult(lastLine), 'main')).toEqual([]); // no To line and no marker: not a push
  });

  it('ignores other branches, fetches, forced pushes, new refs and non-tool lines', () => {
    expect(parsePushLine(toolResult('To origin\n   1dbcae1..7535b4b  ak/fix -> ak/fix\n'), 'main')).toEqual([]);
    expect(parsePushLine(toolResult('From github.com:acme/demo\n   1dbcae1..7535b4b  main       -> origin/main\n'), 'main')).toEqual([]);
    expect(parsePushLine(toolResult('To origin\n + 1dbcae1...7535b4b HEAD -> main (forced update)\n'), 'main')).toEqual([]);
    expect(parsePushLine(toolResult('To origin\n * [new branch]      HEAD -> main\n'), 'main')).toEqual([]);
    expect(parsePushLine(line('a', { output_tokens: 1 }), 'main')).toEqual([]);
    expect(parsePushLine('not json -> "toolUseResult"', 'main')).toEqual([]);
  });

  it('records each push once while reading a transcript', () => {
    const { dir, db } = setup();
    const main = join(dir, 'c.jsonl');
    writeFileSync(main, toolResult(push) + line('a', { output_tokens: 3 }));
    expect(ingestSessionUsage(db, 's1', main, 'main')).toBe(true);
    appendFileSync(main, toolResult('To origin\n   7535b4b..9f00c1e  HEAD -> main\n'));
    ingestSessionUsage(db, 's1', main, 'main');
    // Reading from the start again (a migration cleared the offsets) finds nothing new.
    run(db, 'DELETE FROM usage_files');
    ingestSessionUsage(db, 's1', main, 'main');
    const rows = db.prepare('SELECT range, ref, commits FROM pushes WHERE session_id = ? ORDER BY at, range').all('s1');
    expect(rows).toEqual([
      { range: '1dbcae1..7535b4b', ref: 'main', commits: null },
      { range: '7535b4b..9f00c1e', ref: 'main', commits: null },
    ]);
  });
});

describe('usage across projects', () => {
  it('totals tokens, merged PRs and pushed commits per project, deleted ones included', () => {
    const { db } = setup();
    const now = Date.now();
    run(db, `INSERT INTO projects(id, name, repo_path, default_branch, created_at, deleted_at) VALUES ('old', 'Old', '/o', 'main', 0, ?)`, now);
    run(db, `INSERT INTO projects(id, name, repo_path, default_branch, created_at, deleted_at) VALUES ('gone', 'Gone', '/g', 'main', 0, ?)`, now);
    run(db, `INSERT INTO sessions(id, project_id, slug, name, claude_session_id, branch, base_branch, worktree_path, tmux_name,
             hook_token, desired, status, stage, created_at) VALUES ('s3', 'old', 's3', 'S3', 'c', 'b', 'main', '/w', 't3', 'tok-s3', 'archived', 'archived', 'done', ?)`, now);
    const msg = (sid: string, id: string, output: number) =>
      run(db, `INSERT INTO usage_messages(session_id, message_id, model, at, input, output, cache_read, cache_write) VALUES (?, ?, 'm', ?, 1, ?, 0, 0)`, sid, id, now, output);
    msg('s1', 'a', 10);
    msg('s2', 'b', 20);
    msg('s3', 'c', 500);
    const ev = (sid: string, project: string) =>
      run(db, `INSERT INTO session_events(session_id, project_id, kind, text, at) VALUES (?, ?, 'merged', 'PR merged', ?)`, sid, project, now);
    ev('s1', 'p');
    ev('s1', 'p'); // logged twice (a restart mid-cleanup): one merged PR
    ev('s3', 'old');
    run(db, `INSERT INTO pushes(session_id, range, ref, at, commits) VALUES ('s2', 'a..b', 'main', ?, 3), ('s2', 'b..c', 'main', ?, -1)`, now, now);

    const o = usageOverview(db, 14, now);
    expect(o.byProject.map((p) => [p.name, p.repoPath, p.deleted])).toEqual([['P', '/r', false], ['Old', '/o', true]]); // 'Gone' never ran a session
    const p = o.byProject[0];
    expect(p).toMatchObject({ sessions: 2, mergedPrs: 1, directPushes: 2, directCommits: 3 });
    expect(p.usage.output).toBe(30);
    expect(o.byProject[1]).toMatchObject({ sessions: 1, mergedPrs: 1, directPushes: 0, directCommits: 0 });
    expect(o).toMatchObject({ mergedPrs: 2, directPushes: 2, directCommits: 3 });
    expect(o.total.output).toBe(530);
    expect(o.byModel).toEqual([{ model: 'm', usage: expect.objectContaining({ output: 530, messages: 3 }) }]);
    expect(o.daily).toHaveLength(14);
    expect(o.daily.at(-1)!.usage.output).toBe(530);
  });
});

describe('merged PRs', () => {
  it('still count after old events are pruned', () => {
    const { db } = setup();
    const now = Date.now();
    const old = now - 60 * 24 * 3600 * 1000;
    const ev = (sid: string, kind: string, at: number) =>
      run(db, `INSERT INTO session_events(session_id, project_id, kind, text, at) VALUES (?, 'p', ?, 'x', ?)`, sid, kind, at);
    ev('s1', 'merged', old);
    ev('s1', 'stage', old);
    ev('s2', 'stage', now);
    // Merged before merge events were kept: Done with a PR, its event already pruned.
    run(db, `INSERT INTO sessions(id, project_id, slug, name, claude_session_id, branch, base_branch, worktree_path, tmux_name,
             hook_token, desired, status, stage, pr_number, created_at) VALUES ('s3', 'p', 's3', 'S3', 'c', 'b', 'main', '/w', 't3', 'tok-s3', 'archived', 'archived', 'done', 7, ?)`, old);
    pruneSessionEvents(db, now);
    const kinds = all<{ kind: string; at: number }>(db, 'SELECT kind, at FROM session_events ORDER BY id').map((e) => [e.kind, e.at]);
    expect(kinds).toEqual([['merged', old], ['stage', now]]);
    const o = usageOverview(db, 14, now);
    expect(o.byProject[0].mergedPrs).toBe(2);
    expect(o.mergedPrs).toBe(2);
  });
});

describe('token formatting (web)', () => {
  it('abbreviates large counts', async () => {
    const { formatTokens } = await import('../../src/web/util.js');
    expect(formatTokens(950)).toBe('950');
    expect(formatTokens(1000)).toBe('1k');
    expect(formatTokens(12_300)).toBe('12.3k');
    expect(formatTokens(4_560_000)).toBe('4.56M');
    expect(formatTokens(250_000_000)).toBe('250M');
    expect(formatTokens(1_200_000_000)).toBe('1.2B');
  });

  it('formats costs in dollars', async () => {
    const { formatCost } = await import('../../src/web/util.js');
    expect(formatCost(0)).toBe('$0');
    expect(formatCost(0.004)).toBe('<$0.01');
    expect(formatCost(3.064)).toBe('$3.06');
    expect(formatCost(241.4)).toBe('$241');
    expect(formatCost(1234.5)).toBe('$1,235');
    expect(formatCost(-2.75)).toBe('−$2.75');
  });

  it('marks a cost that leaves out unpriced models as a lower bound', async () => {
    const { formatCostOf } = await import('../../src/web/util.js');
    expect(formatCostOf({ usd: 3.06, unpricedMessages: 0 })).toBe('$3.06');
    expect(formatCostOf({ usd: 3.06, unpricedMessages: 4 })).toBe('≥ $3.06');
  });

  it('puts the 1st-turn tokens in words, seed first, saying when AgentK has never seeded a session', async () => {
    const { firstTurnText } = await import('../../src/web/util.js');
    expect(firstTurnText({ sessions: 0, startSearches: 0, searchedTokens: 0, seedTokens: 0 })).toEqual({
      split: null, title: 'No session has finished its first turn yet.',
    });
    // Most sessions are from before AgentK searched at start.
    const old = firstTurnText({ sessions: 50, startSearches: 2, searchedTokens: 12_300, seedTokens: 0 });
    expect(old.split).toBe('Seeded ≈ 0 · Searched ≈ 12.3k');
    expect(old.title).toBe(
      "About 12,300 tokens from Meko in the first turns of 50 sessions (characters ÷ 4): 0 that AgentK's search at session start passed to Claude " +
      "(the seed; it ran in 2 of them, the others are from before it was added or had no task), and 12,300 from Claude's own Meko searches before its first turn ended. " +
      'Only sessions in projects with datapacks count.');
    const none = firstTurnText({ sessions: 1, startSearches: 0, searchedTokens: 900, seedTokens: 0 });
    expect(none.split).toBe('Seeded none yet · Searched ≈ 900');
    expect(none.title).toContain('first turns of 1 session (');
    expect(none.title).toContain("the seed, hasn't run in any of them yet; and 900 from");
    const all = firstTurnText({ sessions: 3, startSearches: 3, searchedTokens: 0, seedTokens: 4_560 });
    expect(all.split).toBe('Seeded ≈ 4.56k · Searched ≈ 0');
    expect(all.title).toContain("4,560 that AgentK's search at session start passed to Claude (the seed), and 0 from");
  });

  it("puts the Meko section's learn-step tiles in words", async () => {
    const { learningText } = await import('../../src/web/util.js');
    const none = { runs: 0, failed: 0, skipped: 0, saves: 0, updates: 0, skips: 0, tokens: 0, usd: null };
    expect(learningText(none)).toEqual({
      learned: { num: '—', line: null, title: 'No learn step has run yet. One runs at Merge & Close and Discard, or when you choose Save learnings.' },
      cost: { num: '—', line: null, title: 'No learn step has run yet. One runs at Merge & Close and Discard, or when you choose Save learnings.' },
    });
    expect(learningText({ ...none, skipped: 2 }).learned.title).toMatch(/Save learnings\. 2 sessions were closed without one, since Claude was gone or busy\.$/);
    const some = learningText({ runs: 3, failed: 1, skipped: 1, saves: 12, updates: 2, skips: 5, tokens: 345_000, usd: 0.42 });
    expect(some.learned).toEqual({
      num: '12',
      line: '2 corrected · 3 steps',
      title: "3 learn steps saved 12 memories to the datapack, corrected 2 and found 5 facts already known; 1 didn't finish. 1 session was closed without one, since Claude was gone or busy.",
    });
    expect(some.cost).toMatchObject({ num: '$0.42', line: '≈ 345k tokens' });
    expect(some.cost.title).toContain('345,000 tokens, at Anthropic API list prices');
    // Without a known price: tokens only.
    expect(learningText({ ...none, runs: 1, saves: 1, tokens: 9_000, usd: null }).cost).toMatchObject({ num: '≈ 9k', line: null });
  });
});
