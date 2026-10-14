// Sharing-data hook: a viewer refused both lists gets membersHidden rather than
// an error, while any other failure still surfaces as an error.
import { beforeEach, describe, expect, it, vi } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";

vi.mock("@/lib/api/mekoApi", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api/mekoApi")>();
  return { ...actual, listDatapackMembers: vi.fn(), listDatapackOutgoingShares: vi.fn() };
});

import {
  MekoApiRequestError,
  listDatapackMembers,
  listDatapackOutgoingShares,
} from "@/lib/api/mekoApi";
import { useDatapackOutgoingShares } from "./useDatapackOutgoingShares";

const forbidden = () =>
  new MekoApiRequestError("forbidden", { code: "FORBIDDEN", httpStatus: 403 });

describe("useDatapackOutgoingShares", () => {
  beforeEach(() => vi.resetAllMocks());

  it("reports a viewer as membersHidden, not an error, when both lists are refused", async () => {
    vi.mocked(listDatapackMembers).mockRejectedValue(forbidden());
    vi.mocked(listDatapackOutgoingShares).mockRejectedValue(forbidden());

    const { result } = renderHook(() => useDatapackOutgoingShares("dp-1"));

    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.error).toBeNull();
    expect(result.current.membersHidden).toBe(true);
    expect(result.current.data).toEqual({ members: [], pending_invites: [] });
  });

  it("still reports a member-list failure that isn't a 403", async () => {
    vi.mocked(listDatapackMembers).mockRejectedValue(
      new MekoApiRequestError("boom", { code: "INTERNAL_ERROR", httpStatus: 500 }),
    );
    vi.mocked(listDatapackOutgoingShares).mockResolvedValue({ pending_invites: [] });

    const { result } = renderHook(() => useDatapackOutgoingShares("dp-1"));

    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.error).not.toBeNull();
    expect(result.current.data).toBeNull();
  });
});
