import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, cleanup, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { DeleteClusterDialog } from "./DeleteClusterDialog";

// Cluster delete cascades through every branch + endpoint and destroys the data,
// so the destructive action must stay disabled until the operator types the exact
// cluster name. A regression here would make a data-destroying cascade one click.
afterEach(cleanup);

function renderDialog() {
  const qc = new QueryClient();
  return render(
    <QueryClientProvider client={qc}>
      <DeleteClusterDialog
        open
        onOpenChange={vi.fn()}
        poolId="pool-1"
        clusterId="cluster-1"
        clusterName="analytics"
      />
    </QueryClientProvider>,
  );
}

describe("DeleteClusterDialog", () => {
  it("keeps the delete action disabled until the exact cluster name is typed", () => {
    renderDialog();
    const btn = screen.getByRole("button", { name: /delete cluster/i }) as HTMLButtonElement;
    const input = screen.getByLabelText(/type the cluster name/i);

    expect(btn.disabled).toBe(true); // empty

    fireEvent.change(input, { target: { value: "analyti" } });
    expect(btn.disabled).toBe(true); // partial name

    fireEvent.change(input, { target: { value: "Analytics" } });
    expect(btn.disabled).toBe(true); // wrong case — exact match required

    fireEvent.change(input, { target: { value: "analytics" } });
    expect(btn.disabled).toBe(false); // exact match enables it

    fireEvent.change(input, { target: { value: "  analytics  " } });
    expect(btn.disabled).toBe(false); // surrounding whitespace is trimmed
  });
});
