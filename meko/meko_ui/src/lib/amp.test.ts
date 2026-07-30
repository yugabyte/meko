import { describe, it, expect, beforeEach } from "vitest";
import { isAmpEnabled } from "@/lib/amp";

beforeEach(() => {
  window.__MEKO_RUNTIME__ = {};
});

describe("isAmpEnabled", () => {
  it("is false by default (flag unset)", () => {
    expect(isAmpEnabled()).toBe(false);
  });

  it("is true only when SHOW_PG_AMP is exactly 'true' (case/space-insensitive)", () => {
    window.__MEKO_RUNTIME__ = { SHOW_PG_AMP: "  TRUE " };
    expect(isAmpEnabled()).toBe(true);
  });

  it("is false for other values", () => {
    window.__MEKO_RUNTIME__ = { SHOW_PG_AMP: "1" };
    expect(isAmpEnabled()).toBe(false);
    window.__MEKO_RUNTIME__ = { SHOW_PG_AMP: "false" };
    expect(isAmpEnabled()).toBe(false);
  });
});
