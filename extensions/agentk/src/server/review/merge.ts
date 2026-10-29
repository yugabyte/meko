/**
 * The review's algorithm, as the agent-k reviewer runs it (`multi.go`, `score.go`, `validate.go`):
 * merging what the passes report into candidates, the refutation gate a scorer has to pass to drop
 * one, which candidates survive scoring, and which of those can be shown on the change's diff.
 */
import { noCounts, SEVERITIES, type SeverityCounts } from '../../shared/codereview.js';
import { anchorsLine } from './diff.js';
import type { Candidate, Diff, Finding, PassReport, ScoreReport } from './types.js';

/**
 * The confidence an unverified candidate needs to survive (REVIEW_MIN_CONFIDENCE). Confirmed ones
 * survive whatever their confidence, and refuted ones go whatever theirs.
 */
export const MIN_CONFIDENCE = 50;

/** A finding's comment is cut here (`maxCommentBytes`): long ones are the model narrating, not reporting. */
export const MAX_COMMENT_BYTES = 4000;

/** How close a later sweep's finding may land to an earlier one and still be the same subject. */
export const NEAR_LINES = 3;

function rank(severity: string): number {
  const i = SEVERITIES.indexOf(severity as (typeof SEVERITIES)[number]);
  return i < 0 ? SEVERITIES.length : i;
}

/** Any of `cands` is in `f`'s file within NEAR_LINES of it. */
export function nearAny(cands: Candidate[], f: Finding): boolean {
  return cands.some((c) => c.finding.file === f.file && Math.abs(c.finding.line - f.line) <= NEAR_LINES);
}

/**
 * Merges one sweep's reports into the candidates the earlier sweeps produced, keyed by file and
 * line, and counts how many it added that are new: not on or near a line an earlier sweep raised.
 *
 * Two passes on one line is corroboration, so the pass is named rather than the finding dropped
 * (once, however many findings it landed there), the worse severity and the fuller comment are
 * kept, and gotcha credits union. A later sweep landing exactly on an earlier candidate's line adds
 * nothing to it, not even its pass name: that pass was shown the earlier finding, so naming it would
 * read to the scorer as an independent reviewer agreeing. A near one is its own candidate (it may be
 * a second bug a line away) but doesn't count as new.
 */
export function mergeSweep(cands: Candidate[], reports: PassReport[], sweep: number): { candidates: Candidate[]; added: number } {
  const key = (file: string, line: number) => `${line}\0${file}`;
  const index = new Map<string, number>();
  cands.forEach((c, i) => index.set(key(c.finding.file, c.finding.line), i));
  const earlier = cands.length;
  const out = cands.map((c) => ({ ...c, finding: { ...c.finding, gotchas: [...c.finding.gotchas] }, passes: [...c.passes] }));
  let added = 0;
  for (const r of reports) {
    for (const raw of r.findings) {
      const f: Finding = { ...raw, file: raw.file.trim().replace(/^(\.\/)+/, ''), gotchas: [...raw.gotchas] };
      const k = key(f.file, f.line);
      const i = index.get(k);
      if (i === undefined) {
        index.set(k, out.length);
        out.push({ finding: f, passes: [r.pass], sweep, score: 0, reason: '', verdict: null, refuted: false, scored: false });
        if (!nearAny(out.slice(0, earlier), f)) added++;
        continue;
      }
      const c = out[i];
      if (c.sweep < sweep) continue;
      if (!c.passes.includes(r.pass)) c.passes.push(r.pass);
      for (const id of f.gotchas) if (!c.finding.gotchas.includes(id)) c.finding.gotchas.push(id);
      if (rank(f.severity) < rank(c.finding.severity)) c.finding.severity = f.severity;
      if (f.comment.length > c.finding.comment.length) c.finding.comment = f.comment;
    }
  }
  return { candidates: out, added };
}

/**
 * A verdict may drop a finding only with a stated fact and a list of what was opened to find it.
 * "refuted" alone is a word the model can produce without doing anything; either part missing and
 * the verdict counts as unverified, which keeps the finding.
 */
export function refuted(verdict: string, refutation: string, read: string[]): boolean {
  return verdict === 'refuted' && refutation.trim() !== '' && read.length > 0;
}

/** Records a scorer's answer on its candidate. */
export function applyScore(c: Candidate, s: ScoreReport): void {
  c.score = s.confidence;
  c.reason = s.reason.trim();
  c.verdict = s.verdict;
  c.refuted = refuted(s.verdict, s.refutation, s.read);
  c.scored = true;
}

/**
 * The candidates that survive scoring, strongest first. Refuting drops a finding; one the scorer
 * confirmed is kept whatever its confidence; one it couldn't settle either way is kept at `min` or
 * above, the only place a confidence threshold does more good than harm. An unscored candidate is
 * kept: the process failing isn't evidence against the finding.
 */
export function keepScored(cands: Candidate[], min = MIN_CONFIDENCE): Candidate[] {
  const out = cands.filter((c) => {
    if (!c.scored) return true;
    if (c.refuted) return false;
    if (c.verdict === 'confirmed') return true;
    return c.score >= min;
  });
  // Array.prototype.sort is stable.
  return out.sort((a, b) => b.score - a.score);
}

/** Why `validate` didn't keep a candidate, in validate.go's words. */
export type WithheldReason = 'empty comment' | 'file is not changed by this change' | 'line is outside the diff' | 'over the comment limit';

/**
 * The candidates that can be shown on the change's diff, worst first, at most `maxFindings`: a
 * finding's file has to be one the change touches and its line inside a hunk, since the model's
 * output is derived from text the change's author controls and has to be checked rather than
 * trusted. The rest are withheld with the reason. Comments over MAX_COMMENT_BYTES are cut.
 */
export function validate(cands: Candidate[], diff: Diff, maxFindings = 100): { post: Candidate[]; withheld: { candidate: Candidate; reason: WithheldReason }[] } {
  const post: Candidate[] = [];
  const withheld: { candidate: Candidate; reason: WithheldReason }[] = [];
  const sorted = [...cands].sort((a, b) => rank(a.finding.severity) - rank(b.finding.severity));
  for (const c of sorted) {
    const comment = c.finding.comment.trim();
    const file = diff.files.find((f) => f.path === c.finding.file);
    let reason: WithheldReason | null = null;
    if (!comment) reason = 'empty comment';
    else if (!file) reason = 'file is not changed by this change';
    else if (!anchorsLine(file, c.finding.line)) reason = 'line is outside the diff';
    else if (maxFindings > 0 && post.length >= maxFindings) reason = 'over the comment limit';
    if (reason) {
      withheld.push({ candidate: c, reason });
      continue;
    }
    const cut = Buffer.byteLength(comment) > MAX_COMMENT_BYTES
      ? `${Buffer.from(comment).subarray(0, MAX_COMMENT_BYTES).toString('utf8').replace(/�+$/, '')}\n\n[truncated]`
      : comment;
    post.push({ ...c, finding: { ...c.finding, comment: cut } });
  }
  return { post, withheld };
}

/** How many of each severity. */
export function countsOf(cands: Candidate[]): SeverityCounts {
  const counts = noCounts();
  for (const c of cands) counts[c.finding.severity]++;
  return counts;
}

/**
 * The passes' summaries worth showing: "LGTM" is silence, and identical text from two passes is one
 * note (they overlap at the edges by design). One note reads as prose; several as a list.
 */
export function mergeNotes(reports: PassReport[]): string {
  const parts: string[] = [];
  const seen = new Set<string>();
  for (const r of reports) {
    const s = r.summary.trim();
    if (!s || s.replace(/^[. ]+|[. ]+$/g, '').toLowerCase() === 'lgtm') continue;
    const k = s.toLowerCase();
    if (seen.has(k)) continue;
    seen.add(k);
    parts.push(s.replaceAll('\n', ' '));
  }
  if (parts.length <= 1) return parts[0] ?? '';
  return parts.map((p) => `- ${p}`).join('\n');
}
