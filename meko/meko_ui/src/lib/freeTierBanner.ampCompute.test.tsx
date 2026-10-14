// Covers the TierLimitBanner behaviour MEKO-914 added/changed: the ampCompute
// kind, the metricLine override, the Pro source arm (AMP caps gate Pro), and the
// preserved Standard fallback for enterprise/unknown users. Own file because the
// user_type mock is mutated per test (one static mock per module elsewhere).
import { describe, expect, it, beforeEach, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { TierLimitBanner } from "./freeTierBanner";
import { AMP_COMPUTE_BANNER_KIND, type TierBannerKind } from "./freeTier";

// Mutable so each test can pick the viewer's tier before render.
const mockUser = vi.hoisted(() => ({ userType: "free" as string }));

vi.mock("@/hooks/useCurrentUser", () => ({
  useCurrentUser: () => ({ data: { user_type: mockUser.userType, email: "u@example.com" } }),
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

// Keep the Standard column enabled so a Free source resolves to the Standard CTA.
vi.mock("@/lib/freeTier", async () => {
  const actual = await vi.importActual<typeof import("@/lib/freeTier")>("@/lib/freeTier");
  return { ...actual, STANDARD_TIER_ENABLED: true };
});

function renderBanner(metricLine?: string, kind: TierBannerKind = AMP_COMPUTE_BANNER_KIND) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter>
        <TierLimitBanner kind={kind} metricLine={metricLine} />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  mockUser.userType = "free";
});

describe("TierLimitBanner — AMP compute", () => {
  it("shows the Pro-tier lead and Talk-to-our-team CTA for a Pro user at the vCPU cap", () => {
    mockUser.userType = "pro";
    renderBanner("0.5 / 0.5 vCPU used");
    expect(screen.getByText(/Pro tier limit on AMP compute \(vCPU\)/i)).toBeTruthy();
    expect(screen.getByRole("link", { name: /talk to our team/i })).toBeTruthy();
  });

  it("keeps the Standard lead for an enterprise/unknown user (unchanged fallback)", () => {
    mockUser.userType = "enterprise";
    renderBanner("5 / 5 vCPU used");
    expect(screen.getByText(/Standard tier limit on AMP compute \(vCPU\)/i)).toBeTruthy();
  });

  it("shows the Free lead + Upgrade-to-Standard CTA for a Free user", () => {
    renderBanner("0.5 / 0.5 vCPU used");
    expect(screen.getByText(/Free tier limit on AMP compute \(vCPU\)/i)).toBeTruthy();
    expect(screen.getByRole("link", { name: /upgrade to standard/i })).toBeTruthy();
  });

  it("renders the metricLine override", () => {
    renderBanner("0.5 / 0.5 vCPU used");
    expect(screen.getByText("0.5 / 0.5 vCPU used")).toBeTruthy();
  });

  it("metricLine takes precedence over the usage-derived line", () => {
    // datapack is usage-backed (would read "10 / 10 datapacks used"); the
    // explicit override must win.
    renderBanner("42 / 99 datapacks used", "datapack");
    expect(screen.getByText("42 / 99 datapacks used")).toBeTruthy();
    expect(screen.queryByText("10 / 10 datapacks used")).toBeNull();
  });
});
