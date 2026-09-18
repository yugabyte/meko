// The flag decides whether a product surface exists for a deployment, so what
// is pinned here is the parsing — an unset or unrecognised value must not
// leave Labs reachable.

import { afterEach, describe, expect, it } from "vitest";

import { isLabsEnabled, isLabsPath } from "./labs";

declare global {
  interface Window {
    __MEKO_RUNTIME__?: Record<string, string>;
  }
}

afterEach(() => {
  delete window.__MEKO_RUNTIME__;
});

function withEnv(value: string | undefined) {
  window.__MEKO_RUNTIME__ = value === undefined ? {} : { MEKO_ENABLE_LABS: value };
}

describe("isLabsEnabled", () => {
  it("is off when the deployment never set it", () => {
    withEnv(undefined);
    expect(isLabsEnabled()).toBe(false);
  });

  it("accepts the spellings the BE and MCP server accept", () => {
    for (const on of ["true", "TRUE", " true ", "1", "yes"]) {
      withEnv(on);
      expect(isLabsEnabled(), on).toBe(true);
    }
  });

  it("is off for false and for anything it does not recognise", () => {
    for (const off of ["false", "0", "no", "", "maybe"]) {
      withEnv(off);
      expect(isLabsEnabled(), off).toBe(false);
    }
  });
});

describe("isLabsPath", () => {
  // The runner is what the flag most has to reach: it early-returns in
  // ProtectedRoutes and takes over the whole viewport, so a miss here would
  // leave a disabled deployment fully usable through a shared link.
  it("covers the list, a lab, and the runner", () => {
    expect(isLabsPath("/labs")).toBe(true);
    expect(isLabsPath("/labs/adlc-review-agent")).toBe(true);
    expect(isLabsPath("/labs/adlc-review-agent/run")).toBe(true);
  });

  it("does not catch another route that merely starts with the same letters", () => {
    expect(isLabsPath("/labsomething")).toBe(false);
    expect(isLabsPath("/datapacks")).toBe(false);
    expect(isLabsPath("/")).toBe(false);
  });
});
