import { afterEach, describe, expect, it } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";

import { useLabProgress } from "../../hooks/useLabProgress";
import type { LabChapter, LabStage } from "../../types";
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
    id: "knowledge-base",
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

const ONE_CHAPTER: LabChapter[] = [
  { id: "chapter-1", title: "Chapter one", summary: "", stages: FIXTURE_STAGES },
];

function TestHarness({ chapters }: { chapters: LabChapter[] }) {
  const progress = useLabProgress({ id: "test-lab", chapters } as never);
  return <LabStageNav chapters={chapters} progress={progress} />;
}

describe("LabStageNav — single chapter (today's only lab)", () => {
  it("always shows the chapter header, and every stage's numbered heading", () => {
    render(<TestHarness chapters={ONE_CHAPTER} />);
    expect(screen.getByText("Chapter 1:")).toBeTruthy();
    expect(screen.getByText("Chapter one")).toBeTruthy();
    expect(screen.getByText("Step 1. Create a datapack")).toBeTruthy();
    expect(screen.getByText("Step 2. Seed context")).toBeTruthy();
    expect(screen.getByText("Step 3. Run a review")).toBeTruthy();
  });

  it("only the current stage's card shows its body and a Next button", () => {
    render(<TestHarness chapters={ONE_CHAPTER} />);
    expect(screen.getByText("Pick one.")).toBeTruthy();
    expect(screen.queryByText("Upload or chat.")).toBeNull();
    expect(screen.getByRole("button", { name: "Next" })).toBeTruthy();
  });

  it("clicking a collapsed stage card jumps directly to it", () => {
    render(<TestHarness chapters={ONE_CHAPTER} />);
    fireEvent.click(screen.getByText("Step 3. Run a review"));
    expect(screen.getByText("Review a diff.")).toBeTruthy();
  });

  it("clicking Next inside the active card advances one stage, and reads Done + disables on the last one", () => {
    render(<TestHarness chapters={ONE_CHAPTER} />);
    fireEvent.click(screen.getByRole("button", { name: "Next" }));
    expect(screen.getByText("Upload or chat.")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Next" }));
    const doneButton = screen.getByRole("button", { name: "Done" }) as HTMLButtonElement;
    expect(doneButton.disabled).toBe(true);
  });

  it("with only one chapter, there is no Previous/Next Chapter control", () => {
    render(<TestHarness chapters={ONE_CHAPTER} />);
    expect(screen.queryByRole("button", { name: /previous chapter/i })).toBeNull();
    expect(screen.queryByRole("button", { name: /next chapter/i })).toBeNull();
  });
});

const TWO_CHAPTERS: LabChapter[] = [
  {
    id: "setup",
    title: "Setup your context",
    summary: "",
    stages: [FIXTURE_STAGES[0], FIXTURE_STAGES[1]],
  },
  {
    id: "review",
    title: "Review cycle",
    summary: "",
    stages: [FIXTURE_STAGES[2]],
  },
];

describe("LabStageNav — multiple chapters", () => {
  it("shows the current chapter's header and only its own steps, not the other chapter's", () => {
    render(<TestHarness chapters={TWO_CHAPTERS} />);
    expect(screen.getByText("Chapter 1:")).toBeTruthy();
    expect(screen.getByText("Setup your context")).toBeTruthy();
    expect(screen.getByText("Step 1. Create a datapack")).toBeTruthy();
    expect(screen.getByText("Step 2. Seed context")).toBeTruthy();
    // The other chapter isn't shown at all -- not its title, not its steps.
    expect(screen.queryByText("Review cycle")).toBeNull();
    expect(screen.queryByText(/Run a review/)).toBeNull();
  });

  it("the last step's button reads Done and stays disabled, without silently crossing into the next chapter", () => {
    render(<TestHarness chapters={TWO_CHAPTERS} />);
    fireEvent.click(screen.getByRole("button", { name: "Next" })); // create-datapack -> knowledge-base
    const doneButton = screen.getByRole("button", { name: "Done" }) as HTMLButtonElement;
    expect(doneButton.disabled).toBe(true);
    // Still on chapter 1 -- the other chapter's step never appeared.
    expect(screen.queryByText(/Run a review/)).toBeNull();
  });

  it("Next Chapter jumps to the next chapter's first stage, and step numbering restarts at 1", () => {
    render(<TestHarness chapters={TWO_CHAPTERS} />);
    fireEvent.click(screen.getByRole("button", { name: /next chapter/i }));
    expect(screen.getByText("Chapter 2:")).toBeTruthy();
    expect(screen.getByText("Review cycle")).toBeTruthy();
    expect(screen.getByText("Step 1. Run a review")).toBeTruthy();
    expect(screen.getByText("Review a diff.")).toBeTruthy();
    // No further chapter to advance to.
    expect(screen.queryByRole("button", { name: /next chapter/i })).toBeNull();
  });

  it("Previous chapter appears once past chapter 1, and jumps back to its first stage", () => {
    render(<TestHarness chapters={TWO_CHAPTERS} />);
    expect(screen.queryByRole("button", { name: /previous chapter/i })).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: /next chapter/i }));
    fireEvent.click(screen.getByRole("button", { name: /previous chapter/i }));

    expect(screen.getByText("Chapter 1:")).toBeTruthy();
    expect(screen.getByText("Step 1. Create a datapack")).toBeTruthy();
  });
});
