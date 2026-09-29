// Attaches the knowledge chunks and memories a review retrieved to the
// individual findings they most plausibly drove.
//
// IMPORTANT: this is a match, not an attribution. The decision trace records
// what the turn retrieved, never which chunk produced which comment, so the
// pairing below is computed here from word overlap and every caller must say
// so. It exists because "these four rules were retrieved, somewhere among
// these six comments" is not a thing a reader can act on, and a wrong-looking
// pairing they can see is better than a correct one they cannot.

export type RetrievedSource = {
  kind: "knowledge" | "memory";
  /** Document name or memory scope, for the row's own label. */
  source: string;
  quote: string;
};

export type GroundedSource = RetrievedSource & { overlap: number };

/** Words that appear in nearly every code-review sentence and every coding
 *  standard, so counting them would rank every pair the same. */
const STOPWORDS = new Set([
  "that",
  "this",
  "with",
  "from",
  "into",
  "must",
  "should",
  "never",
  "always",
  "when",
  "which",
  "their",
  "there",
  "have",
  "been",
  "will",
  "would",
  "code",
  "line",
  "lines",
  "file",
  "using",
  "used",
  "than",
  "then",
  "each",
  "your",
  "here",
]);

/** Crude stemming, so "concatenating" in a comment reaches "concatenation"
 *  in the rule it came from. Exact word matching missed almost every real
 *  pair: a review writes the verb and a standard writes the noun. */
function stem(word: string): string {
  const singular = word.length > 4 && word.endsWith("s") ? word.slice(0, -1) : word;
  return singular.slice(0, 6);
}

function terms(text: string): Set<string> {
  const out = new Set<string>();
  for (const word of text.toLowerCase().split(/[^a-z0-9_]+/)) {
    if (word.length >= 4 && !STOPWORDS.has(word)) out.add(stem(word));
  }
  return out;
}

/** How much two texts share, weighted so one distinctive word counts for
 *  more than one vague one. A long stem ("inject", "concat") is specific
 *  enough to carry a pairing on its own; a four-letter one ("team", "rows")
 *  is not, and needs a second word beside it to reach the threshold. */
export function overlapScore(a: string, b: string): number {
  const left = terms(a);
  if (left.size === 0) return 0;
  let score = 0;
  terms(b).forEach((word) => {
    if (left.has(word)) score += word.length >= 5 ? 2 : 1;
  });
  return score;
}

/**
 * The retrieved sources that best match one finding, strongest first.
 *
 * `minOverlap` is 2 rather than 1 deliberately: on overlapScore's weighting
 * that is one distinctive word or two vague ones, and a single vague word is
 * usually the file or the language, which pairs a finding with a rule it has
 * nothing to do with. Returning an empty list is a fine answer, and the
 * caller shows no grounding for that finding.
 */
export function groundFinding(
  findingText: string,
  sources: RetrievedSource[],
  { limit = 2, minOverlap = 2 }: { limit?: number; minOverlap?: number } = {},
): GroundedSource[] {
  return sources
    .map((source) => ({ ...source, overlap: overlapScore(findingText, source.quote) }))
    .filter((source) => source.overlap >= minOverlap)
    .sort((a, b) => b.overlap - a.overlap)
    .slice(0, limit);
}
