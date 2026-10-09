// Unit tests for resolveGuardRedirect, mostly focused on the return_to
// round-trip (MEKO-737: a logged-out user hitting a deep-link should
// land on that deep-link after sign-in, not on the generic home page).
import { describe, expect, it } from "vitest";
import { resolveGuardRedirect } from "./guards";

const DEFAULT = {
  authLoading: false,
  authenticated: false,
  hasCompletedOnboarding: false,
};

describe("resolveGuardRedirect — return_to round-trip", () => {
  it("sends unauthenticated protected-route visits to /signin with the deep-link", () => {
    const redirect = resolveGuardRedirect({
      ...DEFAULT,
      location: "/datapacks/3a2691ed-2afc-4259-bf50-63d9be9b4eaf",
    });
    expect(redirect).toBe("/signin?return_to=%2Fdatapacks%2F3a2691ed-2afc-4259-bf50-63d9be9b4eaf");
  });

  it("preserves the deep-link's own query string in return_to", () => {
    const redirect = resolveGuardRedirect({
      ...DEFAULT,
      location: "/datapacks/abc",
      search: "?connect=1",
    });
    expect(redirect).toBe("/signin?return_to=%2Fdatapacks%2Fabc%3Fconnect%3D1");
  });

  it("does NOT redirect to /login (that route is itself a <Navigate> that drops the query)", () => {
    const redirect = resolveGuardRedirect({
      ...DEFAULT,
      location: "/datapacks/abc",
    });
    expect(redirect).not.toContain("/login");
    expect(redirect).toContain("/signin?");
  });

  it("returns null when the user is on a public route", () => {
    const redirect = resolveGuardRedirect({ ...DEFAULT, location: "/signin" });
    expect(redirect).toBeNull();
  });

  it("honours an already-safe ?return_to= on auth routes for an authed user", () => {
    const redirect = resolveGuardRedirect({
      ...DEFAULT,
      authenticated: true,
      hasCompletedOnboarding: true,
      location: "/signup",
      returnTo: "/marketplace/complete",
    });
    expect(redirect).toBe("/marketplace/complete");
  });

  it("falls back to the default landing when ?return_to= is unsafe", () => {
    const redirect = resolveGuardRedirect({
      ...DEFAULT,
      authenticated: true,
      hasCompletedOnboarding: true,
      location: "/signup",
      returnTo: "https://evil.example.com/pwn",
    });
    expect(redirect).toBe("/datapacks");
  });
});
