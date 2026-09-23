// Covers the three-state latch in useFeatureFlag: the reason ?flag=false has
// to persist "false" rather than clear the key, which is what lets a flag
// default to ON from a deployment gate and still be switched off per browser.

import { afterEach, describe, expect, it } from "vitest";
import type { ReactNode } from "react";
import { renderHook } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { FEATURE_FLAGS } from "@/lib/featureFlags";
import { useFeatureFlag } from "@/lib/useFeatureFlag";

const STORAGE_KEY = FEATURE_FLAGS.collections.storageKey;

afterEach(() => localStorage.clear());

/** Renders the hook at `url` so the ?collections= param is in scope. */
function renderAt(url: string, defaultValue?: boolean) {
  const wrapper = ({ children }: { children: ReactNode }) => (
    <MemoryRouter initialEntries={[url]}>{children}</MemoryRouter>
  );
  return renderHook(() => useFeatureFlag("collections", defaultValue), { wrapper });
}

describe("useFeatureFlag — default with no stored choice", () => {
  it("is off by default, preserving the pre-existing contract", () => {
    expect(renderAt("/").result.current).toBe(false);
  });

  it("follows a true default when the user has made no choice", () => {
    expect(renderAt("/", true).result.current).toBe(true);
  });
});

describe("useFeatureFlag — the query param overrides the default both ways", () => {
  it("?collections=true turns on a flag whose default is off", () => {
    expect(renderAt("/?collections=true", false).result.current).toBe(true);
  });

  it("?collections=false turns off a flag whose default is on", () => {
    expect(renderAt("/?collections=false", true).result.current).toBe(false);
  });
});

describe("useFeatureFlag — the choice outlives the URL", () => {
  it("persists an opt-out so a default-on flag stays off on the next visit", () => {
    renderAt("/?collections=false", true);
    expect(localStorage.getItem(STORAGE_KEY)).toBe("false");
    // Navigating away from the ?collections= URL must not re-enable it.
    expect(renderAt("/", true).result.current).toBe(false);
  });

  it("persists an opt-in so a default-off flag stays on on the next visit", () => {
    renderAt("/?collections=true", false);
    expect(localStorage.getItem(STORAGE_KEY)).toBe("true");
    expect(renderAt("/", false).result.current).toBe(true);
  });

  it("lets a stored choice be reversed by the opposite param", () => {
    renderAt("/?collections=true", false);
    expect(renderAt("/?collections=false", false).result.current).toBe(false);
    expect(localStorage.getItem(STORAGE_KEY)).toBe("false");
  });
});
