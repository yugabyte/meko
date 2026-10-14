// Signed-out side of a share link: the sign-up and sign-in buttons carry no token,
// since analytics records page URLs, and continuing remembers the link instead.
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { ROUTES } from "@/app/routes/paths";
import { pendingSharePath } from "@/lib/pendingShareLink";
import ShareLinkLandingPage from "@/features/share-link/pages/ShareLinkLandingPage";

afterEach(() => {
  cleanup();
  sessionStorage.clear();
});

function renderAt(token: string) {
  render(
    <MemoryRouter initialEntries={[`/share/${token}`]}>
      <Routes>
        <Route path="/share/:token" element={<ShareLinkLandingPage />} />
        <Route path="*" element={<p>auth page</p>} />
      </Routes>
    </MemoryRouter>,
  );
}

describe("ShareLinkLandingPage", () => {
  it("links to sign-up and sign-in without the token in the URL", () => {
    renderAt("secret-tok");
    const signup = screen.getByRole("link", { name: /sign up/i });
    const signin = screen.getByRole("link", { name: /sign in/i });
    expect(signup.getAttribute("href")).toBe(ROUTES.signup);
    expect(signin.getAttribute("href")).toBe(ROUTES.signin);
  });

  it("remembers the link only once the visitor continues", () => {
    renderAt("secret-tok");
    expect(pendingSharePath()).toBe("");
    fireEvent.click(screen.getByRole("link", { name: /sign in/i }));
    expect(pendingSharePath()).toBe("/share/secret-tok");
  });
});
