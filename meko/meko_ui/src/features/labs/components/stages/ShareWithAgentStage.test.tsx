import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

import { inviteDatapackShare, listDatapackMembers, listDatapackOutgoingShares } from "@/lib/api/mekoApi";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import { ShareWithAgentStage } from "./ShareWithAgentStage";

vi.mock("@/lib/runtimeEnv", () => ({
  readRuntimeEnv: (name: string) => (name === "MEKO_LABS_AGENTK_ADMIN_EMAIL" ? "agentk-admin@example.com" : undefined),
}));

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
    datapackId,
    datapackName: datapackId ? "test-datapack" : null,
    currentUser: {
      account_id: "user-1",
      email: "owner@example.com",
      first_name: "Owner",
      last_name: "User",
      has_completed_onboarding: true,
    },
  } as unknown as ReturnType<typeof useLabRunnerSession>;
}

afterEach(cleanup);
beforeEach(() => {
  vi.clearAllMocks();
  mockListMembers.mockResolvedValue({
    members: [
      { user_id: "user-1", is_owner: true, user_name: "Owner User", user_email_id: "owner@example.com", grant: "maintainer" },
    ],
  });
  mockListOutgoingShares.mockResolvedValue({ pending_invites: [] });
  mockInvite.mockResolvedValue({} as never);
});

describe("ShareWithAgentStage", () => {
  it("prompts for a datapack before anything else", () => {
    render(
      <ShareWithAgentStage
        session={fakeSession(null)}
        onGoToCreateDatapack={vi.fn()}
        onComplete={vi.fn()}
      />,
    );
    expect(screen.getByText(/choose or create a datapack first/i)).toBeTruthy();
  });

  it("names the exact access level AgentK is granted, in the product's own words", async () => {
    render(
      <ShareWithAgentStage
        session={fakeSession("dp-1")}
        onGoToCreateDatapack={vi.fn()}
        onComplete={vi.fn()}
      />,
    );
    await waitFor(() => expect(screen.getByText("Contributor")).toBeTruthy());
    expect(screen.getByText("Includes viewer rights and can generate memories")).toBeTruthy();
    // ONE grant, stated once. Listing the levels it implies made the reader
    // work out which one actually applies.
    expect(screen.queryByText("Viewer")).toBeNull();
  });

  it("shares with the configured address and grant, with nothing to type", async () => {
    render(
      <ShareWithAgentStage
        session={fakeSession("dp-1")}
        onGoToCreateDatapack={vi.fn()}
        onComplete={vi.fn()}
      />,
    );
    fireEvent.click(await screen.findByRole("button", { name: /share datapack/i }));
    await waitFor(() =>
      expect(mockInvite).toHaveBeenCalledWith(
        "dp-1",
        { recipient_email: "agentk-admin@example.com", grant: "contributor" },
        {},
      ),
    );
  });

  it("surfaces a failed share instead of silently leaving the button idle", async () => {
    mockInvite.mockRejectedValueOnce(new Error("share limit reached"));
    render(
      <ShareWithAgentStage
        session={fakeSession("dp-1")}
        onGoToCreateDatapack={vi.fn()}
        onComplete={vi.fn()}
      />,
    );
    fireEvent.click(await screen.findByRole("button", { name: /share datapack/i }));
    // The API's own message, not a generic fallback that hides the reason.
    await waitFor(() => expect(screen.getByText(/share limit reached/i)).toBeTruthy());
  });

  it("treats an existing member as already shared, so nobody shares twice", async () => {
    mockListMembers.mockResolvedValue({
      members: [
        { user_id: "user-1", is_owner: true, user_email_id: "owner@example.com", grant: "maintainer" },
        { user_id: "agent-1", is_owner: false, user_email_id: "agentk-admin@example.com", grant: "contributor" },
      ],
    } as never);
    const onComplete = vi.fn();
    render(
      <ShareWithAgentStage
        session={fakeSession("dp-1")}
        onGoToCreateDatapack={vi.fn()}
        onComplete={onComplete}
      />,
    );
    await waitFor(() => expect(onComplete).toHaveBeenCalled());
    expect((await screen.findByRole("button", { name: /shared/i })).hasAttribute("disabled")).toBe(true);
  });

  it("treats a still-pending invite as shared too, since re-inviting is a no-op", async () => {
    mockListOutgoingShares.mockResolvedValue({
      pending_invites: [{ recipient_email_id: "agentk-admin@example.com", grant: "contributor" }],
    } as never);
    const onComplete = vi.fn();
    render(
      <ShareWithAgentStage
        session={fakeSession("dp-1")}
        onGoToCreateDatapack={vi.fn()}
        onComplete={onComplete}
      />,
    );
    await waitFor(() => expect(onComplete).toHaveBeenCalled());
  });
});
