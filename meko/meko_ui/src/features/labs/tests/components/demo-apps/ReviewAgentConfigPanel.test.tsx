import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";

import {
  DEFAULT_REVIEW_AGENT_CONFIG,
  MODEL_OPTIONS,
  ReviewAgentConfigPanel,
  type ReviewAgentConfig,
} from "@/features/labs/components/demo-apps/ReviewAgentConfigPanel";

afterEach(cleanup);

function renderPanel(value: ReviewAgentConfig = DEFAULT_REVIEW_AGENT_CONFIG) {
  const onChange = vi.fn();
  render(<ReviewAgentConfigPanel value={value} onChange={onChange} agentDisplayName="AgentK" />);
  return onChange;
}

// Tone/strictness/focus live behind the "+ Tone, strictness, focus"
// disclosure, collapsed by default -- open it before asserting on anything
// inside.
function openAdvanced() {
  fireEvent.click(screen.getByText("+ Tone, strictness, focus"));
}

describe("ReviewAgentConfigPanel", () => {
  it("both switches default on", () => {
    renderPanel();
    const switches = screen.getAllByRole("switch") as HTMLButtonElement[];
    expect(switches).toHaveLength(2);
    for (const s of switches) {
      expect(s.getAttribute("aria-checked")).toBe("true");
    }
  });

  it("toggling the knowledge-base switch flips only that field", () => {
    const onChange = renderPanel();
    fireEvent.click(screen.getAllByRole("switch")[0]);
    expect(onChange).toHaveBeenCalledWith({ ...DEFAULT_REVIEW_AGENT_CONFIG, useKnowledgeBase: false });
  });

  it("toggling the memories switch flips only that field", () => {
    const onChange = renderPanel();
    fireEvent.click(screen.getAllByRole("switch")[1]);
    expect(onChange).toHaveBeenCalledWith({ ...DEFAULT_REVIEW_AGENT_CONFIG, useMemories: false });
  });

  it("typing in the focus textarea reports the updated string", () => {
    const onChange = renderPanel();
    openAdvanced();
    fireEvent.change(screen.getByPlaceholderText(/pay extra attention/i), {
      target: { value: "Watch for N+1 queries." },
    });
    expect(onChange).toHaveBeenCalledWith({ ...DEFAULT_REVIEW_AGENT_CONFIG, focus: "Watch for N+1 queries." });
  });

  it("caps the focus textarea at 300 characters", () => {
    renderPanel();
    openAdvanced();
    const textarea = screen.getByPlaceholderText(/pay extra attention/i) as HTMLTextAreaElement;
    expect(textarea.maxLength).toBe(300);
  });

  it("tone and strictness default to unset (\"Default\")", () => {
    renderPanel();
    openAdvanced();
    const triggers = screen.getAllByText("Default");
    expect(triggers).toHaveLength(2);
  });

  it("reflects a pre-set tone/strictness value in the trigger", () => {
    renderPanel({ ...DEFAULT_REVIEW_AGENT_CONFIG, tone: "concise", strictness: "strict" });
    openAdvanced();
    expect(screen.getByText("Concise")).toBeTruthy();
    expect(screen.getByText("Strict")).toBeTruthy();
  });

  it("model defaults to the first option, with no unset state", () => {
    renderPanel();
    expect(screen.getByText(MODEL_OPTIONS[0].label)).toBeTruthy();
  });

  it("reflects a pre-set model value in the trigger", () => {
    renderPanel({ ...DEFAULT_REVIEW_AGENT_CONFIG, model: MODEL_OPTIONS[2].value });
    expect(screen.getByText(MODEL_OPTIONS[2].label)).toBeTruthy();
  });
});
