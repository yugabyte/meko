import { describe, it, expect, afterEach } from "vitest";
import { render, cleanup } from "@testing-library/react";
import { FreeTierLimitBanner } from "@/lib/freeTierBanner";
import { FREE_TIER_BANNER_MESSAGES } from "@/lib/freeTier";

afterEach(cleanup);

describe("FreeTierLimitBanner", () => {
  // The three limits the user is gated on: datapack creation, WorkbenchLM token
  // burn, and conversation count. When getFreeTierStatus reports the matching
  // hasHit* flag (see freeTier.test.ts), the caller renders this banner with the
  // corresponding `kind`; here we assert the right copy + upgrade CTA show.
  it.each([
    ["datapack", FREE_TIER_BANNER_MESSAGES.datapack],
    ["tokenBurn", FREE_TIER_BANNER_MESSAGES.tokenBurn],
    ["conversations", FREE_TIER_BANNER_MESSAGES.conversations],
  ] as const)("shows the %s limit banner with a Contact sales CTA", (kind, message) => {
    const { container } = render(<FreeTierLimitBanner kind={kind} />);
    expect(container.textContent).toContain(message);
    expect(container.textContent).toContain("Contact sales");
  });

  it("renders distinct copy per limit kind", () => {
    const a = render(<FreeTierLimitBanner kind="datapack" />).container.textContent;
    cleanup();
    const b = render(<FreeTierLimitBanner kind="tokenBurn" />).container.textContent;
    expect(a).not.toEqual(b);
  });
});
