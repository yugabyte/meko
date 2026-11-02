import { countsText, totalCount } from '../../shared/codereview';
import type { CodeReviewUsage } from '../../shared/types';
import { CreditsStat } from './UsagePanel';

/** The rounds the reviews ran, and those that haven't ended done, in words: "5 rounds · 1 failed · 1 running". */
export function reviewOutcomeNote(r: CodeReviewUsage): string {
  const { failed, cancelled, running } = r.byOutcome;
  return [
    `${r.rounds.toLocaleString()} ${r.rounds === 1 ? 'round' : 'rounds'}`,
    failed && `${failed.toLocaleString()} failed`,
    cancelled && `${cancelled.toLocaleString()} cancelled`,
    running && `${running.toLocaleString()} running`,
  ].filter(Boolean).join(' · ');
}

/**
 * Code reviews (docs/design/code-review.md), under the Meko section on the project home and the
 * Usage page: the reviews, the issues they found, what the task fixed, the PRs opened after them, and
 * the Kiro credits their sessions billed. Their tokens are in the Token usage chart, stacked on each
 * day's bar. Shown only once a review has run.
 */
export function CodeReviewUsageSection({ reviews }: { reviews: CodeReviewUsage }) {
  const issues = totalCount(reviews.findings);
  const outcomes = reviewOutcomeNote(reviews);
  return (
    <div className="review-usage">
      <span className="label">Code Reviews</span>
      <div className="usage-stats">
        <Tile label="Reviews" value={reviews.runs} note={outcomes} />
        <Tile label="Issues found" value={issues} note={issues ? countsText(reviews.findings) : undefined} />
        <Tile label="Fixed" value={reviews.fixed} note={reviews.declined ? `${reviews.declined.toLocaleString()} declined` : undefined} />
        <Tile label="PRs opened" value={reviews.prs} note={reviews.lgtm ? `${reviews.lgtm.toLocaleString()} LGTM` : undefined} />
        <CreditsStat usage={reviews.usage} />
      </div>
    </div>
  );
}

function Tile({ label, value, note }: { label: string; value: number; note?: string }) {
  return (
    <div className="usage-stat">
      <span className="usage-num">{value.toLocaleString()}</span>
      <span className="usage-lbl">{label}</span>
      {note && <span className="muted small">{note}</span>}
    </div>
  );
}
