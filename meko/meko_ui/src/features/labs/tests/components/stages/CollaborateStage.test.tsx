import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

import {
  inviteDatapackShare,
  listDatapackMembers,
  listDatapackOutgoingShares,
} from "@/lib/api/mekoApi";
import type { useLabRunnerSession } from "@/features/labs/hooks/useLabRunnerSession";
import { CollaborateStage } from "@/features/labs/components/stages/CollaborateStage";

vi.mock("@/lib/api/mekoApi", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api/mekoApi")>();
  return {
    ...actual,
    listDatapackMembers: vi.fn(),
    listDatapackOutgoingShares: vi.fn(),
    inviteDatapackShare: vi.fn(),
  };
});

const mockListMembers = vi.mocked(listDatapackMembers);
const mockListOutgoingShares = vi.mocked(listDatapackOutgoingShares);
const mockInvite = vi.mocked(inviteDatapackShare);

function fakeSession(datapackId: string | null): ReturnType<typeof useLabRunnerSession> {
  return {
    userId: "user-1",
    currentUser: {
      account_id: "user-1",
      email: "owner@example.com",
      first_name: "Owner",
      last_name: "User",
      has_completed_onboarding: true,
    },
    datapackId,
    datapackName: datapackId ? "test-datapack" : null,
    datapackGrant: null,
    accessibleDatapacks: [],
    datapacksLoading: false,
    selectDatapack: vi.fn(),
    createAndSelectDatapack: vi.fn(),
    creating: false,
    hasHitDatapackCount: false,
    userToken: "mko_tkn_fake",
    loading: false,
    error: null,
    refreshTick: 0,
    bumpRefreshTick: vi.fn(),
    lastLessonTrace: null,
    recordLessonTrace: vi.fn(),
    deploymentId: null,
    selectDeployment: vi.fn(),
    clearDeployment: vi.fn(),
    deploymentMode: "shared" as const,
    selectDeploymentMode: vi.fn(),
    deployment: null,
    deploymentActive: false,
    ampClusterId: null,
    ampBranchId: null,
    selectAmpCluster: vi.fn(),
    clearAmpCluster: vi.fn(),
  };
}

afterEach(cleanup);
beforeEach(() => {
  vi.clearAllMocks();
  mockListMembers.mockResolvedValue({
    members: [
      {
        user_id: "user-1",
        is_owner: true,
        user_name: "Owner User",
        user_email_id: "owner@example.com",
        grant: "maintainer",
      },
    ],
  });
  mockListOutgoingShares.mockResolvedValue({
    pending_invites: [
      { invite_id: "inv-1", grant: "contributor", recipient_email_id: "pending@example.com" },
    ],
  });
  mockInvite.mockResolvedValue({} as never);
});

describe("CollaborateStage", () => {
  it("shows the degrade prompt when no datapack is selected", () => {
    render(<CollaborateStage session={fakeSession(null)} onGoToCreateDatapack={vi.fn()} />);
    expect(screen.getByText(/choose or create a datapack first/i)).toBeTruthy();
    expect(mockListMembers).not.toHaveBeenCalled();
  });

  it("renders members and pending invites once loaded", async () => {
    render(<CollaborateStage session={fakeSession("dp-1")} onGoToCreateDatapack={vi.fn()} />);

    await waitFor(() => expect(screen.getByText("Owner User")).toBeTruthy());
    expect(screen.getByText(/pending@example.com/i)).toBeTruthy();
  });

  it("sending an invite calls inviteDatapackShare then a silent refetch", async () => {
    render(<CollaborateStage session={fakeSession("dp-1")} onGoToCreateDatapack={vi.fn()} />);
    await waitFor(() => expect(mockListMembers).toHaveBeenCalledTimes(1));

    fireEvent.change(screen.getByPlaceholderText("name@company.com"), {
      target: { value: "newperson@example.com" },
    });
    fireEvent.click(screen.getByRole("button", { name: /send invite/i }));

    await waitFor(() => expect(mockInvite).toHaveBeenCalledTimes(1));
    expect(mockInvite).toHaveBeenCalledWith(
      "dp-1",
      expect.objectContaining({ recipient_email: "newperson@example.com" }),
      {},
    );
    // Silent refetch: a second members fetch, without the initial loading skeleton reappearing.
    await waitFor(() => expect(mockListMembers).toHaveBeenCalledTimes(2));
  });

  it("hides the invite form for a contributor -- accessibleDatapacks admits them, but only owner/maintainer can share", async () => {
    mockListMembers.mockResolvedValue({
      members: [
        {
          user_id: "user-1",
          is_owner: true,
          user_name: "Owner User",
          user_email_id: "owner@example.com",
          grant: "maintainer",
        },
        {
          user_id: "user-2",
          is_owner: false,
          user_name: "Contributor User",
          user_email_id: "contributor@example.com",
          grant: "contributor",
        },
      ],
    });
    const contributorSession = {
      ...fakeSession("dp-1"),
      currentUser: { ...fakeSession("dp-1").currentUser!, email: "contributor@example.com" },
    };

    render(<CollaborateStage session={contributorSession} onGoToCreateDatapack={vi.fn()} />);

    await waitFor(() => expect(screen.getByText("Owner User")).toBeTruthy());
    expect(screen.queryByPlaceholderText("name@company.com")).toBeNull();
    expect(screen.queryByRole("button", { name: /send invite/i })).toBeNull();
  });
});
