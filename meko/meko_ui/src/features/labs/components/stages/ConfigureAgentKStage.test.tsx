// The model and source toggles moved here out of the review step, so this is
// where their wiring is pinned: what the panel changes must reach the caller
// that owns the config, since the review a chapter later reads it.

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";

import { ConfigureAgentKStage } from "./ConfigureAgentKStage";
import {
  DEFAULT_REVIEW_AGENT_CONFIG,
  MODEL_OPTIONS,
  type ReviewAgentConfig,
} from "../demo-apps/ReviewAgentConfigPanel";

afterEach(cleanup);

function renderStage(overrides: Partial<ReviewAgentConfig> = {}) {
  const onConfigChange = vi.fn();
  const onGoToCreateDatapack = vi.fn();
  render(
    <ConfigureAgentKStage
      agentDisplayName="AgentK"
      datapackId="dp-1"
      config={{ ...DEFAULT_REVIEW_AGENT_CONFIG, ...overrides }}
      onConfigChange={onConfigChange}
      onGoToCreateDatapack={onGoToCreateDatapack}
    />,
  );
  return { onConfigChange, onGoToCreateDatapack };
}

describe("ConfigureAgentKStage", () => {
  it("asks for a datapack first, since the toggles gate reads against one", () => {
    const { onGoToCreateDatapack } = renderStage();
    cleanup();
    render(
      <ConfigureAgentKStage
        agentDisplayName="AgentK"
        datapackId={null}
        config={DEFAULT_REVIEW_AGENT_CONFIG}
        onConfigChange={vi.fn()}
        onGoToCreateDatapack={onGoToCreateDatapack}
      />,
    );
    expect(screen.queryByRole("switch")).toBeNull();
  });

  it("reports a switched-off source to the owner of the config", () => {
    const { onConfigChange } = renderStage();

    // First switch is the knowledgebase_search chip, before memory_search.
    fireEvent.click(screen.getAllByRole("switch")[0]);

    expect(onConfigChange).toHaveBeenCalledWith(
      expect.objectContaining({ useKnowledgeBase: false, useMemories: true }),
    );
  });

  it("reports a focus note, which lives behind a disclosure", () => {
    const { onConfigChange } = renderStage();

    fireEvent.click(screen.getByText("+ Tone, strictness, focus"));
    fireEvent.change(screen.getByPlaceholderText(/pay extra attention/i), {
      target: { value: "Check for SQL injection." },
    });

    expect(onConfigChange).toHaveBeenCalledWith(
      expect.objectContaining({ focus: "Check for SQL injection." }),
    );
  });

  it("shows the model the review will run with", () => {
    renderStage({ model: MODEL_OPTIONS[1].value });
    expect(screen.getByText(MODEL_OPTIONS[1].label)).toBeTruthy();
  });
});
