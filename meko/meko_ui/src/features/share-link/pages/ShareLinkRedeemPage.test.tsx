// Signed-in side of a share link: a successful join opens the datapack, a dead link
// says so, and the pending link is cleared on those outcomes but kept on a transient error.
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

vi.mock("@/hooks/use-toast", () => ({ toast: vi.fn() }));
vi.mock("@/lib/api/shareLinksApi", async (o) => ({
  ...(await o<typeof import("@/lib/api/shareLinksApi")>()),
  redeemShareLink: vi.fn(),
}));

import { MekoApiRequestError } from "@/lib/api/mekoApi";
import { SHARE_LINK_REMOVED_ERROR_CODE, redeemShareLink } from "@/lib/api/shareLinksApi";
import { toast } from "@/hooks/use-toast";
import { pendingSharePath, savePendingShareToken } from "@/lib/pendingShareLink";
import {
  SHARE_LINK_ALREADY_MEMBER_TITLE,
  SHARE_LINK_REMOVED_TITLE,
  SHARE_LINK_UNAVAILABLE_TITLE,
} from "@/features/share-link/shareLinkCopy";
import ShareLinkRedeemPage from "@/features/share-link/pages/ShareLinkRedeemPage";

function renderAt(token: string) {
  return render(
    <QueryClientProvider client={new QueryClient()}>
      <MemoryRouter initialEntries={[`/share/${token}`]}>
        <Routes>
          <Route path="/share/:token" element={<ShareLinkRedeemPage token={token} />} />
          <Route path="/datapacks/:id" element={<p>datapack page</p>} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

afterEach(() => {
  cleanup();
  vi.resetAllMocks();
  sessionStorage.clear();
});

describe("ShareLinkRedeemPage", () => {
  it("joins once and opens the datapack", async () => {
    vi.mocked(redeemShareLink).mockResolvedValue({
      datapack_id: "dp-1",
      grant: "viewer",
      already_member: false,
    });
    savePendingShareToken("tok");

    renderAt("tok");

    expect(await screen.findByText("datapack page")).toBeTruthy();
    expect(redeemShareLink).toHaveBeenCalledTimes(1);
    expect(redeemShareLink).toHaveBeenCalledWith("tok");
    expect(pendingSharePath()).toBe("");
  });

  it("tells someone the owner removed that they were removed, and drops the link", async () => {
    vi.mocked(redeemShareLink).mockRejectedValue(
      new MekoApiRequestError("removed", { code: SHARE_LINK_REMOVED_ERROR_CODE, httpStatus: 403 }),
    );
    savePendingShareToken("tok");

    renderAt("tok");

    expect(await screen.findByText(SHARE_LINK_REMOVED_TITLE)).toBeTruthy();
    expect(pendingSharePath()).toBe("");
  });

  it("says the link isn't available when the API answers 404", async () => {
    vi.mocked(redeemShareLink).mockRejectedValue(
      new MekoApiRequestError("not found", { code: "NOT_FOUND", httpStatus: 404 }),
    );
    savePendingShareToken("tok");

    renderAt("tok");

    expect(await screen.findByText(SHARE_LINK_UNAVAILABLE_TITLE)).toBeTruthy();
    await waitFor(() => expect(pendingSharePath()).toBe(""));
  });

  it("tells an existing member they already belong, and opens the datapack", async () => {
    vi.mocked(redeemShareLink).mockResolvedValue({
      datapack_id: "dp-1",
      grant: "contributor",
      already_member: true,
    });

    renderAt("tok");

    expect(await screen.findByText("datapack page")).toBeTruthy();
    expect(toast).toHaveBeenCalledWith(
      expect.objectContaining({ title: SHARE_LINK_ALREADY_MEMBER_TITLE }),
    );
  });

  it("doesn't call a 403 without the removal code a removal", async () => {
    vi.mocked(redeemShareLink).mockRejectedValue(
      new MekoApiRequestError("blocked", { httpStatus: 403 }),
    );
    savePendingShareToken("tok");

    renderAt("tok");

    expect(await screen.findByText(SHARE_LINK_UNAVAILABLE_TITLE)).toBeTruthy();
    expect(screen.queryByText(SHARE_LINK_REMOVED_TITLE)).toBeNull();
    expect(pendingSharePath()).toBe("/share/tok");
  });

  it("keeps the pending link when the redeem fails for a reason that may pass", async () => {
    vi.mocked(redeemShareLink).mockRejectedValue(
      new MekoApiRequestError("unavailable", { code: "INTERNAL_ERROR", httpStatus: 503 }),
    );
    savePendingShareToken("tok");

    renderAt("tok");

    expect(await screen.findByText(SHARE_LINK_UNAVAILABLE_TITLE)).toBeTruthy();
    expect(pendingSharePath()).toBe("/share/tok");
  });
});
