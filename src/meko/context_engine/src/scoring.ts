// Meko's memory score. mem0 fuses a memory's semantic similarity, its normalized BM25 score and its
// entity boost into one 0-1 score. Upstream divides every memory by the most any memory in the
// result set could reach; Meko divides each memory only by what that memory earned (1 for the
// semantic score, +1 if it had a keyword hit, +0.5 if it had an entity boost), so a memory isn't
// pulled down by another memory's keyword hit. The cosine itself comes back as
// raw_similarity_score.

/** The parts of a score, as mem0 reports them in a search with explain: true. */
export interface ScoreDetails {
  semanticScore: number;
  bm25Score: number;
  entityBoost: number;
}

/** mem0's weight for an entity boost: its share of the divisor. */
export const ENTITY_BOOST_WEIGHT = 0.5;

export function mekoScore(d: ScoreDetails): number {
  const divisor = 1 + (d.bm25Score > 0 ? 1 : 0) + (d.entityBoost > 0 ? ENTITY_BOOST_WEIGHT : 0);
  return Math.min((d.semanticScore + d.bm25Score + d.entityBoost) / divisor, 1);
}
