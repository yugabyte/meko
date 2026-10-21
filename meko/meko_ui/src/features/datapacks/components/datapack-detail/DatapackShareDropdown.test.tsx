// Three items in the Share dropdown and their effects — pin the routing +
// modal-open contract so a future refactor doesn't send Invite People to the
// Share page or let a maintainer see the link item.

import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";

vi.mock("@/lib/api/shareLinksApi", async (o) => ({
  ...(await o<typeof import("@/lib/api/shareLinksApi")>()),
  getShareLink: vi.fn(),
}));
vi.mock("@/features/datapacks/hooks/useDatapackShareLink", async (o) => {
  const actual = await o<typeof import("@/features/datapacks/hooks/useDatapackShareLink")>();
  return { ...actual, copyShareLinkUrl: vi.fn() };
});

import {
  getShareLink,
  type DatapackShareLink,
  type ShareLinkStatus,
} from "@/lib/api/shareLinksApi";
import { copyShareLinkUrl } from "@/features/datapacks/hooks/useDatapackShareLink";
import { DatapackShareDropdown } from "./DatapackShareDropdown";

beforeAll(() => {
  global.ResizeObserver = class {
    observe() {}
    unobserve() {}
    disconnect() {}
  };
  Element.prototype.scrollIntoView = vi.fn();
  Element.prototype.hasPointerCapture = vi.fn(() => false);
  Element.prototype.setPointerCapture = vi.fn();
  Element.prototype.releasePointerCapture = vi.fn();
});

function LocationProbe() {
  const location = useLocation();
  return <div data-testid="loc">{location.pathname + location.search}</div>;
}

function renderDropdown({
  canManageLinkSharing = true,
  linkStatus,
  token,
}: {
  canManageLinkSharing?: boolean;
  linkStatus?: ShareLinkStatus;
  token?: string;
} = {}) {
  const link: DatapackShareLink | null = linkStatus
    ? {
        datapack_id: "dp-1",
        token: token ?? "tok",
        grant: "viewer",
        status: linkStatus,
        redemption_count: 0,
        members_at_link_role: 0,
        created_by_user_id: "u1",
        created_at: "",
        updated_at: "",
      }
    : null;
  vi.mocked(getShareLink).mockResolvedValue(link);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={["/datapacks/dp-1"]}>
        <Routes>
          <Route
            path="/datapacks/dp-1"
            element={
              <>
                <DatapackShareDropdown
                  datapackId="dp-1"
                  canManageLinkSharing={canManageLinkSharing}
                />
                <LocationProbe />
              </>
            }
          />
          <Route path="/datapacks/:id/share" element={<LocationProbe />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

async function openMenu() {
  const trigger = screen.getByRole("button", { name: /share this datapack/i });
  fireEvent.pointerDown(trigger, { button: 0, pointerType: "mouse" });
  fireEvent.pointerUp(trigger, { button: 0, pointerType: "mouse" });
  fireEvent.click(trigger);
  await screen.findByRole("menu");
}

afterEach(() => {
  cleanup();
  vi.resetAllMocks();
});

describe("DatapackShareDropdown", () => {
  it("owner sees all three items", async () => {
    renderDropdown({ linkStatus: "off" });
    await waitFor(() => expect(getShareLink).toHaveBeenCalled());
    await openMenu();
    expect(screen.getByText(/Invite People/i)).toBeTruthy();
    expect(screen.getByText(/Create share link/i)).toBeTruthy();
    expect(screen.getByText(/^Settings$/i)).toBeTruthy();
  });

  it("maintainer (canManageLinkSharing=false) sees Invite + Settings, no link item", async () => {
    renderDropdown({ canManageLinkSharing: false });
    await openMenu();
    expect(screen.getByText(/Invite People/i)).toBeTruthy();
    expect(screen.queryByText(/Create share link/i)).toBeNull();
    expect(screen.queryByText(/Copy share link/i)).toBeNull();
    expect(screen.getByText(/^Settings$/i)).toBeTruthy();
  });

  it("Settings navigates to the Share page (no highlight)", async () => {
    renderDropdown({ linkStatus: "off" });
    await openMenu();
    fireEvent.click(screen.getByText(/^Settings$/i));
    await waitFor(() =>
      expect(screen.getByTestId("loc").textContent).toBe("/datapacks/dp-1/share"),
    );
  });

  it("Invite People opens the InvitePeopleDialog, does not navigate", async () => {
    renderDropdown({ linkStatus: "off" });
    await openMenu();
    fireEvent.click(screen.getByText(/Invite People/i));
    expect(await screen.findByRole("button", { name: /Send invite/i })).toBeTruthy();
    expect(screen.getByTestId("loc").textContent).toBe("/datapacks/dp-1");
  });

  it("Copy share link copies directly when active (no modal)", async () => {
    renderDropdown({ linkStatus: "active", token: "live-tok" });
    await waitFor(() => expect(getShareLink).toHaveBeenCalled());
    await openMenu();
    fireEvent.click(screen.getByText(/Copy share link/i));
    await waitFor(() => expect(copyShareLinkUrl).toHaveBeenCalledWith("live-tok"));
    expect(screen.queryByRole("button", { name: /Create and copy link/i })).toBeNull();
  });

  it("Create share link opens the CreateShareLinkDialog when there is no link row", async () => {
    renderDropdown(); // no link row at all
    await waitFor(() => expect(getShareLink).toHaveBeenCalled());
    await waitFor(() =>
      expect(vi.mocked(getShareLink).mock.results[0].value).resolves.toBeDefined(),
    );
    await openMenu();
    fireEvent.click(screen.getByText(/Create share link/i));
    expect(await screen.findByRole("button", { name: /Create and copy link/i })).toBeTruthy();
  });

  it("regression: off link row → navigates to /share?highlight=link, does NOT open the modal (saved grant preserved)", async () => {
    // The CreateShareLinkDialog's default grant on top of an existing off
    // link would overwrite the saved role and move members who joined at the
    // old role. useShareLinkAction routes off-but-exists to the Share page's
    // Switch instead, which sends {status} alone. If someone drops the
    // !hasRow guard from useShareLinkAction, this test fails.
    renderDropdown({ linkStatus: "off" });
    await waitFor(() => expect(getShareLink).toHaveBeenCalled());
    await waitFor(() =>
      expect(vi.mocked(getShareLink).mock.results[0].value).resolves.toBeDefined(),
    );
    await openMenu();
    fireEvent.click(screen.getByText(/Create share link/i));
    await waitFor(() =>
      expect(screen.getByTestId("loc").textContent).toBe("/datapacks/dp-1/share?highlight=link"),
    );
    // Dialog never mounted.
    expect(screen.queryByRole("button", { name: /Create and copy link/i })).toBeNull();
  });
});
