import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

import { ReviewLoopDiagram } from "./ReviewLoopDiagram";

afterEach(cleanup);

describe("ReviewLoopDiagram", () => {
  it("shows the three loop nodes", () => {
    render(<ReviewLoopDiagram agentDisplayName="AgentK" />);
    expect(screen.getByText("Seed Context")).toBeTruthy();
    expect(screen.getByText("Datapack")).toBeTruthy();
    expect(screen.getByText("Run a Review")).toBeTruthy();
  });

  it("names the real Meko retrieval tools, not an invented one", () => {
    const { container } = render(<ReviewLoopDiagram agentDisplayName="AgentK" />);
    const text = container.textContent ?? "";
    expect(text).toContain("context_search");
    expect(text).toContain("memory_search");
    expect(text).toContain("knowledgebase_search");
    expect(text).not.toContain("datapack_search");
  });

  it("explains the closed-loop write-back", () => {
    render(<ReviewLoopDiagram agentDisplayName="AgentK" />);
    expect(screen.getByText(/what you seed is what gets retrieved/i)).toBeTruthy();
  });

  it("names the lab's own agent instead of a generic 'this agent'", () => {
    render(<ReviewLoopDiagram agentDisplayName="AgentK" />);
    expect(screen.getByText("How AgentK reads your datapack")).toBeTruthy();
  });
});
