import { describe, it, expect } from "vitest";
import {
  formatLimitNumeric,
  formatStorageLimit,
  formatDays,
  compactNumber,
} from "@/features/billing/lib/format";

describe("compactNumber", () => {
  it("passes sub-1000 through unchanged", () => {
    expect(compactNumber(0)).toBe("0");
    expect(compactNumber(999)).toBe("999");
  });
  it("abbreviates thousands (K branch rounds to a whole number)", () => {
    expect(compactNumber(1000)).toBe("1K");
    // NB: the K branch uses toFixed(0), so 1500 → "2K" (rounded), not "1.5K".
    expect(compactNumber(1500)).toBe("2K");
  });
  it("abbreviates millions with one decimal", () => {
    expect(compactNumber(1_000_000)).toBe("1M");
    expect(compactNumber(1_500_000)).toBe("1.5M");
  });
});

describe("formatLimitNumeric", () => {
  it("maps the sentinels", () => {
    expect(formatLimitNumeric("custom")).toBe("Custom");
    expect(formatLimitNumeric("unlimited")).toBe("Unlimited");
  });
  it("compacts a count and appends /mo only when asked", () => {
    expect(formatLimitNumeric(1000)).toBe("1K");
    expect(formatLimitNumeric(1000, { perMonth: true })).toBe("1K/mo");
  });
});

describe("formatStorageLimit", () => {
  it("maps the sentinels", () => {
    expect(formatStorageLimit("custom")).toBe("Custom");
    expect(formatStorageLimit("unlimited")).toBe("Unlimited");
  });
  it("keeps MB under 1 GB and converts upward", () => {
    expect(formatStorageLimit(512)).toBe("512 MB");
    expect(formatStorageLimit(1024)).toBe("1 GB");
    expect(formatStorageLimit(1536)).toBe("1.5 GB");
    expect(formatStorageLimit(2048)).toBe("2 GB");
  });
});

describe("formatDays", () => {
  it("maps the sentinels", () => {
    expect(formatDays("custom")).toBe("Configurable");
    expect(formatDays("unlimited")).toBe("Unlimited");
  });
  it("renders a day count", () => {
    expect(formatDays(7)).toBe("7 days");
  });
  it("special-cases 1 day to '24 hours' only when hoursWhenOne is set", () => {
    expect(formatDays(1, { hoursWhenOne: true })).toBe("24 hours");
    expect(formatDays(1)).toBe("1 days");
  });
});
