import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

import { ContextIntroStage } from "./ContextIntroStage";

afterEach(cleanup);

describe("ContextIntroStage", () => {
  it("introduces all three context concepts", () => {
    render(<ContextIntroStage />);
    expect(screen.getByText("What is context?")).toBeTruthy();
    expect(screen.getByText("Knowledge Base")).toBeTruthy();
    expect(screen.getByText("Memory")).toBeTruthy();
    expect(screen.getByText("Conversation")).toBeTruthy();
  });
});
