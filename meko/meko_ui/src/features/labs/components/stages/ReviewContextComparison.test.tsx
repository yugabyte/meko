import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

import { ReviewContextComparison } from "./ReviewContextComparison";

afterEach(cleanup);

describe("ReviewContextComparison", () => {
  it("shows both the without-context and with-context cards", () => {
    render(<ReviewContextComparison agentDisplayName="AgentK" />);
    expect(screen.getByText("Without context")).toBeTruthy();
    expect(screen.getByText("With context")).toBeTruthy();
  });

  it("without context misses the issue; with context catches and explains it", () => {
    render(<ReviewContextComparison agentDisplayName="AgentK" />);
    expect(screen.getByText('"Looks good to me."')).toBeTruthy();
    expect(screen.getByText(/missed the raw sql/i)).toBeTruthy();
    expect(screen.getByText(/parameterized queries/i)).toBeTruthy();
    expect(screen.getByText(/caught it, cites your knowledge base/i)).toBeTruthy();
  });

  it("names the lab's own agent instead of a generic 'this agent'", () => {
    render(<ReviewContextComparison agentDisplayName="AgentK" />);
    expect(screen.getByText(/reviewed by AgentK/i)).toBeTruthy();
  });
});
