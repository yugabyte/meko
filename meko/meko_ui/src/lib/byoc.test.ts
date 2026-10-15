// The flag decides whether a deployment shows prices it does not charge, so
// what is pinned here is the parsing — an unset or unrecognised value must
// leave the hosted behaviour (pricing visible) intact.

import { afterEach, describe, expect, it } from "vitest";

import { isByocEnabled, isWorkbenchLmEnabled } from "./byoc";

declare global {
  interface Window {
    __MEKO_RUNTIME__?: Record<string, string>;
  }
}

afterEach(() => {
  delete window.__MEKO_RUNTIME__;
});

function withEnv(value: string | undefined) {
  window.__MEKO_RUNTIME__ = value === undefined ? {} : { MEKO_ENABLE_BYOC: value };
}

describe("isByocEnabled", () => {
  it("is off when the deployment never set it", () => {
    withEnv(undefined);
    expect(isByocEnabled()).toBe(false);
  });

  it("accepts the spellings the BE and MCP server accept", () => {
    for (const on of ["true", "TRUE", " true ", "1", "yes"]) {
      withEnv(on);
      expect(isByocEnabled(), on).toBe(true);
    }
  });

  it("is off for false and for anything it does not recognise", () => {
    for (const off of ["false", "0", "no", "", "maybe"]) {
      withEnv(off);
      expect(isByocEnabled(), off).toBe(false);
    }
  });
});

describe("isWorkbenchLmEnabled", () => {
  it("keeps WorkbenchLM on a hosted deployment", () => {
    withEnv(undefined);
    expect(isWorkbenchLmEnabled()).toBe(true);
    withEnv("false");
    expect(isWorkbenchLmEnabled()).toBe(true);
  });

  it("hides WorkbenchLM on a BYOC data plane", () => {
    withEnv("true");
    expect(isWorkbenchLmEnabled()).toBe(false);
  });
});
