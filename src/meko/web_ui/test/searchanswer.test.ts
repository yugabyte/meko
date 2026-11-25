// passagesOf: which search results an answer is written from, and in what order.
import { describe, expect, it } from "vitest";
import { passagesOf } from "../src/components/SearchAnswer";
import type { ContextSearch } from "../src/lib/types";

const hits = (n: number, prefix: string) => Array.from({ length: n }, (_, i) => `${prefix}${i}`);

describe("passagesOf", () => {
  it("takes from each source in turn, up to 10, leaving weak memories out", () => {
    const r: ContextSearch["results"] = {
      memory: hits(6, "m").map((id) => ({ id, memory: id, agent_id: "a", created_at: "" })),
      knowledge_base: hits(6, "k").map((id) => ({ id, chunk_text: id, document_id: "d", distance: 0, match_type: "" })),
      conversation: { hits: hits(1, "c").map((id) => ({ conversation_id: "x", message_id: id, content: id, distance: 0 })), thread: [] },
    };
    const got = passagesOf(r, (id) => id !== "m0").map((p) => p.id);
    expect(got).toEqual(["m1", "k0", "c0", "m2", "k1", "m3", "k2", "m4", "k3", "m5"]);
  });

  it("skips a source that wasn't searched", () => {
    const r: ContextSearch["results"] = { memory: null, knowledge_base: null, conversation: null };
    expect(passagesOf(r, () => true)).toEqual([]);
  });
});
