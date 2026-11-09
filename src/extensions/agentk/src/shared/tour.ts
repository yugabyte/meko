import type { TourScoreSide } from './types.js';

/**
 * How the tour's scoreboard says its numbers (src/server/tour.ts fills a stage's text with them, and
 * src/web/components/TourPanel.tsx shows them).
 */

/** `41.2k tokens in`, or Kiro's credits when its session billed those instead. */
export function tokensText(side: Pick<TourScoreSide, 'tokensIn' | 'credits'>): string {
  if (!side.tokensIn && side.credits) return `${Number(side.credits.toFixed(2))} credits`;
  const n = side.tokensIn;
  const short = n >= 1_000_000 ? `${Number((n / 1_000_000).toFixed(1))}M` : n >= 1000 ? `${Number((n / 1000).toFixed(1))}k` : `${n}`;
  return `${short} tokens in`;
}

/** A scoreboard side's line: `2 issues`, or a dash before its review is done. What Meko caught is the Difference row's. */
export function sideText(s: TourScoreSide | null): string {
  if (!s) return '—';
  return `${s.kept} ${s.kept === 1 ? 'issue' : 'issues'}`;
}

/** The scoreboard's headline once both reviews are done: what the review with context caught with Meko. */
export function scoreDifference(context: TourScoreSide): string {
  return context.caught ? `Caught with Meko: ${context.gotchas.join(', ')}` : 'Nothing caught with Meko this time';
}
