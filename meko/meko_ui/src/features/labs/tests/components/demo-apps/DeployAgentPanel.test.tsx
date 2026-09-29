import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { useState } from "react";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import { confirmByocDeployment, getSharedDeployment, initByocDeployment } from "@/lib/api/deploymentsApi";
import { useDeploymentStatus } from "@/features/labs/hooks/useDeployments";
import type { DeploymentMode, useLabRunnerSession } from "@/features/labs/hooks/useLabRunnerSession";
import { DeployAgentPanel } from "@/features/labs/components/demo-apps/DeployAgentPanel";

vi.mock("@/lib/api/deploymentsApi", () => ({
  initByocDeployment: vi.fn(),
  confirmByocDeployment: vi.fn(),
  getDeployment: vi.fn().mockResolvedValue({
    id: "dep-1",
    runtime: "agentcore",
    account_mode: "byoc",
    agent_slug: "adlc-review-agent",
    status: "deploying",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
  }),
  getSharedDeployment: vi.fn(),
}));
vi.mock("@/hooks/use-toast", () => ({ toast: vi.fn() }));

const mockInitByoc = vi.mocked(initByocDeployment);
const mockConfirmByoc = vi.mocked(confirmByocDeployment);
const mockGetSharedDeployment = vi.mocked(getSharedDeployment);

// DeployAgentPanel touches session.deployment/selectDeployment (dedicated
// mode) plus deploymentMode/selectDeploymentMode/currentUser (mode chooser
// + tier gate) -- a harness wiring those through real state/hooks (rather
// than a full fakeSession stub) exercises the actual mode-switch behavior.
function Harness(props: { agentSlug: string; agentDisplayName: string; userType?: string }) {
  const [deploymentId, setDeploymentId] = useState<string | null>(null);
  const [deploymentMode, setDeploymentMode] = useState<DeploymentMode>("shared");
  const { data: deployment } = useDeploymentStatus(deploymentId);
  const session = {
    currentUser: { user_type: props.userType ?? "pro" },
    selectDeployment: setDeploymentId,
    clearDeployment: () => setDeploymentId(null),
    deployment: deployment ?? null,
    deploymentMode,
    selectDeploymentMode: setDeploymentMode,
  } as unknown as ReturnType<typeof useLabRunnerSession>;
  return (
    <DeployAgentPanel
      agentSlug={props.agentSlug}
      agentDisplayName={props.agentDisplayName}
      sourceRepoUrl="https://github.com/yugabyte/canned-agents/tree/main/adlc-review-agent"
      session={session}
    />
  );
}

function renderPanel(props: { agentSlug: string; agentDisplayName: string; userType?: string }) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <Harness {...props} />
    </QueryClientProvider>,
  );
}

afterEach(cleanup);
beforeEach(() => vi.clearAllMocks());

describe("DeployAgentPanel", () => {
  it("only AgentCore is selectable; Vercel and Lyzr render as disabled 'coming soon' cards", () => {
    renderPanel({ agentSlug: "adlc-review-agent", agentDisplayName: "AgentK" });
    fireEvent.click(screen.getByText("Deploy your own dedicated AgentK"));

    const agentCoreCard = screen.getByText("AWS Bedrock AgentCore").closest('[role="button"]');
    expect(agentCoreCard).not.toBeNull();

    const vercelCard = screen.getByText("Vercel").closest("[aria-disabled]");
    expect(vercelCard?.getAttribute("aria-disabled")).toBe("true");
    const lyzrCard = screen.getByText("Lyzr").closest("[aria-disabled]");
    expect(lyzrCard?.getAttribute("aria-disabled")).toBe("true");

    // Neither disabled card exposes a clickable role, so selecting them is
    // impossible via keyboard/AT navigation, not just visually greyed out.
    expect(screen.getByText("Vercel").closest('[role="button"]')).toBeNull();
    expect(screen.getByText("Lyzr").closest('[role="button"]')).toBeNull();
  });

  it("full BYOC flow calls initByocDeployment then confirmByocDeployment with the pasted role ARN", async () => {
    mockInitByoc.mockResolvedValue({
      id: "dep-2",
      external_id: "ext-abc123",
      trust_policy_json: '{"Version":"2012-10-17"}',
    });
    mockConfirmByoc.mockResolvedValue({
      id: "dep-2",
      runtime: "agentcore",
      account_mode: "byoc",
      agent_slug: "adlc-review-agent",
      status: "deploying",
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:00:00Z",
    });
    renderPanel({ agentSlug: "adlc-review-agent", agentDisplayName: "AgentK" });
    fireEvent.click(screen.getByText("Deploy your own dedicated AgentK"));

    fireEvent.click(screen.getByText("AWS Bedrock AgentCore"));
    fireEvent.click(screen.getByRole("button", { name: "Generate setup instructions" }));

    await waitFor(() => expect(mockInitByoc).toHaveBeenCalledWith("adlc-review-agent"));
    expect(await screen.findByText("ext-abc123")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "I've created the role" }));

    const roleArnInput = screen.getByLabelText("Role ARN");
    fireEvent.change(roleArnInput, { target: { value: "not-a-valid-arn" } });
    expect((screen.getByRole("button", { name: "Deploy" }) as HTMLButtonElement).disabled).toBe(true);

    fireEvent.change(roleArnInput, {
      target: { value: "arn:aws:iam::123456789012:role/meko-agentcore-deploy" },
    });
    expect((screen.getByRole("button", { name: "Deploy" }) as HTMLButtonElement).disabled).toBe(false);

    fireEvent.click(screen.getByRole("button", { name: "Deploy" }));
    await waitFor(() =>
      expect(mockConfirmByoc).toHaveBeenCalledWith("dep-2", "arn:aws:iam::123456789012:role/meko-agentcore-deploy"),
    );
  });

  it("a failed deployment shows a retry that returns to the runtime picker", async () => {
    mockInitByoc.mockResolvedValue({
      id: "dep-3",
      external_id: "ext-def456",
      trust_policy_json: '{"Version":"2012-10-17"}',
    });
    mockConfirmByoc.mockResolvedValue({
      id: "dep-3",
      runtime: "agentcore",
      account_mode: "byoc",
      agent_slug: "adlc-review-agent",
      status: "failed",
      status_message: "Role not assumable",
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:00:00Z",
    });
    renderPanel({ agentSlug: "adlc-review-agent", agentDisplayName: "AgentK" });
    fireEvent.click(screen.getByText("Deploy your own dedicated AgentK"));

    fireEvent.click(screen.getByText("AWS Bedrock AgentCore"));
    fireEvent.click(screen.getByRole("button", { name: "Generate setup instructions" }));
    await screen.findByText("ext-def456");
    fireEvent.click(screen.getByRole("button", { name: "I've created the role" }));
    fireEvent.change(screen.getByLabelText("Role ARN"), {
      target: { value: "arn:aws:iam::123456789012:role/meko-agentcore-deploy" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Deploy" }));

    expect(await screen.findByText("Couldn't deploy AgentK")).toBeTruthy();
    expect(screen.getByText("Role not assumable")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(screen.getByText("Choose a runtime")).toBeTruthy();
  });

  it("shows the mode chooser, defaulting to the shared instance (not the dedicated wizard)", () => {
    renderPanel({ agentSlug: "adlc-review-agent", agentDisplayName: "AgentK" });
    expect(screen.getByText("Use the predeployed AgentK")).toBeTruthy();
    expect(screen.getByText("Deploy your own dedicated AgentK")).toBeTruthy();
    expect(screen.queryByText("Choose a runtime")).toBeNull();
  });

  it("selecting the dedicated card shows the wizard", () => {
    renderPanel({ agentSlug: "adlc-review-agent", agentDisplayName: "AgentK" });
    fireEvent.click(screen.getByText("Deploy your own dedicated AgentK"));
    expect(screen.getByText("Choose a runtime")).toBeTruthy();
  });

  it("switching to shared mode shows the shared instance's status instead of the wizard", async () => {
    mockGetSharedDeployment.mockResolvedValue({
      id: "shared-dep-1",
      runtime: "agentcore",
      account_mode: "managed",
      agent_slug: "adlc-review-agent",
      status: "active",
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:00:00Z",
    });
    renderPanel({ agentSlug: "adlc-review-agent", agentDisplayName: "AgentK" });

    fireEvent.click(screen.getByText("Use the predeployed AgentK"));

    expect(screen.queryByText("Choose a runtime")).toBeNull();
    expect(await screen.findByText(/ready\. click next/i)).toBeTruthy();
  });

  it("shows the shared instance as unavailable when none is configured", async () => {
    mockGetSharedDeployment.mockRejectedValue(new Error("not found"));
    renderPanel({ agentSlug: "adlc-review-agent", agentDisplayName: "AgentK" });

    fireEvent.click(screen.getByText("Use the predeployed AgentK"));

    expect(await screen.findByText(/isn't configured with a shared instance yet/i)).toBeTruthy();
  });

  it("gates the dedicated card behind a paid tier, with an upgrade tooltip", () => {
    renderPanel({ agentSlug: "adlc-review-agent", agentDisplayName: "AgentK", userType: "free" });

    const dedicatedCard = screen.getByText("Deploy your own dedicated AgentK").closest("[aria-disabled]");
    expect(dedicatedCard?.getAttribute("aria-disabled")).toBe("true");
    // Disabled card exposes no clickable role -- selecting it is impossible
    // via keyboard/AT navigation, not just visually greyed out.
    expect(screen.getByText("Deploy your own dedicated AgentK").closest('[role="button"]')).toBeNull();
    // Still on the shared (default) view -- the gated card never activated.
    expect(screen.queryByText("Choose a runtime")).toBeNull();
  });
});
