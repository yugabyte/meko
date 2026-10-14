// Unit tests for the shared TierLimitBanner (MEKO-809). Previously the
// banner was a dead-end copy line; it now renders a metric line, a
// context-sensitive CTA (Upgrade to Standard with deep-link, or
// Talk-to-our-team for Pro / gated-Standard tiers), and a Contact-us
// link. Each is covered so a future regression on Scott's "no next
// step" complaint is caught in CI.
import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { TierLimitBanner, HIGHLIGHT_TIER_PARAM, tierBannerKindForError } from "./freeTierBanner";
import { MekoApiRequestError } from "@/lib/api/mekoApi";

// useCurrentUser hits auth; useAccountUsage hits a react-query fetch.
// Both are deterministic shims so we assert formatted output, not
// network paths.
vi.mock("@/hooks/useCurrentUser", () => ({
  useCurrentUser: () => ({ data: { user_type: "free", email: "u@example.com" } }),
}));

vi.mock("@/hooks/useAccountUsage", () => ({
  useAccountUsage: () => ({
    data: {
      datapack_count: 10,
      datapack_creations: 10,
      kb_storage_file_size: 95 * 1024 * 1024,
      num_conversations: 1000,
      token_burn: 10_000_000,
    },
  }),
}));

// MEKO-319: STANDARD_TIER_ENABLED gates the Standard column. Forcing it
// on here keeps the Free-tier tests on the Standard→Upgrade path; the
// gated-off case lives in freeTierBanner.gated.test.tsx (sibling file —
// vi.mock is module-scoped, so the alternate value needs its own
// import).
vi.mock("@/lib/freeTier", async () => {
  const actual = await vi.importActual<typeof import("@/lib/freeTier")>("@/lib/freeTier");
  return { ...actual, STANDARD_TIER_ENABLED: true };
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

describe("TierLimitBanner (Free user, Standard tile enabled)", () => {
  it("names the user's current tier in the lead copy", () => {
    renderBanner("datapack");
    expect(
      screen.getByText(/You've reached the Free tier limit on datapack creation\./),
    ).toBeTruthy();
  });

  it("renders the metric line with X / Y datapacks for a free user at cap", () => {
    renderBanner("datapack");
    expect(screen.getByText(/10 \/ 10 datapacks used/)).toBeTruthy();
  });

  it("formats kb storage in MB", () => {
    renderBanner("kbStorage");
    // kb_storage_file_size is 95 MB, cap is 100 MB. Pricing table shows
    // "100 MB" / "5 GB" without decimals; formatBytes mirrors that.
    expect(screen.getByText(/95\.0 MB \/ 100 MB knowledge storage used/)).toBeTruthy();
  });

  it("formats conversations with thousands separator", () => {
    renderBanner("conversations");
    expect(screen.getByText(/1,000 \/ 1,000 conversations used/)).toBeTruthy();
  });

  it("formats token burn compactly", () => {
    renderBanner("tokenBurn");
    expect(screen.getByText(/10M \/ 10M WorkbenchLM tokens used/)).toBeTruthy();
  });

  it("Upgrade button deep-links to /settings with the highlight-tier param", () => {
    renderBanner("datapack");
    const link = screen.getByRole("link", { name: /Upgrade to Standard/ });
    expect(link.getAttribute("href")).toBe(`/settings?${HIGHLIGHT_TIER_PARAM}=standard`);
  });

  it("does not render a secondary Contact-us link next to the Upgrade button", () => {
    // Simplified banner (Rajagopalan on PR #1697): the Upgrade CTA is
    // the only secondary action on the Standard-target variant. The
    // sales form is still one click away from the Pro card the Upgrade
    // button lands on; keeping it off the banner avoids a crowded row.
    renderBanner("datapack");
    expect(screen.queryByRole("link", { name: /Contact us/ })).toBeNull();
  });
});

describe("TierLimitBanner for a standard-tier user", () => {
  it("shows 'Talk to our team' linking directly to the HubSpot form", async () => {
    // Pro has no self-serve upgrade — the Pro card itself just links to
    // SALES_CONTACT_URL. Routing a Standard user through Settings to
    // click another "Talk to our team" link would be a dead-end second
    // hop, so the banner opens the form directly.
    const useCurrentUserMod = await import("@/hooks/useCurrentUser");
    vi.spyOn(useCurrentUserMod, "useCurrentUser").mockReturnValue({
      data: { user_type: "standard", email: "s@example.com" },
    } as never);
    renderBanner("datapack");
    const link = screen.getByRole("link", { name: /Talk to our team/ });
    expect(link.getAttribute("href") ?? "").toMatch(/^https:\/\/share-na2\.hsforms\.com\//);
    expect(link.getAttribute("target")).toBe("_blank");
  });
});

describe("tierBannerKindForError — api_server 402 mapping", () => {
  it("maps every tier-limit code api_server emits to the right banner kind", () => {
    // Mirrors api_server/internal/service/free_tier.go:301-309. The
    // conversations / token-burn caps live in inference_gateway and the
    // MCP tiers, not api_server, so no 402 code for those surfaces
    // reaches this detector today.
    const cases = [
      { code: "FREE_TIER_DATAPACK_LIMIT", kind: "datapack" },
      { code: "STANDARD_TIER_DATAPACK_LIMIT", kind: "datapack" },
      { code: "FREE_TIER_KB_STORAGE_LIMIT", kind: "kbStorage" },
      { code: "STANDARD_TIER_KB_STORAGE_LIMIT", kind: "kbStorage" },
      // The per-tier AMP vCPU cap (Free/Standard/Pro) maps to the ampCompute banner.
      { code: "FREE_TIER_CORE_LIMIT", kind: "ampCompute" },
      { code: "STANDARD_TIER_CORE_LIMIT", kind: "ampCompute" },
      { code: "PRO_TIER_CORE_LIMIT", kind: "ampCompute" },
    ];
    for (const c of cases) {
      const err = new MekoApiRequestError("over cap", { code: c.code, httpStatus: 402 });
      expect(tierBannerKindForError(err)).toBe(c.kind);
    }
  });

  it("returns null for a 402 code with no banner kind", () => {
    const err = new MekoApiRequestError("over cap", {
      code: "FREE_TIER_SOMETHING_ELSE",
      httpStatus: 402,
    });
    expect(tierBannerKindForError(err)).toBeNull();
  });

  it("returns null for non-402 responses", () => {
    const err = new MekoApiRequestError("not found", {
      code: "FREE_TIER_DATAPACK_LIMIT",
      httpStatus: 404,
    });
    expect(tierBannerKindForError(err)).toBeNull();
  });

  it("returns null for unknown codes", () => {
    const err = new MekoApiRequestError("over", { code: "SOMETHING_ELSE", httpStatus: 402 });
    expect(tierBannerKindForError(err)).toBeNull();
  });

  it("returns null for non-MekoApiRequestError inputs", () => {
    expect(tierBannerKindForError(new Error("boom"))).toBeNull();
    expect(tierBannerKindForError(null)).toBeNull();
    expect(tierBannerKindForError(undefined)).toBeNull();
  });
});
