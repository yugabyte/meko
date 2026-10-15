// Sign-out gate: hosted always offers it; a BYOC data plane only when /auth/config
// says OIDC, so OIDC users can end their issuer session and dev auth never shows it.

import { describe, expect, it } from "vitest";

import {
  canSignOut,
  FALLBACK_AUTH_CONFIG,
  type AuthConfig,
  type AuthProvider,
} from "@/features/auth/authConfig";

function config(provider: AuthProvider): AuthConfig {
  return { provider, passwordLogin: false, signup: false, socialProviders: [] };
}

describe("canSignOut", () => {
  it("always offers sign-out on a hosted deployment, without needing /auth/config", () => {
    expect(canSignOut(false, null)).toBe(true);
    expect(canSignOut(false, config("cognito"))).toBe(true);
  });

  it("offers sign-out on BYOC signed in through OIDC", () => {
    expect(canSignOut(true, config("oidc"))).toBe(true);
  });

  it("hides sign-out on BYOC dev auth, where there is no session to end", () => {
    expect(canSignOut(true, config("none"))).toBe(false);
  });

  it("hides sign-out on BYOC while /auth/config is loading or not fetched", () => {
    expect(canSignOut(true, null)).toBe(false);
  });

  it("hides sign-out on BYOC when /auth/config failed and fell back to Cognito", () => {
    // useAuthConfig returns this fallback on a fetch error or an unknown
    // provider; on BYOC it must not read as a real IdP.
    expect(canSignOut(true, FALLBACK_AUTH_CONFIG)).toBe(false);
    expect(canSignOut(true, config("cognito"))).toBe(false);
  });
});
