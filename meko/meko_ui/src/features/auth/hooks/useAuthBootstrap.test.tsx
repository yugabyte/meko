// consumeWelcomeParam is the FE half of the sign-up detection contract:
// api_server appends ?welcome=1 on JIT-provisioned OAuth callbacks, and this
// strips it so a refresh doesn't re-fire user_signed_up. The hook tests
// below also pin how welcome routes event firing — the regression surface
// AgentK caught twice (OIDC bypass + email/social session parity).
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, waitFor } from "@testing-library/react";
import { consumeWelcomeParam, useAuthBootstrap } from "@/features/auth/hooks/useAuthBootstrap";

vi.mock("@/lib/api/mekoApi", async (o) => ({
  ...(await o<typeof import("@/lib/api/mekoApi")>()),
  getCurrentUser: vi.fn(),
}));
vi.mock("@/lib/telemetry/rudderstack", () => ({ identifyThenTrack: vi.fn() }));

import { getCurrentUser } from "@/lib/api/mekoApi";
import { identifyThenTrack } from "@/lib/telemetry/rudderstack";
import { setPendingLoginMethod } from "@/features/auth/pendingLoginMethod";
import { AUTH_METHODS, TELEMETRY_EVENTS } from "@/lib/telemetry/events";

afterEach(() => {
  cleanup();
  vi.resetAllMocks();
  sessionStorage.clear();
  window.history.replaceState({}, "", "/");
});

describe("consumeWelcomeParam", () => {
  it("returns true and strips the param when ?welcome=1 is present", () => {
    window.history.replaceState({}, "", "/?welcome=1");
    expect(consumeWelcomeParam()).toBe(true);
    expect(window.location.pathname).toBe("/");
    expect(window.location.search).toBe("");
  });

  it("preserves other params when stripping welcome", () => {
    window.history.replaceState({}, "", "/?welcome=1&return_to=%2Fx");
    expect(consumeWelcomeParam()).toBe(true);
    expect(window.location.search).toBe("?return_to=%2Fx");
  });

  it("returns false and leaves the URL alone when welcome is absent", () => {
    window.history.replaceState({}, "", "/?other=1");
    expect(consumeWelcomeParam()).toBe(false);
    expect(window.location.search).toBe("?other=1");
  });

  it("ignores a non-1 welcome value (defensive against caller mistakes)", () => {
    window.history.replaceState({}, "", "/?welcome=0");
    expect(consumeWelcomeParam()).toBe(false);
    expect(window.location.search).toBe("?welcome=0");
  });
});

function Harness() {
  useAuthBootstrap({
    setAuthLoading: () => {},
    setAuthenticated: () => {},
    setHasCompletedOnboarding: () => {},
  });
  return null;
}

const mockUser = {
  account_id: "acct-1",
  email: "a@b.c",
  first_name: "A",
  last_name: "B",
  has_completed_onboarding: false,
} as Awaited<ReturnType<typeof getCurrentUser>>;

beforeEach(() => {
  vi.mocked(getCurrentUser).mockResolvedValue(mockUser);
});

describe("useAuthBootstrap post-OAuth boot", () => {
  it("fires only user_signed_in when a method is stashed and welcome is absent", async () => {
    setPendingLoginMethod(AUTH_METHODS.LINKEDIN_OAUTH);
    render(<Harness />);
    await waitFor(() => expect(identifyThenTrack).toHaveBeenCalledTimes(1));
    expect(identifyThenTrack).toHaveBeenCalledWith(
      expect.objectContaining({ id: "acct-1", email: "a@b.c" }),
      TELEMETRY_EVENTS.USER_SIGNED_IN,
      expect.objectContaining({ login_method: "linkedin_oauth", user_id: "acct-1" }),
    );
  });

  it("fires BOTH user_signed_up and user_signed_in when welcome=1 (social session parity)", async () => {
    setPendingLoginMethod(AUTH_METHODS.LINKEDIN_OAUTH);
    window.history.replaceState({}, "", "/?welcome=1");
    render(<Harness />);
    await waitFor(() => expect(identifyThenTrack).toHaveBeenCalledTimes(2));
    const events = vi.mocked(identifyThenTrack).mock.calls.map((call) => call[1]);
    expect(events).toContain(TELEMETRY_EVENTS.USER_SIGNED_UP);
    expect(events).toContain(TELEMETRY_EVENTS.USER_SIGNED_IN);
    expect(window.location.search).toBe("");
  });

  it("fires both events for OIDC too (OidcSignInPage stashes AUTH_METHODS.OIDC)", async () => {
    setPendingLoginMethod(AUTH_METHODS.OIDC);
    window.history.replaceState({}, "", "/?welcome=1");
    render(<Harness />);
    await waitFor(() => expect(identifyThenTrack).toHaveBeenCalledTimes(2));
    const signUpCall = vi
      .mocked(identifyThenTrack)
      .mock.calls.find((c) => c[1] === TELEMETRY_EVENTS.USER_SIGNED_UP);
    expect(signUpCall?.[2]).toMatchObject({ signup_method: "oidc" });
  });

  it("still strips ?welcome=1 even when no pending method is stashed (URL hygiene)", async () => {
    window.history.replaceState({}, "", "/?welcome=1");
    render(<Harness />);
    await waitFor(() => expect(getCurrentUser).toHaveBeenCalled());
    expect(window.location.search).toBe("");
    // No pending method ⇒ nothing fires. The welcome strip still runs so a
    // refresh wouldn't carry the stale param forward.
    expect(identifyThenTrack).not.toHaveBeenCalled();
  });
});
