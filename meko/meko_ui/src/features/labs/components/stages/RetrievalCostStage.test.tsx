import { afterEach, describe, expect, it } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";

import type { LabContextFeed } from "../../hooks/useLabContextFeed";
import { RetrievalCostStage } from "./RetrievalCostStage";

function feed(overrides: Partial<LabContextFeed> = {}): LabContextFeed {
  return {
    knowledge: [],
    knowledgeTotal: 0,
    memories: [],
    memoriesTotal: 0,
    collective: [],
    collectiveTotal: 0,
    conversationsTotal: 0,
  chunkSize: null,
    loading: false,
    ...overrides,
  };
}

afterEach(cleanup);

describe("RetrievalCostStage", () => {
  // Opens on retrieval because that is what the agent actually does; the
  // other card is there to show what it would have cost instead.
  it("opens with retrieval already chosen, and says what it saves", () => {
    render(<RetrievalCostStage agentDisplayName="AgentK" feed={feed()} />);
    expect(screen.getByRole("status").textContent).toMatch(/fewer input tokens per review/);
    expect(screen.queryByRole("alert")).toBeNull();
  });

  // Picking the expensive option should READ as a warning, not as neutral
  // prose a user skims past.
  it("warns when the user picks pasting the whole document", () => {
    render(<RetrievalCostStage agentDisplayName="AgentK" feed={feed()} />);
    fireEvent.click(screen.getByText("Paste the whole doc"));
    expect(screen.getByRole("alert").textContent).toMatch(/the input, same answer/);
  });

  it("comes back to the saving after the user has looked at the costly option", () => {
    render(<RetrievalCostStage agentDisplayName="AgentK" feed={feed()} />);
    fireEvent.click(screen.getByText("Paste the whole doc"));
    fireEvent.click(screen.getByText("Ask for the rules that apply"));
    const banner = screen.getByRole("status");
    expect(banner.textContent).toMatch(/fewer input tokens per review/);
    // Good news must not be announced as an alert.
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("labels both figures as estimates, since neither was measured here", () => {
    render(<RetrievalCostStage agentDisplayName="AgentK" feed={feed()} />);
    expect(screen.getByText(/Both figures are estimates/)).toBeTruthy();
  });

  // The lesson should quote the user's OWN document once they've uploaded
  // one, not the bundled sample they may never have used.
  it("estimates from the uploaded documents' real size when there are any", () => {
    render(
      <RetrievalCostStage
        agentDisplayName="AgentK"
        feed={feed({
          knowledgeTotal: 1,
          knowledge: [{ id: "d1", size_bytes: 40_000 }] as never,
        })}
      />,
    );
    // 40,000 bytes / 4 = 10,000 tokens pasted, a quarter of that retrieved.
    expect(screen.getByText(/10,000 input tokens/)).toBeTruthy();
    expect(screen.getByText(/2,500 input tokens/)).toBeTruthy();
    expect(screen.getByText(/size of what you uploaded/)).toBeTruthy();
  });

  it("falls back to the sample document when nothing has been uploaded yet", () => {
    render(<RetrievalCostStage agentDisplayName="AgentK" feed={feed()} />);
    expect(screen.getByText(/sample document's size/)).toBeTruthy();
  });
});
