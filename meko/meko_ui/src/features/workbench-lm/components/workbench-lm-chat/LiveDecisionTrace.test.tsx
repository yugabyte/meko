// The decision-trace endpoint and the conversation thread return different
// shapes for the same turn, and this adapter bridges them. Every field on both
// sides is optional, so dropping one compiles clean and fails silently.

import { describe, expect, it } from "vitest";

import type { DatapackMessageDecisionTrace } from "@/lib/api/mekoApi";
import { toThreadMessage } from "./LiveDecisionTrace";

const TRACE: DatapackMessageDecisionTrace = {
  message_id: "m1",
  latency_ms: 10,
  total_tokens: 20,
  llm_calls: 0,
  memories_extracted: 0,
  answered_from: "conversation_cache",
};

describe("toThreadMessage", () => {
  // Regression: answered_from was absent from the api type and from this
  // adapter, so it read undefined on every WorkbenchLM turn — including the
  // live cache hit the "~N saved" chip exists for.
  it("carries answered_from through", () => {
    expect(toThreadMessage(TRACE).answered_from).toBe("conversation_cache");
  });

  it("leaves it undefined when the turn had none", () => {
    const { answered_from: _omitted, ...rest } = TRACE;
    expect(toThreadMessage(rest).answered_from).toBeUndefined();
  });
});
