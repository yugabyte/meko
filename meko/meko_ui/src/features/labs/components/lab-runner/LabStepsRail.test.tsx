import { afterEach, describe, expect, it } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { GitPullRequest } from "lucide-react";

import { useLabProgress } from "../../hooks/useLabProgress";
import type { LabDefinition } from "../../types";
import { LabStepsRail } from "./LabStepsRail";

afterEach(() => {
  cleanup();
  localStorage.clear();
});

const FIXTURE_LAB: LabDefinition = {
  id: "test-lab",
  title: "Test Lab",
  summary: "",
  description: "",
  tags: [],
  icon: GitPullRequest,
  estimatedMinutes: 1,
  chapters: [
    {
      id: "chapter-1",
      title: "Chapter One",
      steps: [
        { id: "c1-s1", title: "Step One", body: "First step body" },
        { id: "c1-s2", title: "Step Two", body: "Second step body" },
      ],
    },
    {
      id: "chapter-2",
      title: "Chapter Two",
      steps: [{ id: "c2-s1", title: "Only Step", body: "Third step body" }],
    },
  ],
  demoApp: { kind: "pr-review-chat", persona: "review_agent", datapackName: "test-datapack" },
  observation: { kind: "workbench-live-observation" },
};

function TestHarness() {
  const progress = useLabProgress(FIXTURE_LAB);
  return <LabStepsRail lab={FIXTURE_LAB} progress={progress} />;
}

describe("LabStepsRail", () => {
  it("starts on chapter 1 / step 1, with that step's body visible", () => {
    render(<TestHarness />);
    expect(screen.getByText("First step body")).toBeTruthy();
    expect(screen.queryByText("Second step body")).toBeNull();
    expect(screen.getByRole("button", { name: "Next" })).toBeTruthy();
  });

  it("Next advances within the chapter before moving to the next chapter", () => {
    render(<TestHarness />);
    fireEvent.click(screen.getByRole("button", { name: "Next" }));
    expect(screen.getByText("Second step body")).toBeTruthy();
    // Last step of chapter 1 (of 2) -> button relabels.
    expect(screen.getByRole("button", { name: "Next chapter" })).toBeTruthy();
  });

  it("Next chapter moves into chapter 2 and disables the button on the true last step", () => {
    render(<TestHarness />);
    fireEvent.click(screen.getByRole("button", { name: "Next" })); // -> c1-s2
    fireEvent.click(screen.getByRole("button", { name: "Next chapter" })); // -> c2-s1
    expect(screen.getByText("Third step body")).toBeTruthy();
    const doneButton = screen.getByRole("button", { name: "Done" }) as HTMLButtonElement;
    expect(doneButton.disabled).toBe(true);
  });
});
