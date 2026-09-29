import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";

import { DeploymentModeChooser } from "@/features/labs/components/demo-apps/deploy/DeploymentModeChooser";

afterEach(cleanup);

describe("DeploymentModeChooser", () => {
  it("clicking the shared card calls onSelect with 'shared'", () => {
    const onSelect = vi.fn();
    render(<DeploymentModeChooser selected="dedicated" onSelect={onSelect} canDeployDedicated />);
    fireEvent.click(screen.getByText("Use the predeployed AgentK"));
    expect(onSelect).toHaveBeenCalledWith("shared");
  });

  it("clicking the dedicated card calls onSelect with 'dedicated' when allowed", () => {
    const onSelect = vi.fn();
    render(<DeploymentModeChooser selected="shared" onSelect={onSelect} canDeployDedicated />);
    fireEvent.click(screen.getByText("Deploy your own dedicated AgentK"));
    expect(onSelect).toHaveBeenCalledWith("dedicated");
  });

  it("renders the dedicated card as disabled, with no clickable role, when not allowed", () => {
    const onSelect = vi.fn();
    render(<DeploymentModeChooser selected="shared" onSelect={onSelect} canDeployDedicated={false} />);

    const card = screen.getByText("Deploy your own dedicated AgentK").closest("[aria-disabled]");
    expect(card?.getAttribute("aria-disabled")).toBe("true");
    expect(screen.getByText("Deploy your own dedicated AgentK").closest('[role="button"]')).toBeNull();

    fireEvent.click(screen.getByText("Deploy your own dedicated AgentK"));
    expect(onSelect).not.toHaveBeenCalled();
  });

  it("collapses to a one-line summary once a card is picked, hiding the other option", () => {
    // onSelect doesn't feed back into `selected` here (it's a bare mock,
    // not real state) -- pass the value that matches what gets clicked so
    // the collapsed summary (which reads the `selected` prop) is coherent.
    render(<DeploymentModeChooser selected="shared" onSelect={vi.fn()} canDeployDedicated />);
    fireEvent.click(screen.getByText("Use the predeployed AgentK"));

    expect(screen.queryByText("Deploy your own dedicated AgentK")).toBeNull();
    expect(screen.getByText("Use the predeployed AgentK")).toBeTruthy();
    expect(screen.getByText("Change")).toBeTruthy();
  });

  it("'Change' re-expands both options", () => {
    render(<DeploymentModeChooser selected="shared" onSelect={vi.fn()} canDeployDedicated />);
    fireEvent.click(screen.getByText("Use the predeployed AgentK"));
    fireEvent.click(screen.getByRole("button", { name: /change/i }));

    expect(screen.getByText("Deploy your own dedicated AgentK")).toBeTruthy();
    expect(screen.queryByText("Change")).toBeNull();
  });
});
