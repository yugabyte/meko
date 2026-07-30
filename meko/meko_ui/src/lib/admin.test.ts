import { describe, it, expect, beforeEach } from "vitest";
import { isAdminViewEnabled, ADMIN_VIEW_LOCAL_KEY } from "@/lib/admin";

// isAdminViewEnabled reads the runtime flag + localStorage at call time, so we
// set both per test (jsdom provides window + localStorage).
beforeEach(() => {
  window.__MEKO_RUNTIME__ = {};
  localStorage.clear();
});

describe("isAdminViewEnabled", () => {
  it("is false when the runtime flag is off, regardless of localStorage", () => {
    localStorage.setItem(ADMIN_VIEW_LOCAL_KEY, "true");
    expect(isAdminViewEnabled()).toBe(false);
  });

  it("is false when the flag is on but the localStorage opt-in is absent", () => {
    window.__MEKO_RUNTIME__ = { SHOW_ADMIN_VIEW: "true" };
    expect(isAdminViewEnabled()).toBe(false);
  });

  it("requires BOTH the flag AND the localStorage opt-in", () => {
    window.__MEKO_RUNTIME__ = { SHOW_ADMIN_VIEW: "true" };
    localStorage.setItem(ADMIN_VIEW_LOCAL_KEY, "true");
    expect(isAdminViewEnabled()).toBe(true);
  });

  it("is false when the localStorage opt-in is any non-'true' value", () => {
    window.__MEKO_RUNTIME__ = { SHOW_ADMIN_VIEW: "true" };
    localStorage.setItem(ADMIN_VIEW_LOCAL_KEY, "false");
    expect(isAdminViewEnabled()).toBe(false);
  });
});
