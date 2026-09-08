import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

import { ContextSourceTracker } from "./ContextSourceTracker";

afterEach(cleanup);

describe("ContextSourceTracker", () => {
  it("always shows all three sources by name", () => {
    render(<ContextSourceTracker current="memory" />);
    expect(screen.getByText("Knowledge Base")).toBeTruthy();
    expect(screen.getByText("Memory")).toBeTruthy();
    expect(screen.getByText("Conversation")).toBeTruthy();
  });

  it("marks sources before the current one as done, via a checkmark icon", () => {
    const { container } = render(<ContextSourceTracker current="conversation" />);
    // Knowledge Base and Memory are done (before "conversation" in the fixed
    // order); their icon swaps to a checkmark -- Conversation's own Sparkles/
    // FileText/MessageSquare icons render as <svg>, so counting checkmarks is
    // the only reliable way to assert "done" from the DOM without snapshotting.
    const checkmarks = container.querySelectorAll("svg.lucide-check");
    expect(checkmarks).toHaveLength(2);
  });

  it("renders no checkmarks when the first source is current", () => {
    const { container } = render(<ContextSourceTracker current="knowledge-base" />);
    expect(container.querySelectorAll("svg.lucide-check")).toHaveLength(0);
  });
});
