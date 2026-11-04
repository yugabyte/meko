import { describe, expect, it } from 'vitest';
import { checkOutcomes, checkReport, checkScore } from '../../src/server/review/schemas.js';
import { strategyFor } from '../../src/server/review/strategies.js';

const passes = strategyFor('sagittarius-3.0').passes;
const pass = (name: string) => passes.find((p) => p.name === name)!;

const finding = { file: './a.go', line: 3, severity: 'major', comment: ' bug ' };

describe("a pass's report", () => {
  it.each([
    [{ findings: [] }, 'report.summary: is required'],
    [{ summary: 'LGTM', findings: [{ ...finding, line: 0 }] }, 'report.findings[0].line: must be at least 1'],
    [{ summary: 'LGTM', findings: [{ ...finding, severity: 'nit' }] }, 'report.findings[0].severity: must be one of "blocker", "major", "minor"'],
    [{ summary: 'LGTM', findings: [{ ...finding, line: '3' }] }, 'report.findings[0].line: must be a whole number'],
  ])('refuses %j, naming %s', (input, error) => {
    expect(checkReport(pass('guide'), input)).toEqual({ error });
  });

  it('cleans paths and comments, and keeps only gotcha credits with an MG id and a quote, with the quote', () => {
    const out = checkReport(pass('diff'), {
      summary: ' LGTM ',
      findings: [{
        ...finding,
        gotchas: [{ id: 'MG-12', quote: 'Retries drop the lease.' }, { id: 'MG-0', quote: 'x' }, { id: 'MG-3', quote: ' ' }, { id: 'MG-12', quote: 'again' }],
      }],
    });
    expect(out).toEqual({
      summary: 'LGTM', findings: [{ file: 'a.go', line: 3, severity: 'major', comment: 'bug', gotchas: ['MG-12'], quotes: { 'MG-12': 'Retries drop the lease.' } }],
    });
  });

  it('needs the dependents ledger, and files every survivor not cleared with a citation', () => {
    expect(checkReport(pass('dependents'), { summary: 'LGTM', findings: [] })).toEqual({ error: 'report.state_ledger: is required' });
    const survivor = (over: Record<string, unknown>) => ({
      survivor: 's', where: 'w', evidence: 'e', used_next: 'u', reconciled_by: 'none found', reconciled: false,
      anchor_file: 'a.go', anchor_line: 7, finding: 'the pid file survives', severity: 'blocker', ...over,
    });
    const out = checkReport(pass('dependents'), {
      state_ledger: [{
        what: 'db', where: 'a.go:7', later_consumers: 'x', evidence: 'x', still_holds: false,
        survivors: [
          survivor({}),
          survivor({ reconciled: true, reconciled_by: 'it is reset at startup', finding: 'uncited clearance' }),
          survivor({ reconciled: true, reconciled_by: 'cleared in main.go:42', finding: 'cited clearance' }),
          survivor({ severity: 'blocker', finding: '' }),
        ],
      }],
      summary: 'LGTM',
      findings: [],
    });
    expect(out).toEqual({
      summary: 'LGTM',
      findings: [
        { file: 'a.go', line: 7, severity: 'blocker', comment: 'the pid file survives', gotchas: [] },
        { file: 'a.go', line: 7, severity: 'blocker', comment: 'uncited clearance', gotchas: [] },
      ],
    });
  });

  it('files a claim that does not hold, and takes one that holds at its word', () => {
    const claim = (over: Record<string, unknown>) => ({
      claim: 'c', where: 'w', asserts: 'a', checked: 'x', contradicted_by: 'b.go:3', holds: false,
      anchor_file: 'b.go', anchor_line: 2, finding: 'the comment says safe', severity: 'major', ...over,
    });
    const out = checkReport(pass('claims'), { claims: [claim({}), claim({ holds: true, finding: 'fine' })], summary: 'LGTM', findings: [] });
    expect(out).toEqual({ summary: 'LGTM', findings: [{ file: 'b.go', line: 2, severity: 'major', comment: 'the comment says safe', gotchas: [] }] });
  });
});

describe("a scorer's verdict and the task's outcomes", () => {
  it.each([
    [{ read: [], refutation: '', verdict: 'maybe', confidence: 50, reason: '' }, 'score.verdict: must be one of "refuted", "confirmed", "unverified"'],
    [{ read: [], refutation: '', verdict: 'confirmed', confidence: 101, reason: '' }, 'score.confidence: must be at most 100'],
    [{ refutation: '', verdict: 'confirmed', confidence: 1, reason: '' }, 'score.read: is required'],
  ])('refuses %j', (input, error) => {
    expect(checkScore(input)).toEqual({ error });
  });

  it('reads outcomes, and refuses an empty list', () => {
    expect(checkOutcomes({ outcomes: [{ key: ' 1:0 ', status: 'declined', note: ' not reachable ' }] })).toEqual([
      { key: '1:0', status: 'declined', note: 'not reachable' },
    ]);
    expect(checkOutcomes({ outcomes: [] })).toEqual({ error: 'input.outcomes: needs at least 1 item' });
  });
});
