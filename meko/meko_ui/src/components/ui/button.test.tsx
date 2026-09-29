// The busy/done states exist so an action button looks the same everywhere.
// They were spelled out per screen before, and the copies had already drifted
// apart: one kept its primary colour when finished and another switched to
// outline, one showed a tick and another only changed its text.

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";

import { Button } from "./button";

afterEach(cleanup);

describe("Button", () => {
  it("renders its children when idle", () => {
    render(<Button>Run review</Button>);
    const button = screen.getByRole("button", { name: "Run review" });
    expect(button.hasAttribute("disabled")).toBe(false);
  });

  it("swaps the label and disables itself while busy", () => {
    const onClick = vi.fn();
    render(
      <Button busy busyLabel="Reviewing" onClick={onClick}>
        Run review
      </Button>,
    );
    const button = screen.getByRole("button", { name: /reviewing/i });
    expect(button.hasAttribute("disabled")).toBe(true);
    fireEvent.click(button);
    expect(onClick).not.toHaveBeenCalled();
  });

  it("swaps the label and disables itself when done", () => {
    render(
      <Button done doneLabel="Review complete">
        Run review
      </Button>,
    );
    const button = screen.getByRole("button", { name: /review complete/i });
    expect(button.hasAttribute("disabled")).toBe(true);
    expect(screen.queryByText("Run review")).toBeNull();
  });

  // The finished state is still the same button, so it keeps whatever
  // variant the caller chose. A screen that quietly switched to outline on
  // completion was the inconsistency this replaced.
  it("keeps its own variant when done", () => {
    const { container } = render(<Button done>Added</Button>);
    expect(container.querySelector("button")!.className).toContain("bg-primary");
  });

  it("falls back to the children when no state label is given", () => {
    render(<Button busy>Sharing</Button>);
    expect(screen.getByRole("button", { name: /sharing/i })).toBeTruthy();
  });

  it("stays disabled when the caller disables it for its own reasons", () => {
    render(<Button disabled>Run review</Button>);
    expect(screen.getByRole("button").hasAttribute("disabled")).toBe(true);
  });

  // Slot takes exactly one child, so decorating it would break every
  // asChild caller (the collapsible triggers, the links styled as buttons).
  it("leaves an asChild caller's content alone", () => {
    render(
      <Button asChild done doneLabel="ignored">
        <a href="/somewhere">Open</a>
      </Button>,
    );
    expect(screen.getByRole("link", { name: "Open" })).toBeTruthy();
  });
});
