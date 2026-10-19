// Covers the author line (shown with an author, left out without one) and
// that MEKO-824's markdown rendering is wired up so bullet lists / line
// breaks / links come through, while a bare plain-text memory still works.

import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRowBody } from "./memoryRowBody";

afterEach(cleanup);

describe("MemoryRowBody", () => {
  it("names the author when there is one", () => {
    render(<MemoryRowBody memory={{ id: "m1", data: "a fact", user_email: "a@x.com" }} />);
    expect(screen.getByText("a@x.com")).toBeTruthy();
  });

  it("has no by: line without an author", () => {
    render(<MemoryRowBody memory={{ id: "m1", data: "a fact" }} />);
    expect(screen.getByText("a fact")).toBeTruthy();
    expect(screen.queryByText(/by:/)).toBeNull();
  });

  it("renders markdown bullet lists as actual <ul>/<li>", () => {
    const data =
      "AMP functional test plan, P1 items:\n- #7 settings changed\n- #8 pod cannot start\n- #9 CA bundle";
    const { container } = render(<MemoryRowBody memory={{ id: "m2", data }} />);
    const items = container.querySelectorAll("ul li");
    expect(items.length).toBe(3);
    expect(items[0].textContent).toContain("#7");
  });

  it("linkifies full URLs embedded in memory text (GFM autolink)", () => {
    const data = "see https://github.com/yugabyte/meko/issues/976 for context";
    const { container } = render(<MemoryRowBody memory={{ id: "m3", data }} />);
    const link = container.querySelector("a");
    expect(link?.getAttribute("href")).toBe("https://github.com/yugabyte/meko/issues/976");
  });

  it("renders a plain-text memory as a single paragraph (no breakage without markdown)", () => {
    const { container } = render(
      <MemoryRowBody memory={{ id: "m4", data: "just a plain fact" }} />,
    );
    expect(screen.getByText("just a plain fact")).toBeTruthy();
    // Pin the single-<p> contract so a future renderer regression (e.g. react-markdown
    // splitting plain text across multiple blocks) would fail the suite instead of silently
    // changing layout.
    expect(container.querySelectorAll("p").length).toBe(1);
  });

  it("shows the (empty) placeholder when data is missing", () => {
    render(<MemoryRowBody memory={{ id: "m5", data: "" }} />);
    expect(screen.getByText("(empty)")).toBeTruthy();
  });
});
