// LinkedIn sign-up affordance: button renders only when it will work (Cognito +
// no return_to), and clicking it stashes the method before leaving the SPA so
// useAuthBootstrap can route the post-redirect event correctly.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

vi.mock("@/features/auth/authConfig", async (o) => {
  const actual = await o<typeof import("@/features/auth/authConfig")>();
  return { ...actual, useAuthConfig: vi.fn(() => actual.FALLBACK_AUTH_CONFIG) };
});
vi.mock("@/features/auth/pendingLoginMethod", () => ({
  setPendingLoginMethod: vi.fn(),
}));
vi.mock("@/lib/telemetry/rudderstack", () => ({ identifyThenTrack: vi.fn() }));

import { useAuthConfig } from "@/features/auth/authConfig";
import { setPendingLoginMethod } from "@/features/auth/pendingLoginMethod";
import { AUTH_METHODS } from "@/lib/telemetry/events";
import SignUpPage from "@/features/auth/pages/SignUpPage";

const originalHref = window.location.href;

afterEach(() => {
  cleanup();
  vi.resetAllMocks();
  // Reset the href the handler sets before each test.
  window.history.replaceState({}, "", originalHref);
});

beforeEach(() => {
  // jsdom's default location is navigable; stub assignment so the handler
  // doesn't actually navigate the test runner.
  Object.defineProperty(window, "location", {
    value: { ...window.location, href: originalHref, assign: vi.fn() },
    writable: true,
  });
});

function renderAt(entry: string) {
  render(
    <MemoryRouter initialEntries={[entry]}>
      <Routes>
        <Route path="/signup" element={<SignUpPage />} />
      </Routes>
    </MemoryRouter>,
  );
}

describe("SignUpPage LinkedIn affordance", () => {
  it("renders the LinkedIn button on a Cognito deployment with no return_to", () => {
    renderAt("/signup");
    expect(screen.getByRole("button", { name: /Sign up with LinkedIn/i })).toBeTruthy();
  });

  it("hides the LinkedIn button when return_to is present (preserves marketplace / invite flows)", () => {
    renderAt("/signup?return_to=%2Fmarketplace%2Fcomplete%3Ft%3Dxyz");
    expect(screen.queryByRole("button", { name: /Sign up with LinkedIn/i })).toBeNull();
  });

  it("hides the LinkedIn button on an OIDC deployment", () => {
    vi.mocked(useAuthConfig).mockReturnValue({
      provider: "oidc",
      loginUrl: "https://issuer.example/login",
      passwordLogin: false,
      signup: false,
      socialProviders: [],
    });
    renderAt("/signup");
    expect(screen.queryByRole("button", { name: /Sign up with LinkedIn/i })).toBeNull();
  });

  it("stashes LINKEDIN_OAUTH and redirects when the button is clicked", () => {
    renderAt("/signup");
    fireEvent.click(screen.getByRole("button", { name: /Sign up with LinkedIn/i }));
    expect(setPendingLoginMethod).toHaveBeenCalledWith(AUTH_METHODS.LINKEDIN_OAUTH);
    expect(window.location.href).toMatch(/\/auth\/linkedin\/login$/);
  });
});
