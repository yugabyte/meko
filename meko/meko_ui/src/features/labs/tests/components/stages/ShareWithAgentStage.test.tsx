import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

import {
  inviteDatapackShare,
  listDatapackMembers,
  listDatapackOutgoingShares,
} from "@/lib/api/mekoApi";
import type { useLabRunnerSession } from "@/features/labs/hooks/useLabRunnerSession";
import { ShareWithAgentStage } from "@/features/labs/components/stages/ShareWithAgentStage";

vi.mock("@/lib/runtimeEnv", () => ({
  readRuntimeEnv: (name: string) =>
    name === "MEKO_LABS_AGENTK_ADMIN_EMAIL" ? "agentk-admin@example.com" : undefined,
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
      {
        user_id: "user-1",
        is_owner: true,
        user_name: "Owner User",
        user_email_id: "owner@example.com",
        grant: "maintainer",
      },
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
    // Two places name it now: the role picker's value and the description
    // row below it.
    await waitFor(() => expect(screen.getAllByText("Contributor").length).toBeGreaterThan(0));
    // Spelled out rather than deferring to the role above it: a reader
    // shouldn't have to look up "viewer" to learn what contributor grants.
    expect(
      screen.getByText("Can view Knowledge, memories and conversations, and can generate memories"),
    ).toBeTruthy();
    // The picker is readable, not a dead label: the level is shown as one of
    // the product's three, with the lab's choice already made.
    expect(screen.getByLabelText("Access level")).toBeTruthy();
  });

  it("shares with the configured address and grant, with nothing to type", async () => {
    render(
      <ShareWithAgentStage
        session={fakeSession("dp-1")}
        onGoToCreateDatapack={vi.fn()}
        onComplete={vi.fn()}
      />,
    );
    fireEvent.click(await screen.findByRole("button", { name: /share with agentk/i }));
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
    fireEvent.click(await screen.findByRole("button", { name: /share with agentk/i }));
    // The API's own message, not a generic fallback that hides the reason.
    await waitFor(() => expect(screen.getByText(/share limit reached/i)).toBeTruthy());
  });

  it("treats an existing member as already shared, so nobody shares twice", async () => {
    mockListMembers.mockResolvedValue({
      members: [
        {
          user_id: "user-1",
          is_owner: true,
          user_email_id: "owner@example.com",
          grant: "maintainer",
        },
        {
          user_id: "agent-1",
          is_owner: false,
          user_email_id: "agentk-admin@example.com",
          grant: "contributor",
        },
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
    expect((await screen.findByRole("button", { name: /shared/i })).hasAttribute("disabled")).toBe(
      true,
    );
  });

  // A pending invite is not a grant. The shared AgentK runtime authenticates
  // to Meko as this account, so letting the lesson pass here sent the user
  // into Chapter 3 to hit datapack_access_denied from inside the agent.
  it("does not pass the lesson on an invite nobody has accepted", async () => {
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
    expect(await screen.findByText(/invite pending/i)).toBeTruthy();
    expect(onComplete).not.toHaveBeenCalled();
  });

  // Re-sending is rejected by api_server as a duplicate, so offering it
  // again would only produce an error the user can do nothing about.
  it("stops offering to invite again, and says why the review will fail", async () => {
    mockListOutgoingShares.mockResolvedValue({
      pending_invites: [{ recipient_email_id: "agentk-admin@example.com", grant: "contributor" }],
    } as never);
    render(
      <ShareWithAgentStage
        session={fakeSession("dp-1")}
        onGoToCreateDatapack={vi.fn()}
        onComplete={vi.fn()}
      />,
    );
    const button = await screen.findByRole("button", { name: /invite sent, not accepted/i });
    expect(button.hasAttribute("disabled")).toBe(true);
    expect(screen.getByText(/the review in Chapter 3 will fail/i)).toBeTruthy();
  });
});
