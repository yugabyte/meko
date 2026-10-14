// A share-link URL carries a token that lets anyone join the datapack, and the SDK
// attaches the page URL to every call, so nothing is sent from a /share/ page.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const sdk = { load: vi.fn(), track: vi.fn(), identify: vi.fn(), page: vi.fn() };
vi.mock("@rudderstack/analytics-js", () => ({
  RudderAnalytics: class {
    load = sdk.load;
    track = sdk.track;
    identify = sdk.identify;
    page = sdk.page;
  },
}));

async function loaded() {
  vi.resetModules();
  (window as unknown as { __MEKO_RUNTIME__: Record<string, string> }).__MEKO_RUNTIME__ = {
    VITE_RUDDERSTACK_WRITE_KEY: "wk",
  };
  const mod = await import("@/lib/telemetry/rudderstack");
  mod.loadTelemetry();
  expect(sdk.load).toHaveBeenCalled();
  return mod;
}

beforeEach(() => vi.clearAllMocks());
afterEach(() => {
  window.history.replaceState(null, "", "/");
  delete (window as unknown as { __MEKO_RUNTIME__?: unknown }).__MEKO_RUNTIME__;
});

describe("telemetry on a share-link page", () => {
  it("sends nothing, but still runs the event callback", async () => {
    const t = await loaded();
    window.history.replaceState(null, "", "/share/secret-tok");
    const done = vi.fn();
    t.trackEvent("user_signed_in" as never, {}, done);
    t.identifyUser({ email: "a@b.c" });
    t.trackPageView("/share/secret-tok");
    expect(sdk.track).not.toHaveBeenCalled();
    expect(sdk.identify).not.toHaveBeenCalled();
    expect(sdk.page).not.toHaveBeenCalled();
    expect(done).toHaveBeenCalledTimes(1);
  });

  it("sends as usual on any other page", async () => {
    const t = await loaded();
    window.history.replaceState(null, "", "/datapacks");
    t.trackPageView("/datapacks");
    t.trackEvent("user_signed_in" as never);
    expect(sdk.page).toHaveBeenCalledTimes(1);
    expect(sdk.track).toHaveBeenCalledTimes(1);
  });
});
