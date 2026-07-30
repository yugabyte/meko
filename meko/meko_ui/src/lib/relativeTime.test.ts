import { describe, it, expect } from "vitest";
import { formatRelativeTime, formatUtcAbsolute } from "@/lib/relativeTime";

const NOW = Date.parse("2026-06-15T12:00:00Z");
const ago = (ms: number) => new Date(NOW - ms).toISOString();
const S = 1000;
const MIN = 60 * S;
const HR = 60 * MIN;
const DAY = 24 * HR;

describe("formatRelativeTime", () => {
  it("returns an em dash for missing or unparseable input", () => {
    expect(formatRelativeTime(undefined, NOW)).toBe("—");
    expect(formatRelativeTime(null, NOW)).toBe("—");
    expect(formatRelativeTime("not-a-date", NOW)).toBe("—");
  });

  it("buckets recent times", () => {
    expect(formatRelativeTime(ago(30 * S), NOW)).toBe("just now");
    expect(formatRelativeTime(ago(5 * MIN), NOW)).toBe("5m ago");
    expect(formatRelativeTime(ago(3 * HR), NOW)).toBe("3h ago");
    expect(formatRelativeTime(ago(2 * DAY), NOW)).toBe("2d ago");
  });

  it("buckets weeks, months, and years", () => {
    expect(formatRelativeTime(ago(10 * DAY), NOW)).toBe("1w ago");
    expect(formatRelativeTime(ago(45 * DAY), NOW)).toBe("1mo ago");
    expect(formatRelativeTime(ago(400 * DAY), NOW)).toBe("1y ago");
  });

  it("clamps a future timestamp to 'just now'", () => {
    expect(formatRelativeTime(ago(-5 * MIN), NOW)).toBe("just now");
  });
});

describe("formatUtcAbsolute", () => {
  it("renders a stable UTC wall-clock string", () => {
    expect(formatUtcAbsolute("2026-03-04T05:06:00Z")).toBe("2026-03-04 05:06 UTC");
  });
  it("returns an em dash for missing / invalid input", () => {
    expect(formatUtcAbsolute(undefined)).toBe("—");
    expect(formatUtcAbsolute("nope")).toBe("—");
  });
});
