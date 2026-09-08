// Separate file: ShareWithAgentStage reads its AgentK admin email as a
// module-level constant (readRuntimeEnv at import time), so the
// "not configured" case needs its own module instance/mock rather than a
// second describe block in the main test file.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";

import { listDatapackMembers, listDatapackOutgoingShares } from "@/lib/api/mekoApi";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import { ShareWithAgentStage } from "./ShareWithAgentStage";

vi.mock("@/lib/runtimeEnv", () => ({
  readRuntimeEnv: () => undefined,
}));

vi.mock("@/lib/api/mekoApi", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api/mekoApi")>();
  return {
    ...actual,
    listDatapackMembers: vi.fn(),
    listDatapackOutgoingShares: vi.fn(),
  };
});

const mockListMembers = vi.mocked(listDatapackMembers);
const mockListOutgoingShares = vi.mocked(listDatapackOutgoingShares);

afterEach(cleanup);
beforeEach(() => {
  vi.clearAllMocks();
  mockListMembers.mockResolvedValue({
    members: [
      { user_id: "user-1", is_owner: true, user_name: "Owner User", user_email_id: "owner@example.com", grant: "maintainer" },
    ],
  });
  mockListOutgoingShares.mockResolvedValue({ pending_invites: [] });
});

describe("ShareWithAgentStage — no AgentK admin email configured", () => {
  it("shows a config-missing message and doesn't render the invite form", async () => {
    const session = {
      datapackId: "dp-1",
      datapackName: "test-datapack",
      currentUser: { account_id: "user-1", email: "owner@example.com" },
    } as unknown as ReturnType<typeof useLabRunnerSession>;
    render(<ShareWithAgentStage session={session} onGoToCreateDatapack={vi.fn()} />);

    expect(screen.getByText(/isn't configured with an AgentK account/i)).toBeTruthy();
    await waitFor(() => expect(screen.getByText("Owner User")).toBeTruthy());
    expect(screen.queryByPlaceholderText("name@company.com")).toBeNull();
    expect(screen.queryByRole("button", { name: /send invite/i })).toBeNull();
  });
});
