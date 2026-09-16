import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, cleanup, fireEvent } from "@testing-library/react";

import { ErrorCell } from "./TasksPage";

// The error an operator actually has to get out of the page: long, and with the
// cause buried in the middle of it (#635).
const ERROR =
  "deploy endpoint: pod yb-ep-6cd219ec-7d9 could not be scheduled: 0/14 nodes are available: " +
  "1 node(s) were unschedulable, 13 Insufficient ephemeral-storage.: " +
  "release yb-ep-6cd219ec did not become ready within 10m0s (helm --wait expired): " +
  "helm upgrade --install yb-ep-6cd219ec: exit status 1: Error: context deadline exceeded";

afterEach(cleanup);

describe("ErrorCell", () => {
  it("starts collapsed and expands the full text on click", () => {
    render(<ErrorCell error={ERROR} />);

    const toggle = screen.getByRole("button", { name: "Expand error" });
    expect(toggle.getAttribute("aria-expanded")).toBe("false");

    fireEvent.click(toggle);

    expect(
      screen.getByRole("button", { name: "Collapse error" }).getAttribute("aria-expanded"),
    ).toBe("true");
    // Expanded, the error is ordinary wrapped DOM text — selectable, not a
    // hover card that closes the moment the pointer leaves.
    expect(screen.getByText(ERROR).className).toContain("whitespace-pre-wrap");
  });

  it("copies the whole error in one action", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText } });

    render(<ErrorCell error={ERROR} />);
    fireEvent.click(screen.getByRole("button", { name: "Copy error" }));

    expect(writeText).toHaveBeenCalledWith(ERROR);
    vi.unstubAllGlobals();
  });
});
