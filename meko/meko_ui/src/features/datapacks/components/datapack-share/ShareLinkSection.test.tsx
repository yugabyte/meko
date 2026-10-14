// Share page link card: hidden on the API's 403, retryable on any other failure,
// the on/off switch keeps the role, and ?highlight=link scrolls to the card.
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";

vi.mock("@/lib/api/shareLinksApi", async (o) => ({
  ...(await o<typeof import("@/lib/api/shareLinksApi")>()),
  getShareLink: vi.fn(),
  updateShareLink: vi.fn(),
}));

import { MekoApiRequestError } from "@/lib/api/mekoApi";
import { getShareLink, updateShareLink, type DatapackShareLink } from "@/lib/api/shareLinksApi";
import { ShareLinkSection } from "./ShareLinkSection";

const activeLink: DatapackShareLink = {
  datapack_id: "dp-1",
  token: "tok",
  grant: "viewer",
  status: "active",
  redemption_count: 3,
  members_at_link_role: 2,
  created_by_user_id: "u1",
  created_at: "",
  updated_at: "",
};

function renderSection(url = "/") {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={[url]}>
        <ShareLinkSection datapackId="dp-1" />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

afterEach(() => {
  cleanup();
  vi.resetAllMocks();
});

describe("ShareLinkSection", () => {
  it("scrolls to the card when opened with ?highlight=link", async () => {
    const scroll = vi.fn();
    Element.prototype.scrollIntoView = scroll;
    vi.mocked(getShareLink).mockResolvedValue({ ...activeLink, status: "off" });
    renderSection("/share?highlight=link");
    await waitFor(() => expect(scroll).toHaveBeenCalled());
  });

  it("renders nothing for a caller the API refuses the link", async () => {
    vi.mocked(getShareLink).mockRejectedValue(
      new MekoApiRequestError("forbidden", { httpStatus: 403 }),
    );
    const { container } = renderSection();
    await waitFor(() => expect(getShareLink).toHaveBeenCalled());
    await waitFor(() => expect(container.textContent).toBe(""));
  });

  it("shows any other failure with a retry instead of hiding the card", async () => {
    vi.mocked(getShareLink)
      .mockRejectedValueOnce(new MekoApiRequestError("server error", { httpStatus: 500 }))
      .mockResolvedValueOnce(activeLink);
    renderSection();

    fireEvent.click(await screen.findByRole("button", { name: "Retry" }));

    expect(await screen.findByRole("button", { name: /Copy link/ })).toBeTruthy();
    expect(getShareLink).toHaveBeenCalledTimes(2);
  });

  it("resets the link only after the owner confirms", async () => {
    vi.mocked(getShareLink).mockResolvedValue(activeLink);
    vi.mocked(updateShareLink).mockResolvedValue({ ...activeLink, token: "new-tok" });
    renderSection();

    fireEvent.click(await screen.findByRole("button", { name: /Reset link/ }));
    expect(updateShareLink).not.toHaveBeenCalled();
    const dialog = await screen.findByRole("alertdialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Reset link" }));

    await waitFor(() => expect(updateShareLink).toHaveBeenCalledWith("dp-1", { reset: true }));
  });

  it("shows the join count and turns an active link off", async () => {
    vi.mocked(getShareLink).mockResolvedValue(activeLink);
    vi.mocked(updateShareLink).mockResolvedValue({ ...activeLink, status: "off" });
    renderSection();

    expect(await screen.findByText("3 joined via link")).toBeTruthy();
    expect(
      screen.getByText(/also changes it for the 2 people who joined through this link/),
    ).toBeTruthy();
    fireEvent.click(screen.getByRole("switch"));

    await waitFor(() => expect(updateShareLink).toHaveBeenCalledWith("dp-1", { status: "off" }));
    await waitFor(() => expect(screen.queryByRole("button", { name: /copy link/i })).toBeNull());
  });
});
