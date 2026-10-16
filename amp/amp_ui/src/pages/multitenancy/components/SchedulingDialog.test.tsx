import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, cleanup, fireEvent } from "@testing-library/react";
import type { Endpoint } from "@/lib/api/types";
import { SchedulingDialog } from "./SchedulingDialog";

// Either list changing recreates the pod, and the controller reads an omitted
// list as "keep" and `[]` as "clear" — so the dialog sends exactly the lists the
// operator changed, and `[]` for one they emptied.
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

const update = vi.fn();
vi.mock("@/hooks/useEndpoints", () => ({
  useUpdateEndpointScheduling: () => ({ mutate: update, isPending: false }),
}));
const toastSuccess = vi.fn();
const toastError = vi.fn();
vi.mock("sonner", () => ({
  toast: {
    success: (m: string) => toastSuccess(m),
    error: (m: string) => toastError(m),
  },
}));

const ep: Endpoint = {
  id: "ep-1",
  branch_id: "branch-1",
  cluster_id: "cluster-1",
  role: "read_write",
  address: "",
  tailnet_address: "",
  created_at: "2026-01-01T00:00:00Z",
  tolerations: [{ key: "dedicated", operator: "Equal", value: "db", effect: "NoSchedule" }],
  topology_spread_constraints: [
    {
      maxSkew: 1,
      topologyKey: "topology.kubernetes.io/zone",
      whenUnsatisfiable: "ScheduleAnyway",
      labelSelector: { matchLabels: { "amp.yugabyte.com/cluster-id": "cluster-1" } },
    },
  ],
};

const onOpenChange = vi.fn();
const renderDialog = (e: Endpoint = ep) =>
  render(
    <SchedulingDialog open onOpenChange={onOpenChange} poolId="pool-1" ep={e} title="read-write" />,
  );
const save = () => fireEvent.click(screen.getByRole("button", { name: /save and recreate/i }));
const sent = () => update.mock.calls.at(-1)![0];

describe("SchedulingDialog", () => {
  it("opens showing what is stored, and warns that saving recreates the pod", () => {
    renderDialog();
    expect((screen.getByLabelText("Toleration 1 key") as HTMLInputElement).value).toBe("dedicated");
    expect((screen.getByLabelText("Spread 1 topology key") as HTMLInputElement).value).toBe(
      "topology.kubernetes.io/zone",
    );
    expect(screen.getByText(/recreates this compute's pod/i)).toBeTruthy();
  });

  it("sends nothing when nothing changed", () => {
    renderDialog();
    save();
    expect(update).not.toHaveBeenCalled();
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });

  it("sends only the list that changed", () => {
    renderDialog();
    fireEvent.change(screen.getByLabelText("Toleration 1 value"), { target: { value: "olap" } });
    save();
    expect(sent()).toEqual({
      poolId: "pool-1",
      clusterId: "cluster-1",
      branchId: "branch-1",
      endpointId: "ep-1",
      scheduling: {
        tolerations: [{ key: "dedicated", operator: "Equal", value: "olap", effect: "NoSchedule" }],
      },
    });
  });

  it("sends [] for a list the operator emptied", () => {
    renderDialog();
    fireEvent.click(screen.getByRole("button", { name: "Remove spread constraint 1" }));
    save();
    expect(sent().scheduling).toEqual({ topology_spread_constraints: [] });
  });

  it("will not save an invalid list", () => {
    renderDialog();
    fireEvent.change(screen.getByLabelText("Spread 1 max skew"), { target: { value: "0" } });
    const btn = screen.getByRole("button", { name: /save and recreate/i }) as HTMLButtonElement;
    expect(btn.disabled).toBe(true);
    expect(screen.getByText(/maxSkew must be at least 1/)).toBeTruthy();
  });

  it("surfaces the controller's message on a 400", () => {
    renderDialog();
    fireEvent.change(screen.getByLabelText("Toleration 1 value"), { target: { value: "olap" } });
    save();
    update.mock.calls.at(-1)![1].onError(new Error('tolerations[0]: key "x": bad'));
    expect(toastError).toHaveBeenCalledWith('tolerations[0]: key "x": bad');
  });
});
