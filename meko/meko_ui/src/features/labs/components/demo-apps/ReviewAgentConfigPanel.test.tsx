import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";

import {
  DEFAULT_REVIEW_AGENT_CONFIG,
  ReviewAgentConfigPanel,
  type ReviewAgentConfig,
} from "./ReviewAgentConfigPanel";

afterEach(cleanup);

function renderPanel(value: ReviewAgentConfig = DEFAULT_REVIEW_AGENT_CONFIG) {
  const onChange = vi.fn();
  render(<ReviewAgentConfigPanel value={value} onChange={onChange} agentDisplayName="AgentK" />);
  return onChange;
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
    fireEvent.change(screen.getByPlaceholderText(/pay extra attention/i), {
      target: { value: "Watch for N+1 queries." },
    });
    expect(onChange).toHaveBeenCalledWith({ ...DEFAULT_REVIEW_AGENT_CONFIG, focus: "Watch for N+1 queries." });
  });

  it("caps the focus textarea at 300 characters", () => {
    renderPanel();
    const textarea = screen.getByPlaceholderText(/pay extra attention/i) as HTMLTextAreaElement;
    expect(textarea.maxLength).toBe(300);
  });

  it("tone and strictness default to unset (\"Default\")", () => {
    renderPanel();
    const triggers = screen.getAllByText("Default");
    expect(triggers).toHaveLength(2);
  });

  it("reflects a pre-set tone/strictness value in the trigger", () => {
    renderPanel({ ...DEFAULT_REVIEW_AGENT_CONFIG, tone: "concise", strictness: "strict" });
    expect(screen.getByText("Concise")).toBeTruthy();
    expect(screen.getByText("Strict")).toBeTruthy();
  });
});
