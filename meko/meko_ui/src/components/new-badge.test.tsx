// isNewSince: the "new" badge shows from the launch date for 20 days and never
// after, and an unparseable date never shows it.

import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { isNewSince, NewBadge } from "./new-badge";

afterEach(cleanup);

describe("isNewSince", () => {
  const since = "2026-10-09";

  it("is new on the launch date and through day 19", () => {
    expect(isNewSince(since, new Date("2026-10-09T00:00:00Z"))).toBe(true);
    expect(isNewSince(since, new Date("2026-10-28T23:59:59Z"))).toBe(true);
  });

  it("is not new from day 20 on", () => {
    expect(isNewSince(since, new Date("2026-10-29T00:00:00Z"))).toBe(false);
    expect(isNewSince(since, new Date("2027-01-01T00:00:00Z"))).toBe(false);
  });

  it("is not new before launch or for a bad date", () => {
    expect(isNewSince(since, new Date("2026-10-08T23:59:59Z"))).toBe(false);
    expect(isNewSince("not-a-date", new Date("2026-10-09T00:00:00Z"))).toBe(false);
  });

  it("honours a custom window", () => {
    expect(isNewSince(since, new Date("2026-10-16T00:00:00Z"), 7)).toBe(false);
  });
});

describe("NewBadge", () => {
  it("defaults the launch date to today, so it shows", () => {
    render(<NewBadge />);
    expect(screen.getByText("New")).toBeTruthy();
  });

  it("hides once the window has passed", () => {
    render(<NewBadge since="2020-01-01" />);
    expect(screen.queryByText("New")).toBeNull();
  });
});
