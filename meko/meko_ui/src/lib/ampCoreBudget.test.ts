import { describe, it, expect } from "vitest";
import { formatVcpuMetricLine } from "./ampCoreBudget";

const budget = (used: number, max: number | null) => ({
  tier: "free",
  next_tier: "standard",
  used_cores: used,
  max_cores: max,
  at_limit: max != null && used >= max,
});

describe("formatVcpuMetricLine", () => {
  it("formats used / max vCPU", () => {
    expect(formatVcpuMetricLine(budget(0.5, 0.5))).toBe("0.5 / 0.5 vCPU used");
    expect(formatVcpuMetricLine(budget(2, 2))).toBe("2 / 2 vCPU used");
  });

  it("rounds to milli precision", () => {
    expect(formatVcpuMetricLine(budget(0.4999999, 0.5))).toBe("0.5 / 0.5 vCPU used");
  });

  it("returns undefined when uncapped (max_cores null) or no budget", () => {
    expect(formatVcpuMetricLine(budget(9, null))).toBeUndefined();
    expect(formatVcpuMetricLine(undefined)).toBeUndefined();
  });
});
