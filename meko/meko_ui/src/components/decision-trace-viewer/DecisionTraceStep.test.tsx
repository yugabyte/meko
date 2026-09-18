// Behaviour of the MATCHED CONVERSATIONS block inside an expanded step row:
// a conversation chunk shows a match chip only when the trace carried a score.

import { afterEach, describe, expect, it } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";

import type { ProcessingStep } from "@/lib/inferenceGateway";
import { DecisionTraceStep } from "./DecisionTraceStep";

afterEach(cleanup);

function renderStep(step: ProcessingStep) {
  render(<DecisionTraceStep step={step} accent="purple" conversationId="c1" />);
  // Rows are collapsed by default; the chunks live in the expanded body.
  fireEvent.click(screen.getByRole("button", { expanded: false }));
}

function stepWith(chunk: Record<string, unknown>): ProcessingStep {
  return {
    name: "conversation_search",
    duration_ms: 12,
    status: "ok",
    conversation_chunks: [{ content: "Prior turn about billing", ...chunk }],
  } as ProcessingStep;
}

describe("DecisionTraceStep conversation chunks", () => {
  it("renders a match chip from the score", () => {
    renderStep(stepWith({ score: 0.79, distance: 0.21 }));
    expect(screen.getByText("79% match")).toBeTruthy();
    expect(screen.getByText("0.210")).toBeTruthy();
  });

  it("clamps an anti-correlated score to 0%", () => {
    // distance > 1 yields a negative similarity; the chip floors it rather
    // than rendering "-1% match".
    renderStep(stepWith({ score: -0.015, distance: 1.015 }));
    expect(screen.getByText("0% match")).toBeTruthy();
  });

  it("omits the chip when the chunk carries no score", () => {
    // Thread turns and traces recorded before the gateway emitted `score`.
    renderStep(stepWith({ distance: 0.42 }));
    expect(screen.queryByText(/% match/)).toBeNull();
    expect(screen.getByText("0.420")).toBeTruthy();
  });
});
