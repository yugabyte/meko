import { afterEach, describe, expect, it } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";

import { useLabProgress } from "../../hooks/useLabProgress";
import type { LabStage } from "../../types";
import { LabStageNav } from "./LabStageNav";

afterEach(() => {
  cleanup();
  localStorage.clear();
});

const FIXTURE_STAGES: LabStage[] = [
  {
    id: "create-datapack",
    title: "Create a datapack",
    summary: "Pick one.",
    highlights: ["Pick an existing datapack.", "Or create a new one."],
  },
  {
    id: "seed-context",
    title: "Seed context",
    summary: "Upload or chat.",
    highlights: ["Upload documents.", "Or describe them in chat."],
  },
  {
    id: "run-review",
    title: "Run a review",
    summary: "Review a diff.",
    highlights: ["Review a real PR.", "Or the bundled sample diff."],
  },
];

function TestHarness() {
  const progress = useLabProgress({ id: "test-lab", stages: FIXTURE_STAGES } as never);
  return <LabStageNav stages={FIXTURE_STAGES} progress={progress} />;
}

describe("LabStageNav", () => {
  it("renders every stage's numbered heading", () => {
    render(<TestHarness />);
    expect(screen.getByText("Step 1. Create a datapack")).toBeTruthy();
    expect(screen.getByText("Step 2. Seed context")).toBeTruthy();
    expect(screen.getByText("Step 3. Run a review")).toBeTruthy();
  });

  it("only the current stage's card shows its body and a Next button", () => {
    render(<TestHarness />);
    expect(screen.getByText("Pick one.")).toBeTruthy();
    expect(screen.queryByText("Upload or chat.")).toBeNull();
    expect(screen.getByRole("button", { name: "Next" })).toBeTruthy();
  });

  it("clicking a collapsed stage card jumps directly to it", () => {
    render(<TestHarness />);
    fireEvent.click(screen.getByText("Step 3. Run a review"));
    expect(screen.getByText("Review a diff.")).toBeTruthy();
  });

  it("clicking Next inside the active card advances one stage, and reads Done + disables on the last one", () => {
    render(<TestHarness />);
    fireEvent.click(screen.getByRole("button", { name: "Next" }));
    expect(screen.getByText("Upload or chat.")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Next" }));
    const doneButton = screen.getByRole("button", { name: "Done" }) as HTMLButtonElement;
    expect(doneButton.disabled).toBe(true);
  });
});
