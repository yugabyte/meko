// Boundary + charset coverage for the cloud_admin password helper.
import { describe, it, expect } from "vitest";
import {
  AMP_PASSWORD_MIN,
  AMP_PASSWORD_MAX,
  generateAmpPassword,
  validateAmpPassword,
} from "./ampPassword";

describe("generateAmpPassword", () => {
  it("produces a printable-ASCII password within the controller's bounds", () => {
    for (let i = 0; i < 50; i++) {
      const pw = generateAmpPassword();
      expect(pw.length).toBeGreaterThanOrEqual(AMP_PASSWORD_MIN);
      expect(pw.length).toBeLessThanOrEqual(AMP_PASSWORD_MAX);
      expect([...pw].every((ch) => ch.codePointAt(0)! >= 0x20 && ch.codePointAt(0)! <= 0x7e)).toBe(
        true,
      );
      expect(validateAmpPassword(pw)).toBeNull();
    }
  });

  it("is random between calls", () => {
    expect(generateAmpPassword()).not.toBe(generateAmpPassword());
  });
});

describe("validateAmpPassword", () => {
  it("accepts an in-range printable-ASCII password, including spaces", () => {
    expect(validateAmpPassword("a b c d e")).toBeNull(); // 9 chars incl. spaces
    expect(validateAmpPassword("x".repeat(AMP_PASSWORD_MIN))).toBeNull();
    expect(validateAmpPassword("x".repeat(AMP_PASSWORD_MAX))).toBeNull();
  });

  it("rejects out-of-range lengths", () => {
    expect(validateAmpPassword("x".repeat(AMP_PASSWORD_MIN - 1))).toMatch(/characters/);
    expect(validateAmpPassword("x".repeat(AMP_PASSWORD_MAX + 1))).toMatch(/characters/);
    expect(validateAmpPassword("")).toMatch(/characters/);
  });

  it("rejects control and non-ASCII characters", () => {
    expect(validateAmpPassword("pass\tword")).toMatch(/printable ASCII/);
    expect(validateAmpPassword("pÄssword")).toMatch(/printable ASCII/);
  });
});
