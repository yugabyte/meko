// Scroll-target ids for the Analytics tab. Shared between whatever renders an
// anchor and whatever scrolls to it, so a rename can't silently break the
// jump — a literal in two files already had them drifting.

export const METRIC_ANCHOR = {
  contextSize: "metric-context-size",
  tokenSavings: "metric-token-savings",
  matchQuality: "metric-match-quality",
  queryActivity: "metric-query-activity",
  retrievalSection: "section-retrieval-efficiency",
} as const;

export type MetricAnchor = (typeof METRIC_ANCHOR)[keyof typeof METRIC_ANCHOR];
