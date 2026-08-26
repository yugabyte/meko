import { describe, it, expect } from "vitest";
import { isValidMemory } from "./compute";

// A memory quantity with no unit is a byte count, so "4" reserves four bytes
// rather than 4Gi and the endpoint it sizes can never be resized in place
// afterwards (meko#492). The controller rejects the same set with a 400, so this
// table has to stay in step with `memoryQuantityRE` in the controller's
// internal/api/handlers.go.
describe("isValidMemory", () => {
  it("accepts a quantity with an explicit binary or decimal-SI unit", () => {
    for (const v of ["4Gi", "512Mi", "1500M", "1.5Gi", "2Ti", " 8Gi "]) {
      expect(isValidMemory(v), v).toBe(true);
    }
  });

  it("rejects a quantity that is not a real memory reservation", () => {
    // In order: bare byte count, milli (a fraction of a byte), kilobytes, the
    // exponent form, an empty field, and quantities Kubernetes itself refuses.
    for (const v of ["4", "4m", "512Ki", "4k", "1e9", "", "4Gii", "4GB", "4gi", "Gi"]) {
      expect(isValidMemory(v), v).toBe(false);
    }
  });
});
