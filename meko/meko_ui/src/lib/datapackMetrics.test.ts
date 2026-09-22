import { describe, expect, it } from "vitest";

import { CONTEXT_WINDOW, deriveDatapackMetrics, KB_TOKENS_PER_CHUNK } from "./datapackMetrics";
import type { DatapackMetrics } from "./api/mekoApi";

const COUNTS = { knowledgeCount: 0, memoryCount: 0, collectiveMemoryCount: 0 };

/** Only the fields the derivation reads; the rest of DatapackMetrics is
 *  irrelevant here and would only make the cases harder to read. */
function metrics(partial: Partial<DatapackMetrics>): DatapackMetrics {
  return {
    total_requests: 0,
    tokens_saved_total: 0,
    tokens_if_raw_total: 0,
    savings_pct: 0,
    score_high_total: 0,
    score_mid_total: 0,
    score_low_total: 0,
    kb_chunk_count: 0,
    memory_chunk_count: 0,
    conversation_chunk_count: 0,
    ...partial,
  } as DatapackMetrics;
}

describe("deriveDatapackMetrics", () => {
  it("falls back to per-file estimates before the payload arrives", () => {
    const d = deriveDatapackMetrics(undefined, {
      knowledgeCount: 2,
      memoryCount: 3,
      collectiveMemoryCount: 1,
    });
    // 2 files × 10 chunks × 1024 tokens
    expect(d.tokens.knowledge).toBe(2 * 10 * KB_TOKENS_PER_CHUNK);
    // 4 memories × 200 tokens
    expect(d.tokens.memory).toBe(800);
    expect(d.tokens.conversation).toBe(0);
  });

  it("prefers real chunk counts over the estimate once they arrive", () => {
    const d = deriveDatapackMetrics(metrics({ kb_chunk_count: 16 }), {
      ...COUNTS,
      knowledgeCount: 99,
    });
    expect(d.tokens.knowledge).toBe(16 * KB_TOKENS_PER_CHUNK);
  });

  it("reports no savings when nothing has been requested", () => {
    const d = deriveDatapackMetrics(metrics({ total_requests: 0 }), COUNTS);
    expect(d.savedPct).toBeNull();
    expect(d.avgTokensSent).toBeNull();
    expect(d.windowUsed).toBeNull();
  });

  it("reports no savings when the backend never populated the raw baseline", () => {
    // A real case: total_requests is set but tokens_if_raw_total is 0, which
    // would otherwise divide into a 100%-saved claim.
    const d = deriveDatapackMetrics(
      metrics({ total_requests: 5, tokens_if_raw_total: 0, savings_pct: 1 }),
      COUNTS,
    );
    expect(d.savedPct).toBeNull();
  });

  it("averages sent and saved tokens per request", () => {
    const d = deriveDatapackMetrics(
      metrics({
        total_requests: 2,
        tokens_if_raw_total: 2_000,
        tokens_saved_total: 1_400,
        savings_pct: 0.7,
      }),
      COUNTS,
    );
    expect(d.avgTokensIfRaw).toBe(1_000);
    expect(d.avgTokensSent).toBe(300);
    expect(d.avgTokensSaved).toBe(700);
    expect(d.savedPct).toBe(0.7);
    expect(d.windowUsed).toBeCloseTo(300 / CONTEXT_WINDOW);
  });

  it("scores quality on high AND mid, not high alone", () => {
    const d = deriveDatapackMetrics(
      metrics({ score_high_total: 0, score_mid_total: 1, score_low_total: 0 }),
      COUNTS,
    );
    expect(d.qualityScore).toBe(1);
  });

  it("returns a zero quality score rather than dividing by zero", () => {
    const d = deriveDatapackMetrics(metrics({}), COUNTS);
    expect(d.qualityScore).toBe(0);
    expect(d.scores.total).toBe(0);
  });

  it("treats a datapack with content, scores or savings as non-empty", () => {
    expect(deriveDatapackMetrics(metrics({}), COUNTS).hasAnyData).toBe(false);
    expect(deriveDatapackMetrics(metrics({ kb_chunk_count: 1 }), COUNTS).hasAnyData).toBe(true);
    expect(deriveDatapackMetrics(metrics({ score_low_total: 1 }), COUNTS).hasAnyData).toBe(true);
  });
});
