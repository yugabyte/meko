// The memory row's "by:" line: shown with the author, left out when the API
// sends none (a viewer isn't told who added a memory).

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
});
