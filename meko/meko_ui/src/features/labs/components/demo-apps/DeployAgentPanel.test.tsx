import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { useState } from "react";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import {
  confirmByocDeployment,
  createManagedDeployment,
  initByocDeployment,
} from "@/lib/api/deploymentsApi";
import { useDeploymentStatus } from "../../hooks/useDeployments";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import { DeployAgentPanel } from "./DeployAgentPanel";

vi.mock("@/lib/api/deploymentsApi", () => ({
  initByocDeployment: vi.fn(),
  confirmByocDeployment: vi.fn(),
  createManagedDeployment: vi.fn(),
  getDeployment: vi.fn().mockResolvedValue({
    id: "dep-1",
    runtime: "agentcore",
    account_mode: "managed",
    agent_slug: "adlc-review-agent",
    status: "deploying",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
  }),
}));
vi.mock("@/hooks/use-toast", () => ({ toast: vi.fn() }));

const mockInitByoc = vi.mocked(initByocDeployment);
const mockConfirmByoc = vi.mocked(confirmByocDeployment);
const mockCreateManaged = vi.mocked(createManagedDeployment);

// DeployAgentPanel only touches session.deployment/session.selectDeployment
// -- a minimal harness wiring those two through the real useDeploymentStatus
// hook (getDeployment is already mocked above) is enough, no need to fill
// in the full useLabRunnerSession shape.
function Harness(props: { agentSlug: string; agentDisplayName: string }) {
  const [deploymentId, setDeploymentId] = useState<string | null>(null);
  const { data: deployment } = useDeploymentStatus(deploymentId);
  const session = {
    selectDeployment: setDeploymentId,
    clearDeployment: () => setDeploymentId(null),
    deployment: deployment ?? null,
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

function renderPanel(props: { agentSlug: string; agentDisplayName: string }) {
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

  it("managed mode: selecting AgentCore + managed and deploying calls createManagedDeployment with the agent slug", async () => {
    mockCreateManaged.mockResolvedValue({
      id: "dep-1",
      runtime: "agentcore",
      account_mode: "managed",
      agent_slug: "adlc-review-agent",
      status: "deploying",
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:00:00Z",
    });
    renderPanel({ agentSlug: "adlc-review-agent", agentDisplayName: "AgentK" });

    fireEvent.click(screen.getByText("AWS Bedrock AgentCore"));
    fireEvent.click(screen.getByText("Meko's managed AWS account"));
    fireEvent.click(screen.getByRole("button", { name: /Deploy to Meko's managed AWS account/ }));

    await waitFor(() => expect(mockCreateManaged).toHaveBeenCalledWith("adlc-review-agent"));
    expect(await screen.findByText("Deploying AgentK")).toBeTruthy();
  });

  it("BYOC mode: full 3-step flow calls initByocDeployment then confirmByocDeployment with the pasted role ARN", async () => {
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

    fireEvent.click(screen.getByText("AWS Bedrock AgentCore"));
    fireEvent.click(screen.getByText("Your own AWS account (BYOC)"));
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
    mockCreateManaged.mockResolvedValue({
      id: "dep-1",
      runtime: "agentcore",
      account_mode: "managed",
      agent_slug: "adlc-review-agent",
      status: "failed",
      status_message: "AgentCore quota exceeded",
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:00:00Z",
    });
    renderPanel({ agentSlug: "adlc-review-agent", agentDisplayName: "AgentK" });

    fireEvent.click(screen.getByText("AWS Bedrock AgentCore"));
    fireEvent.click(screen.getByText("Meko's managed AWS account"));
    fireEvent.click(screen.getByRole("button", { name: /Deploy to Meko's managed AWS account/ }));

    expect(await screen.findByText("Couldn't deploy AgentK")).toBeTruthy();
    expect(screen.getByText("AgentCore quota exceeded")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(screen.getByText("Choose a runtime")).toBeTruthy();
  });
});
