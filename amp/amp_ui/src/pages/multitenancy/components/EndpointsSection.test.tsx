import { describe, it, expect, afterEach } from "vitest";
import { render, screen, cleanup, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { EndpointsSection } from "./EndpointsSection";

// Provisioning with a unit-less memory reserves BYTES, and the endpoint it
// creates can never be resized in place afterwards (meko#492). The controller
// answers 400, and this form must not send one in the first place.
afterEach(cleanup);

function renderSection() {
  const qc = new QueryClient();
  return render(
    <QueryClientProvider client={qc}>
      <EndpointsSection
        poolId="pool-1"
        clusterId="cluster-1"
        branchId="branch-1"
        endpoints={[]}
        loading={false}
        isDefaultBranch
      />
    </QueryClientProvider>,
  );
}

describe("EndpointsSection", () => {
  it("blocks provisioning while the memory quantity has no unit", () => {
    renderSection();
    const btn = screen.getByRole("button", { name: /provision compute/i }) as HTMLButtonElement;
    const memory = screen.getByLabelText(/memory/i);

    // Pre-filled with the controller's default shape, which clears the floor.
    expect(btn.disabled).toBe(false);

    fireEvent.change(memory, { target: { value: "4" } });
    expect(btn.disabled).toBe(true);
    expect(memory.getAttribute("aria-invalid")).toBe("true");
    expect(screen.getByText(/needs a unit/i)).toBeTruthy();

    fireEvent.change(memory, { target: { value: "16Gi" } });
    expect(btn.disabled).toBe(false);
    expect(screen.queryByText(/needs a unit/i)).toBeNull();
  });
});
