// Password sign-in resumes a share link the visitor started from, including when
// return_to is gone (the new-password step navigates back here without it).
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

vi.mock("@/lib/api/mekoApi", async (o) => ({
  ...(await o<typeof import("@/lib/api/mekoApi")>()),
  login: vi.fn(),
  getCurrentUser: vi.fn(),
}));
vi.mock("@/features/auth/authConfig", async (o) => {
  const actual = await o<typeof import("@/features/auth/authConfig")>();
  return { ...actual, useAuthConfig: () => actual.FALLBACK_AUTH_CONFIG };
});
vi.mock("@/lib/telemetry/rudderstack", () => ({ identifyThenTrack: vi.fn() }));

import { getCurrentUser, login } from "@/lib/api/mekoApi";
import { savePendingShareToken } from "@/lib/pendingShareLink";
import SignInPage from "@/features/auth/pages/SignInPage";

afterEach(() => {
  cleanup();
  vi.resetAllMocks();
  sessionStorage.clear();
  localStorage.clear();
});

function signInAt(entry: string) {
  vi.mocked(login).mockResolvedValue({} as Awaited<ReturnType<typeof login>>);
  vi.mocked(getCurrentUser).mockResolvedValue({
    account_id: "u-1",
    email: "a@b.c",
    has_completed_onboarding: true,
  } as Awaited<ReturnType<typeof getCurrentUser>>);
  render(
    <MemoryRouter initialEntries={[entry]}>
      <Routes>
        <Route path="/signin" element={<SignInPage />} />
        <Route path="/share/:token" element={<p>share page</p>} />
        <Route path="/somewhere" element={<p>return_to page</p>} />
        <Route path="*" element={<p>default page</p>} />
      </Routes>
    </MemoryRouter>,
  );
  fireEvent.change(screen.getByLabelText("Email address"), { target: { value: "a@b.c" } });
  fireEvent.change(screen.getByLabelText("Password"), { target: { value: "pw" } });
  fireEvent.submit(screen.getByLabelText("Password").closest("form")!);
}

describe("SignInPage", () => {
  it("resumes the pending share link when there is no return_to", async () => {
    savePendingShareToken("tok");
    signInAt("/signin");
    expect(await screen.findByText("share page")).toBeTruthy();
  });

  it("lets an explicit return_to win over the pending link", async () => {
    savePendingShareToken("tok");
    signInAt("/signin?return_to=%2Fsomewhere");
    expect(await screen.findByText("return_to page")).toBeTruthy();
  });

  it("lands on the default page with neither", async () => {
    signInAt("/signin");
    expect(await screen.findByText("default page")).toBeTruthy();
  });
});
