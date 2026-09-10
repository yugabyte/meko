import { describe, it, expect } from "vitest";
import {
  DEFAULT_CPU,
  coresFromCpu,
  defaultCpuForBudget,
  minMemoryForCpu,
  resizeToast,
} from "./resize";

// The resize outcome → toast mapping is the one bit of resize UX with branching
// logic: only an "infeasible" outcome is an error the user must notice; the
// others are informational. Lock that, and that every message names the target
// size (so the toast is actionable).
describe("resizeToast", () => {
  it("flags only 'infeasible' as a destructive toast", () => {
    expect(resizeToast("infeasible", "4 CPU / 8Gi").variant).toBe("destructive");
    for (const outcome of ["completed", "pending", "unsupported"] as const) {
      expect(resizeToast(outcome, "4 CPU / 8Gi").variant).toBeUndefined();
    }
  });

  it("names the target size in every outcome's message", () => {
    for (const outcome of ["completed", "pending", "unsupported", "infeasible"] as const) {
      expect(resizeToast(outcome, "4 CPU / 8Gi").title).toContain("4 CPU / 8Gi");
    }
  });
});

// The compute-sizing helpers back the wizard/add-compute defaults + validation,
// so lock their edges: quantity parsing, the 8 GiB-per-vCPU memory floor, and
// the tier-budget default that keeps a gated user within their cap.
describe("coresFromCpu", () => {
  it("parses plain, fractional, and milli quantities", () => {
    expect(coresFromCpu("2")).toBe(2);
    expect(coresFromCpu("0.5")).toBe(0.5);
    expect(coresFromCpu("500m")).toBe(0.5);
    expect(coresFromCpu("1500m")).toBe(1.5);
  });
  it("returns NaN for empty/garbage", () => {
    expect(coresFromCpu("")).toBeNaN();
    expect(coresFromCpu("abc")).toBeNaN();
  });
});

describe("minMemoryForCpu", () => {
  it("floors memory at 8 GiB per vCPU (whole GiB)", () => {
    expect(minMemoryForCpu("0.5")).toBe("4Gi");
    expect(minMemoryForCpu("1")).toBe("8Gi");
    expect(minMemoryForCpu("2")).toBe("16Gi");
    expect(minMemoryForCpu("500m")).toBe("4Gi");
  });
  it("returns '' for an unparseable CPU", () => {
    expect(minMemoryForCpu("")).toBe("");
    expect(minMemoryForCpu("nope")).toBe("");
  });
});

describe("defaultCpuForBudget", () => {
  it("defaults to 0.5 vCPU when unlimited/unknown or there's ample budget", () => {
    expect(defaultCpuForBudget(null)).toBe(DEFAULT_CPU); // "0.5"
    expect(defaultCpuForBudget(5)).toBe("0.5");
    expect(defaultCpuForBudget(1)).toBe("0.5");
  });
  it("caps the default at the remaining budget when it's under 0.5", () => {
    expect(defaultCpuForBudget(0.3)).toBe("0.3");
  });
  it("falls back to the default at/under the cap (button is greyed there)", () => {
    expect(defaultCpuForBudget(0)).toBe(DEFAULT_CPU);
  });
});
