// First-time Create share link modal: pins the one-API-call contract
// (status + grant in the same body) and the auto-copy on success — the
// whole point of MEKO-1009 was skipping the "flip switch, pick role, copy"
// three-step dance on the Share page.

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

vi.mock("@/lib/api/shareLinksApi", async (o) => ({
  ...(await o<typeof import("@/lib/api/shareLinksApi")>()),
  updateShareLink: vi.fn(),
}));
vi.mock("@/features/datapacks/hooks/useDatapackShareLink", async (o) => {
  const actual = await o<typeof import("@/features/datapacks/hooks/useDatapackShareLink")>();
  return { ...actual, copyShareLinkUrl: vi.fn() };
});

import { updateShareLink, type DatapackShareLink } from "@/lib/api/shareLinksApi";
import { copyShareLinkUrl } from "@/features/datapacks/hooks/useDatapackShareLink";
import { CreateShareLinkDialog } from "./CreateShareLinkDialog";

const linkReturnedByServer: DatapackShareLink = {
  datapack_id: "dp-1",
  token: "fresh-tok",
  grant: "viewer",
  status: "active",
  redemption_count: 0,
  members_at_link_role: 0,
  created_by_user_id: "u1",
  created_at: "",
  updated_at: "",
};

function renderDialog(onOpenChange = vi.fn()) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <CreateShareLinkDialog open={true} onOpenChange={onOpenChange} datapackId="dp-1" />
    </QueryClientProvider>,
  );
  return onOpenChange;
}

afterEach(() => {
  cleanup();
  vi.resetAllMocks();
});

describe("CreateShareLinkDialog", () => {
  it("confirms with status=active AND the chosen grant in one call, then auto-copies and closes", async () => {
    vi.mocked(updateShareLink).mockResolvedValue(linkReturnedByServer);
    const onOpenChange = renderDialog();

    fireEvent.click(screen.getByRole("button", { name: /Create and copy link/i }));

    await waitFor(() =>
      expect(updateShareLink).toHaveBeenCalledWith("dp-1", {
        status: "active",
        grant: "viewer",
      }),
    );
    await waitFor(() => expect(copyShareLinkUrl).toHaveBeenCalledWith("fresh-tok"));
    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
  });

  it("stays open on API failure so the user can retry", async () => {
    vi.mocked(updateShareLink).mockRejectedValue(new Error("boom"));
    const onOpenChange = renderDialog();

    fireEvent.click(screen.getByRole("button", { name: /Create and copy link/i }));

    await waitFor(() => expect(updateShareLink).toHaveBeenCalled());
    expect(copyShareLinkUrl).not.toHaveBeenCalled();
    expect(onOpenChange).not.toHaveBeenCalledWith(false);
  });

  it("Cancel closes the dialog without hitting the API", () => {
    const onOpenChange = renderDialog();
    fireEvent.click(screen.getByRole("button", { name: /Cancel/i }));
    expect(updateShareLink).not.toHaveBeenCalled();
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });
});
