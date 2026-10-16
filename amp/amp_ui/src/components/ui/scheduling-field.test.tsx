import { describe, it, expect, vi, afterEach } from "vitest";
import { useState } from "react";
import { render, screen, cleanup, fireEvent } from "@testing-library/react";
import { SchedulingField } from "@/components/ui/scheduling-field";
import { buildScheduling, EMPTY_SCHEDULING, type SchedulingDraft } from "@/lib/scheduling";

afterEach(cleanup);

// Radix Select drives DOM APIs jsdom lacks.
Element.prototype.scrollIntoView = vi.fn();
Element.prototype.hasPointerCapture ??= () => false;
Element.prototype.setPointerCapture ??= () => {};
Element.prototype.releasePointerCapture ??= () => {};

let latest: SchedulingDraft = EMPTY_SCHEDULING;

function Harness({ initial = EMPTY_SCHEDULING }: { initial?: SchedulingDraft }) {
  const [v, setV] = useState(initial);
  latest = v;
  return (
    <SchedulingField
      value={v}
      onChange={setV}
      defaultSelector={{ "amp.yugabyte.com/cluster-id": "cluster-1" }}
    />
  );
}

function pick(trigger: string, option: string) {
  fireEvent.click(screen.getByLabelText(trigger));
  fireEvent.click(screen.getByRole("option", { name: option }));
}

describe("SchedulingField", () => {
  it("prefills a new spread constraint with this cluster's selector", () => {
    render(<Harness />);
    fireEvent.click(screen.getByRole("button", { name: /add spread constraint/i }));
    expect((screen.getByLabelText("Spread 1 label 1 key") as HTMLInputElement).value).toBe(
      "amp.yugabyte.com/cluster-id",
    );
    expect((screen.getByLabelText("Spread 1 label 1 value") as HTMLInputElement).value).toBe(
      "cluster-1",
    );
    expect(screen.queryByRole("alert")).toBeNull();
  });

  // Removing the prefilled selector leaves a constraint the controller refuses.
  it("flags a spread constraint with an empty selector", () => {
    render(<Harness />);
    fireEvent.click(screen.getByRole("button", { name: /add spread constraint/i }));
    fireEvent.click(screen.getByRole("button", { name: "Remove spread 1 label 1" }));
    expect(screen.getByRole("alert").textContent).toMatch(/must select something/);
  });

  it("clears and disables the value when the operator becomes Exists", () => {
    render(<Harness />);
    fireEvent.click(screen.getByRole("button", { name: /add toleration/i }));
    fireEvent.change(screen.getByLabelText("Toleration 1 key"), { target: { value: "dedicated" } });
    fireEvent.change(screen.getByLabelText("Toleration 1 value"), { target: { value: "db" } });
    pick("Toleration 1 operator", "Exists");
    const value = screen.getByLabelText("Toleration 1 value") as HTMLInputElement;
    expect(value.disabled).toBe(true);
    expect(value.value).toBe("");
    const built = buildScheduling(latest);
    expect(built.ok && built.value.tolerations).toEqual([
      { key: "dedicated", operator: "Exists", effect: "NoSchedule" },
    ]);
  });

  it("offers tolerationSeconds only for NoExecute, and drops it on leaving", () => {
    render(<Harness />);
    fireEvent.click(screen.getByRole("button", { name: /add toleration/i }));
    expect(screen.queryByLabelText("Toleration 1 seconds")).toBeNull();
    pick("Toleration 1 effect", "NoExecute");
    fireEvent.change(screen.getByLabelText("Toleration 1 seconds"), { target: { value: "60" } });
    pick("Toleration 1 effect", "NoSchedule");
    expect(screen.queryByLabelText("Toleration 1 seconds")).toBeNull();
    expect(latest.tolerations[0].seconds).toBe("");
  });

  it("flags an empty key with operator Equal", () => {
    render(<Harness />);
    fireEvent.click(screen.getByRole("button", { name: /add toleration/i }));
    expect(screen.getByRole("alert").textContent).toMatch(/empty key needs operator Exists/);
  });

  it("switches to JSON and back without losing anything", () => {
    render(<Harness />);
    fireEvent.click(screen.getByRole("button", { name: /add spread constraint/i }));
    fireEvent.click(screen.getByRole("button", { name: "JSON" }));
    const json = screen.getByLabelText("Scheduling JSON") as HTMLTextAreaElement;
    expect(JSON.parse(json.value).topology_spread_constraints[0].topologyKey).toBe(
      "topology.kubernetes.io/zone",
    );
    fireEvent.change(json, {
      target: {
        value: JSON.stringify({
          topology_spread_constraints: [
            {
              maxSkew: 1,
              topologyKey: "topology.kubernetes.io/zone",
              whenUnsatisfiable: "DoNotSchedule",
              labelSelector: {
                matchExpressions: [{ key: "amp.yugabyte.com/ep-type", operator: "Exists" }],
              },
              minDomains: 2,
            },
          ],
        }),
      },
    });
    fireEvent.click(screen.getByRole("button", { name: "Form" }));
    expect(latest.mode).toBe("form");
    expect(screen.getByText(/also sets minDomains, matchExpressions/i)).toBeTruthy();
    const built = buildScheduling(latest);
    expect(built.ok && built.value.topology_spread_constraints[0].minDomains).toBe(2);
  });

  it("stays in JSON while it does not parse", () => {
    render(<Harness initial={{ ...EMPTY_SCHEDULING, mode: "json", json: "{" }} />);
    fireEvent.click(screen.getByRole("button", { name: "Form" }));
    expect(latest.mode).toBe("json");
    expect(screen.getByRole("alert").textContent).toMatch(/invalid json/i);
  });
});
