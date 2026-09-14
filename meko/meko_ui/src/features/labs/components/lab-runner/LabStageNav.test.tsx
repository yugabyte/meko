import { afterEach, describe, expect, it } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";

import { useLabProgress } from "../../hooks/useLabProgress";
import type { LabChapter, LabStage } from "../../types";
import { LabStageNav } from "./LabStageNav";

afterEach(() => {
  cleanup();
  localStorage.clear();
});

const CHAPTER_ONE_STAGES: LabStage[] = [
  { id: "see-the-pr", title: "See the pull request", summary: "" },
  { id: "create-datapack", title: "Create a datapack", summary: "" },
];
const CHAPTER_TWO_STAGES: LabStage[] = [
  { id: "meet-agentk", title: "Meet AgentK", summary: "" },
];

const TWO_CHAPTERS: LabChapter[] = [
  { id: "chapter-1", title: "Give your agent context", summary: "", stages: CHAPTER_ONE_STAGES },
  { id: "chapter-2", title: "Share it with AgentK", summary: "", stages: CHAPTER_TWO_STAGES },
];

function TestHarness({ chapters }: { chapters: LabChapter[] }) {
  const progress = useLabProgress({ id: "test-lab", chapters } as never);
  return <LabStageNav chapters={chapters} progress={progress} />;
}

describe("LabStageNav", () => {
  it("shows every chapter and every step at once, not just the current chapter", () => {
    render(<TestHarness chapters={TWO_CHAPTERS} />);
    expect(screen.getByText("Give your agent context")).toBeTruthy();
    expect(screen.getByText("Share it with AgentK")).toBeTruthy();
    expect(screen.getByText(/See the pull request/)).toBeTruthy();
    expect(screen.getByText(/Meet AgentK/)).toBeTruthy();
  });

  it("numbers steps from 1 within each chapter, so chapter 2 restarts at 1", () => {
    render(<TestHarness chapters={TWO_CHAPTERS} />);
    // Chapter 2's only stage is its step 1, not step 3 of the whole lab.
    expect(screen.getByText(/Meet AgentK/).textContent).toContain("1.");
  });

  it("marks the current step with aria-current", () => {
    render(<TestHarness chapters={TWO_CHAPTERS} />);
    const current = screen.getByRole("button", { current: "step" });
    expect(current.textContent).toContain("See the pull request");
  });

  it("disables steps the user hasn't reached yet", () => {
    render(<TestHarness chapters={TWO_CHAPTERS} />);
    const notReached = screen.getByRole("button", { name: /Meet AgentK/ });
    expect(notReached.hasAttribute("disabled")).toBe(true);
  });

  it("lets the user click back to a step they've already reached", () => {
    localStorage.setItem("meko:lab-progress:test-lab", JSON.stringify({ chapterIndex: 0, stageIndex: 1 }));
    localStorage.setItem("meko:lab-completed:test-lab", JSON.stringify(["see-the-pr"]));
    render(<TestHarness chapters={TWO_CHAPTERS} />);

    fireEvent.click(screen.getByRole("button", { name: /See the pull request/ }));
    expect(screen.getByRole("button", { current: "step" }).textContent).toContain("See the pull request");
  });

  it("counts progress by completed steps, not by how far the user has clicked", () => {
    localStorage.setItem("meko:lab-progress:test-lab", JSON.stringify({ chapterIndex: 1, stageIndex: 0 }));
    render(<TestHarness chapters={TWO_CHAPTERS} />);
    // Sitting on the last stage having finished nothing is still 0 of 3.
    expect(screen.getByText("0 of 3")).toBeTruthy();
  });
});
