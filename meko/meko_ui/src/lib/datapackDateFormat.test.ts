// The datapack header's created line, which is a date and deliberately not a
// time. Pinned because the format is the product decision here, not an
// implementation detail: a stray time of day is exactly what these assertions
// exist to catch.

import { describe, expect, it } from "vitest";
import { formatDatapackCreatedAtUtc, formatDatapackCreatedDayUtc } from "@/lib/datapackDateFormat";

describe("formatDatapackCreatedDayUtc", () => {
  it("is a weekday and a date, with no time of day", () => {
    expect(formatDatapackCreatedDayUtc("2026-07-06T17:22:00Z")).toBe("Mon, Jul 6, 2026");
  });

  it("reads the timestamp in UTC, not the viewer's zone", () => {
    // 23:30 UTC is already the 7th in Asia, and still the 6th in the Americas.
    // Everyone sees the UTC day, so the string cannot vary by who is looking.
    expect(formatDatapackCreatedDayUtc("2026-07-06T23:30:00Z")).toBe("Mon, Jul 6, 2026");
  });

  it("renders a dash for a missing or unparseable timestamp", () => {
    expect(formatDatapackCreatedDayUtc(undefined)).toBe("—");
    expect(formatDatapackCreatedDayUtc("")).toBe("—");
    expect(formatDatapackCreatedDayUtc("not a date")).toBe("—");
  });

  it("leaves the with-time formatter alone", () => {
    // Tokens and conversations still need the minute, so the two must not
    // collapse into one.
    expect(formatDatapackCreatedAtUtc("2026-07-06T17:22:00Z")).toContain("5:22 PM UTC");
  });
});
