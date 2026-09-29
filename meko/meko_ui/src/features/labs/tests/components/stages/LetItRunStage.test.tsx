// The lesson's whole argument is the split: most of the batch closes itself,
// and the one that stops is the destructive change. Both halves are pinned
// here, because a fixture edit that auto-approved the dropped table would
// contradict Chapter 2's guardrail lesson without breaking anything else.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";

import { BATCH_ROWS } from "@/features/labs/data/adlcReviewAgentLab/batchRun";
import { LetItRunStage } from "@/features/labs/components/stages/LetItRunStage";

beforeEach(() => vi.useFakeTimers());
afterEach(() => {
  vi.useRealTimers();
  cleanup();
});

/** Walks the staggered reveal to the end. Each row is scheduled by the
 *  effect that the previous row's state update ran, so the timers have to be
 *  advanced one step at a time for React to commit in between. */
function runBatch() {
  fireEvent.click(screen.getByRole("button", { name: /run the batch/i }));
  for (let i = 0; i <= BATCH_ROWS.length; i += 1) {
    act(() => {
      vi.advanceTimersByTime(1_000);
    });
  }
}

describe("LetItRunStage", () => {
  // An empty bordered box before the run reads as a broken page. The queue
  // is the thing being shown; only its outcomes are withheld.
  it("shows the queue before the run, with no outcomes on it yet", () => {
    render(<LetItRunStage />);
    for (const row of BATCH_ROWS) {
      expect(screen.getByText(row.title)).toBeTruthy();
    }
    expect(screen.getAllByText("queued")).toHaveLength(BATCH_ROWS.length);
    expect(screen.queryByText(/auto-approved/)).toBeNull();
  });

  it("holds the summary back until the batch has actually run", () => {
    render(<LetItRunStage />);
    expect(screen.queryByText("closed without you")).toBeNull();
    expect(screen.queryByText(/shape of the win/i)).toBeNull();
  });

  it("reports the split once the batch completes", () => {
    render(<LetItRunStage />);
    runBatch();

    expect(screen.getByRole("button", { name: /batch complete/i })).toBeTruthy();
    expect(screen.getByText("closed without you")).toBeTruthy();
    expect(screen.getByText("came back for a human")).toBeTruthy();
    expect(screen.getByText(/shape of the win/i)).toBeTruthy();
  });

  // The counts are derived from the rows, so this catches a fixture edit that
  // changes the argument the stat row is making.
  it("stops on exactly one pull request, and it is the destructive one", () => {
    const blocking = BATCH_ROWS.filter((row) => row.outcome.kind === "blocking");
    expect(blocking).toHaveLength(1);
    expect(blocking[0].title).toMatch(/drop/i);

    render(<LetItRunStage />);
    runBatch();
    expect(screen.getByText(`#${blocking[0].pr}`)).toBeTruthy();
    expect(screen.getByText(/blocking · waiting on you/)).toBeTruthy();
    // The banner splits the number into its own node, so match the clause
    // around it rather than the whole sentence.
    expect(screen.getByText(/is waiting because dropping a table/)).toBeTruthy();
  });

  // It is a fixture, and a page that shows six plausible pull requests
  // without saying so is claiming a run it did not make.
  it("says the batch is an illustration", () => {
    render(<LetItRunStage />);
    expect(screen.getByText(/an illustration, not a run against your repository/i)).toBeTruthy();
  });
});
