import { describe, it, expect } from "vitest";
import { resizeToast } from "./resize";

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
