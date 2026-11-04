import { describe, expect, it } from 'vitest';
import { applyScore, countsOf, keepScored, mergeNotes, mergeSweep, refuted, validate } from '../../src/server/review/merge.js';
import type { Candidate, Diff, FileChange, Finding, PassReport } from '../../src/server/review/types.js';

const f = (file: string, line: number, severity: Finding['severity'], comment: string, gotchas: string[] = []): Finding => ({ file, line, severity, comment, gotchas });
const report = (pass: string, findings: Finding[], summary = 'LGTM'): PassReport => ({ pass, round: 1, sweep: 1, summary, findings });
const cand = (over: Partial<Candidate> = {}): Candidate => ({
  finding: f('a.go', 3, 'major', 'bug'), passes: ['diff'], sweep: 1, score: 0, reason: '', verdict: null, refuted: false, scored: false, ...over,
});

describe('merging the passes', () => {
  it('keys on file and line: one candidate per line, corroborated, worst severity and fuller comment kept, gotchas and their quotes unioned', () => {
    const { candidates, added } = mergeSweep([], [
      report('guide', [{ ...f('./a.go', 3, 'minor', 'x', ['MG-1', 'MG-2']), quotes: { 'MG-1': 'one', 'MG-2': 'two' } }, f('a.go', 3, 'major', 'again from guide')]),
      report('diff', [{ ...f('a.go', 3, 'blocker', 'the longest comment of all', ['MG-2', 'MG-3']), quotes: { 'MG-2': 'two again', 'MG-3': 'three' } }]),
      report('claims', [f('b.go', 1, 'minor', 'other')]),
    ], 1);
    expect(added).toBe(2);
    expect(candidates).toHaveLength(2);
    expect(candidates[0]).toMatchObject({
      passes: ['guide', 'diff'],
      finding: {
        file: 'a.go', line: 3, severity: 'blocker', comment: 'the longest comment of all', gotchas: ['MG-1', 'MG-2', 'MG-3'],
        quotes: { 'MG-1': 'one', 'MG-2': 'two', 'MG-3': 'three' },
      },
    });
  });

  // TestMergeSweepDoesNotCountAnEchoAsNew
  it("doesn't let a later sweep's echo change an earlier candidate or count a near one as new", () => {
    const earlier = mergeSweep([], [report('diff', [f('a.go', 10, 'minor', 'x')])], 1).candidates;
    const { candidates, added } = mergeSweep(earlier, [report('claims', [
      f('a.go', 10, 'blocker', 'the same thing at greater length'),
      f('a.go', 12, 'major', 'near'),
      f('a.go', 50, 'major', 'far'),
      f('b.go', 10, 'major', 'other file'),
    ])], 2);
    expect(added).toBe(2);
    expect(candidates).toHaveLength(4);
    expect(candidates[0]).toMatchObject({ passes: ['diff'], finding: { severity: 'minor', comment: 'x' } });
    expect(candidates.map((c) => c.sweep)).toEqual([1, 2, 2, 2]);
  });
});

describe('scoring', () => {
  it.each([
    ['refuted', 'the guard at a.go:9', ['a.go:1-20'], true],
    ['refuted', '', ['a.go:1-20'], false],
    ['refuted', 'the guard at a.go:9', [], false],
    ['confirmed', 'x', ['a.go'], false],
  ])('%s with refutation %j and read %j drops it: %s', (verdict, refutation, read, want) => {
    expect(refuted(verdict, refutation, read)).toBe(want);
  });

  it('keeps confirmed, unverified at 50 or above and unscored candidates, drops refuted ones, strongest first', () => {
    const scored = (verdict: 'refuted' | 'confirmed' | 'unverified', confidence: number, comment: string) => {
      const c = cand({ finding: f('a.go', 1, 'major', comment) });
      applyScore(c, { read: ['a.go:1-9'], refutation: verdict === 'refuted' ? 'a.go:4 guards it' : '', verdict, confidence, reason: ' r ' });
      return c;
    };
    const kept = keepScored([
      scored('confirmed', 10, 'confirmed low'),
      scored('unverified', 49, 'unverified below'),
      scored('unverified', 50, 'unverified at'),
      scored('refuted', 90, 'refuted high'),
      cand({ finding: f('a.go', 1, 'minor', 'unscored') }),
      scored('confirmed', 80, 'confirmed high'),
    ]);
    expect(kept.map((c) => c.finding.comment)).toEqual(['confirmed high', 'unverified at', 'confirmed low', 'unscored']);
    expect(kept[0].reason).toBe('r');
  });
});

describe('validating against the diff', () => {
  const file = (path: string, anchors: number[]): FileChange => ({
    path, oldPath: null, status: 'modified', additions: 1, deletions: 0, patch: '', withheld: '', anchors, baseRanges: [], symbols: [],
    patchPath: null, deleted: false,
  });
  const diff: Diff = { files: [file('a.go', [10, 11, 12]), file('b.go', [1])], patch: '', inline: true, mergeBase: { ref: 'origin/main', sha: 'x' } };

  // TestValidateDropsFindingsGitHubWouldReject, TestValidateCapKeepsWorstFirst
  it('withholds what has no place on the diff, worst first up to the cap', () => {
    const { post, withheld } = validate([
      cand({ finding: f('a.go', 10, 'minor', 'minor one') }),
      cand({ finding: f('a.go', 11, 'blocker', 'blocker one') }),
      cand({ finding: f('a.go', 12, 'major', '  ') }),
      cand({ finding: f('c.go', 1, 'major', 'not changed') }),
      cand({ finding: f('a.go', 99, 'major', 'outside') }),
      cand({ finding: f('b.go', 1, 'major', 'major one') }),
    ], diff, 2);
    expect(post.map((c) => c.finding.comment)).toEqual(['blocker one', 'major one']);
    expect(withheld.map((w) => [w.candidate.finding.comment.trim(), w.reason])).toEqual([
      ['', 'empty comment'],
      ['not changed', 'file is not changed by this change'],
      ['outside', 'line is outside the diff'],
      ['minor one', 'over the comment limit'],
    ]);
    expect(countsOf(post)).toEqual({ blocker: 1, major: 1, minor: 0 });
  });

  it('cuts a comment over the limit', () => {
    const { post } = validate([cand({ finding: f('b.go', 1, 'major', 'x'.repeat(5000)) })], diff);
    expect(post[0].finding.comment).toBe(`${'x'.repeat(4000)}\n\n[truncated]`);
  });
});

describe("the passes' notes", () => {
  it.each([
    [['LGTM', 'lgtm.', ''], ''],
    [['LGTM', 'The migration has no down step.'], 'The migration has no down step.'],
    [['One\nline.', 'one\nline.', 'Two.'], '- One line.\n- Two.'],
  ])('%j merge to %j', (summaries, want) => {
    expect(mergeNotes(summaries.map((s, i) => report(`p${i}`, [], s)))).toBe(want);
  });
});
