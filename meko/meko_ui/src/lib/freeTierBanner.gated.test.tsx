// Covers the "Standard tile is pre-GA (gated off)" fallback (MEKO-809).
// Pulled into its own test file because freeTierBanner.test.tsx mocks
// STANDARD_TIER_ENABLED to true at the module level — one mock per
// module import.
//
// Flow under test: default production config has STANDARD_TIER_ENABLED
// unset (= false). A Free user hitting a cap would otherwise be routed
// to /settings with no Standard tile to click, so the banner falls
// back to the Pro variant (Talk-to-our-team, direct HubSpot link).
// Reviewer's finding on PR #1697 was that the previous fix reused the
// WHOLE Pro-target copy — including the "Standard tier limit…" lead —
// which lied about the user's current tier. The fix below pairs the
// Pro CTA with a Free-tier lead so the message matches the user.
import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { TierLimitBanner } from "./freeTierBanner";

vi.mock("@/hooks/useCurrentUser", () => ({
  useCurrentUser: () => ({ data: { user_type: "free", email: "u@example.com" } }),
}));

vi.mock("@/hooks/useAccountUsage", () => ({
  useAccountUsage: () => ({
    data: {
      datapack_count: 10,
      datapack_creations: 10,
      kb_storage_file_size: 0,
      num_conversations: 0,
      token_burn: 0,
    },
  }),
}));

// Gated OFF (default prod + typical dev configuration).
vi.mock("@/lib/freeTier", async () => {
  const actual = await vi.importActual<typeof import("@/lib/freeTier")>("@/lib/freeTier");
  return { ...actual, STANDARD_TIER_ENABLED: false };
});

function renderBanner(kind: Parameters<typeof TierLimitBanner>[0]["kind"]) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter>
        <TierLimitBanner kind={kind} />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("TierLimitBanner — Free user with Standard tile gated off", () => {
  it("still names the Free tier in the lead copy (not Standard)", () => {
    renderBanner("datapack");
    expect(
      screen.getByText(/You've reached the Free tier limit on datapack creation\./),
    ).toBeTruthy();
    // Specifically, don't claim the user is on Standard.
    expect(screen.queryByText(/Standard tier limit/)).toBeNull();
  });

  it("pairs the Free lead with a Talk-to-our-team action", () => {
    renderBanner("datapack");
    expect(screen.getByText(/Talk to our team about higher limits\./)).toBeTruthy();
  });

  it("primary CTA opens the HubSpot form directly in a new tab (no Settings hop)", () => {
    renderBanner("datapack");
    const link = screen.getByRole("link", { name: /Talk to our team/ });
    expect(link.getAttribute("href") ?? "").toMatch(/^https:\/\/share-na2\.hsforms\.com\//);
    expect(link.getAttribute("target")).toBe("_blank");
    expect(link.getAttribute("rel") ?? "").toMatch(/noopener/);
  });

  it("does NOT render a /settings link — Standard is not showable", () => {
    renderBanner("datapack");
    // Any <a> whose href starts with /settings would mean we failed to
    // suppress the Standard deep-link path.
    const settingsLinks = screen
      .queryAllByRole("link")
      .filter((el) => (el.getAttribute("href") ?? "").startsWith("/settings"));
    expect(settingsLinks).toHaveLength(0);
  });
});
