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
  it("shows the degrade prompt when no datapack is selected", () => {
    render(<ShareWithAgentStage session={fakeSession(null)} onGoToCreateDatapack={vi.fn()} />);
    expect(screen.getByText(/choose or create a datapack first/i)).toBeTruthy();
    expect(mockListMembers).not.toHaveBeenCalled();
  });

  it("renders members once loaded, same as CollaborateStage", async () => {
    render(<ShareWithAgentStage session={fakeSession("dp-1")} onGoToCreateDatapack={vi.fn()} />);
    await waitFor(() => expect(screen.getByText("Owner User")).toBeTruthy());
  });

  it("pre-fills the AgentK admin email and locks it from editing", async () => {
    render(<ShareWithAgentStage session={fakeSession("dp-1")} onGoToCreateDatapack={vi.fn()} />);
    await waitFor(() => expect(mockListMembers).toHaveBeenCalledTimes(1));

    const emailInput = (await screen.findByDisplayValue("agentk-admin@example.com")) as HTMLInputElement;
    expect(emailInput.disabled).toBe(true);
  });

  it("pre-selects Contributor and disables the grant picker", async () => {
    render(<ShareWithAgentStage session={fakeSession("dp-1")} onGoToCreateDatapack={vi.fn()} />);
    await waitFor(() => expect(mockListMembers).toHaveBeenCalledTimes(1));

    // "Contributor" appears twice: the Select's current value, and the tile
    // label -- both should sit inside a disabled control.
    const contributorMatches = screen.getAllByText("Contributor");
    expect(contributorMatches).toHaveLength(2);
    for (const el of contributorMatches) {
      const control = el.closest("button");
      expect(control?.hasAttribute("disabled")).toBe(true);
    }
  });

  it("clicking 'Send invite' shares with the locked email and grant, no typing required", async () => {
    render(<ShareWithAgentStage session={fakeSession("dp-1")} onGoToCreateDatapack={vi.fn()} />);
    await waitFor(() => expect(mockListMembers).toHaveBeenCalledTimes(1));

    fireEvent.click(screen.getByRole("button", { name: /send invite/i }));

    await waitFor(() => expect(mockInvite).toHaveBeenCalledTimes(1));
    expect(mockInvite).toHaveBeenCalledWith(
      "dp-1",
      { recipient_email: "agentk-admin@example.com", grant: "contributor" },
      {},
    );
    // Silent refetch after the invite, same as CollaborateStage.
    await waitFor(() => expect(mockListMembers).toHaveBeenCalledTimes(2));
  });

  it("disables Send invite once AgentK already has access, via Members", async () => {
    mockListMembers.mockResolvedValue({
      members: [
        { user_id: "user-1", is_owner: true, user_name: "Owner User", user_email_id: "owner@example.com", grant: "maintainer" },
        {
          user_id: "user-2",
          is_owner: false,
          user_name: "Agentk Admin",
          user_email_id: "agentk-admin@example.com",
          grant: "contributor",
        },
      ],
    });
    render(<ShareWithAgentStage session={fakeSession("dp-1")} onGoToCreateDatapack={vi.fn()} />);
    await waitFor(() => expect(mockListMembers).toHaveBeenCalledTimes(1));

    const sendButton = await screen.findByRole("button", { name: /send invite/i });
    expect(sendButton.hasAttribute("disabled")).toBe(true);
    expect(screen.getByText(/already shared with agentk/i)).toBeTruthy();

    fireEvent.click(sendButton);
    expect(mockInvite).not.toHaveBeenCalled();
  });

  it("disables Send invite when AgentK still has a pending invite", async () => {
    mockListOutgoingShares.mockResolvedValue({
      pending_invites: [
        { invite_id: "inv-1", grant: "contributor", recipient_email_id: "agentk-admin@example.com" },
      ],
    });
    render(<ShareWithAgentStage session={fakeSession("dp-1")} onGoToCreateDatapack={vi.fn()} />);
    await waitFor(() => expect(mockListMembers).toHaveBeenCalledTimes(1));

    const sendButton = await screen.findByRole("button", { name: /send invite/i });
    expect(sendButton.hasAttribute("disabled")).toBe(true);
  });
});
