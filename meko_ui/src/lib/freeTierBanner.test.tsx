import { describe, it, expect, afterEach, vi } from "vitest";
import { render, cleanup } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { FreeTierLimitBanner } from "@/lib/freeTierBanner";
import { FREE_TIER_BANNER_MESSAGES } from "@/lib/freeTier";

// TierLimitBanner (aliased as FreeTierLimitBanner) reads /auth/me to
// choose Free→Standard vs Standard→Pro copy. Mock the hook to return a
// Free user so the assertions match FREE_TIER_BANNER_MESSAGES, which is
// the Free→Standard alias. Without the mock, the hook returns undefined
// and nextTierFor defaults to "pro", yielding the Standard→Pro copy.
vi.mock("@/hooks/useCurrentUser", () => ({
  useCurrentUser: () => ({ data: { user_type: "free" } }),
  CURRENT_USER_QUERY_KEY: "current-user",
}));

afterEach(cleanup);

// <Link> from react-router-dom needs a Router; useCurrentUser is
// mocked above but the query client still needs a provider because
// other imports may transitively pull it in.
function renderBanner(ui: React.ReactElement) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, refetchOnWindowFocus: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>{ui}</MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("FreeTierLimitBanner", () => {
  // The three limits the user is gated on: datapack creation, WorkbenchLM token
  // burn, and conversation count. When getFreeTierStatus reports the matching
  // hasHit* flag (see freeTier.test.ts), the caller renders this banner with the
  // corresponding `kind`; here we assert the right copy + the upgrade CTA show.
  // Mocked user_type='free' → banner picks the Free→Standard variant, so the
  // CTA label is "Upgrade".
  it.each([
    ["datapack", FREE_TIER_BANNER_MESSAGES.datapack],
    ["tokenBurn", FREE_TIER_BANNER_MESSAGES.tokenBurn],
    ["conversations", FREE_TIER_BANNER_MESSAGES.conversations],
  ] as const)("shows the %s limit banner with an Upgrade CTA", (kind, message) => {
    const { container } = renderBanner(<FreeTierLimitBanner kind={kind} />);
    expect(container.textContent).toContain(message);
    expect(container.textContent).toContain("Upgrade");
  });

  it("renders distinct copy per limit kind", () => {
    const a = renderBanner(<FreeTierLimitBanner kind="datapack" />).container.textContent;
    cleanup();
    const b = renderBanner(<FreeTierLimitBanner kind="tokenBurn" />).container.textContent;
    expect(a).not.toEqual(b);
  });
});
