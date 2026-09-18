// The phase and badge maps are a contract with api_server's step names:
// groupStepsByPhase drops a name it doesn't know, silently, so a step the
// backend starts emitting renders as nothing at all.

import { describe, expect, it } from "vitest";

import {
  computeDbBreakdown,
  estimateTokensSaved,
  groupStepsByPhase,
  prettifyStepName,
  stepKindLabel,
} from "./helpers";
import type { ProcessingStep } from "@/lib/inferenceGateway";

function step(name: string, duration_ms = 0): ProcessingStep {
  return { name, status: "ok", duration_ms } as ProcessingStep;
}

function convStep(chars: number): ProcessingStep {
  return {
    name: "conversation_search",
    status: "ok",
    conversation_chunks: [{ content: "x".repeat(chars) }],
  } as ProcessingStep;
}

describe("decision-trace helpers", () => {
  // MEKO-663: context_search emits one span per leg, and api_server maps them
  // onto these three names. A name missing here means a turn that retrieved
  // something renders an empty Preprocess phase.
  it("renders every retrieval step context_search produces", () => {
    const { preprocess } = groupStepsByPhase([
      step("memory_search"),
      step("knowledgebase_search"),
      step("conversation_search"),
    ]);
    expect(preprocess.map((s) => s.name)).toEqual([
      "memory_search",
      "knowledgebase_search",
      "conversation_search",
    ]);
  });

  describe("computeDbBreakdown", () => {
    // The regression: conversation_search was badged DB but missing from the
    // breakdown's own copy of the list, so a turn whose only DB step was that
    // one showed a DB-badged step above `db: 0`.
    it("counts a conversation_search-only turn", () => {
      const rows = computeDbBreakdown([step("conversation_search", 12)]);
      expect(rows).toEqual([
        { name: "conversation_search", prettyName: "conversation.search", count: 1, ms: 12 },
      ]);
    });

    // The invariant behind that regression, asserted against the function that
    // broke it rather than against the list it now shares.
    it("counts every step the badge calls DB", () => {
      const badged = ["memory_search", "knowledgebase_search", "conversation_search", "memory_add"];
      const rows = computeDbBreakdown(badged.map((n) => step(n)));
      expect(rows.map((r) => r.name)).toEqual(badged);
      for (const name of badged) expect(stepKindLabel(name)).toBe("DB");
    });

    it("sums repeats of one step and omits steps that never ran", () => {
      const rows = computeDbBreakdown([
        step("memory_search", 10),
        step("memory_search", 5),
        step("llm_invoke", 900),
      ]);
      expect(rows).toHaveLength(1);
      expect(rows[0]).toMatchObject({ name: "memory_search", count: 2, ms: 15 });
    });

    it("leaves out conversation.persist — it writes an audit row, not a datapack row", () => {
      expect(stepKindLabel("conversation_add_message")).toBe("LOG");
      expect(computeDbBreakdown([step("conversation_add_message", 3)])).toEqual([]);
    });
  });

  describe("estimateTokensSaved", () => {
    it("estimates from the chunks a cache hit answered with", () => {
      expect(estimateTokensSaved([convStep(400)], "conversation_cache")).toBe(100);
    });

    // The regression this gate exists for: the step now carries chunks on
    // turns the full model answered, so chunk presence alone would price a
    // normal turn as free.
    it("claims nothing when the model actually ran", () => {
      expect(estimateTokensSaved([convStep(400)], undefined)).toBe(0);
      expect(estimateTokensSaved([convStep(400)], "conversation_cache_rejected")).toBe(0);
    });

    // Blended: the chunks fed the prompt and the model still ran.
    it("claims nothing on a blended turn", () => {
      expect(estimateTokensSaved([convStep(400)], "conversation_context")).toBe(0);
    });

    it("claims nothing when a cache hit carries no chunks", () => {
      expect(estimateTokensSaved([step("conversation_search")], "conversation_cache")).toBe(0);
      expect(estimateTokensSaved([], "conversation_cache")).toBe(0);
    });
  });

  it("names each retrieval step rather than echoing the raw name", () => {
    expect(prettifyStepName("conversation_search")).toBe("conversation.search");
    expect(prettifyStepName("knowledgebase_search")).toBe("knowledge.retrieve");
    // Unknown names fall through on purpose, so a new backend step needs no
    // frontend deploy to render at all.
    expect(prettifyStepName("something_new")).toBe("something_new");
  });
});
