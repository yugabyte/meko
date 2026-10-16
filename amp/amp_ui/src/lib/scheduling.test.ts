import { describe, it, expect } from "vitest";
import {
  buildScheduling,
  draftOf,
  EMPTY_SCHEDULING,
  newSpreadRow,
  newTolerationRow,
  switchMode,
  type SchedulingDraft,
} from "@/lib/scheduling";
import type { TopologySpreadConstraint } from "@/lib/api/types";

const form = (patch: Partial<SchedulingDraft>): SchedulingDraft => ({
  ...EMPTY_SCHEDULING,
  ...patch,
});

const err = (d: SchedulingDraft) => {
  const r = buildScheduling(d);
  return r.ok ? null : r.error;
};

describe("buildScheduling", () => {
  it("builds nothing from an empty draft", () => {
    expect(buildScheduling(EMPTY_SCHEDULING)).toEqual({
      ok: true,
      value: { tolerations: [], topology_spread_constraints: [] },
    });
  });

  it("serialises a toleration with Kubernetes field names, omitting blanks", () => {
    const r = buildScheduling(
      form({
        tolerations: [
          { key: "dedicated", operator: "Equal", value: "db", effect: "NoSchedule", seconds: "" },
          { key: "", operator: "Exists", value: "", effect: "", seconds: "" },
          {
            key: "node.kubernetes.io/unreachable",
            operator: "Exists",
            value: "",
            effect: "NoExecute",
            seconds: "300",
          },
        ],
      }),
    );
    expect(r.ok && r.value.tolerations).toEqual([
      { key: "dedicated", operator: "Equal", value: "db", effect: "NoSchedule" },
      { operator: "Exists" },
      {
        key: "node.kubernetes.io/unreachable",
        operator: "Exists",
        effect: "NoExecute",
        tolerationSeconds: 300,
      },
    ]);
  });

  // Each mirrors a refusal in the controller's validateToleration.
  it("applies the controller's toleration rules", () => {
    expect(err(form({ tolerations: [newTolerationRow()] }))).toMatch(
      /empty key needs operator Exists/,
    );
    expect(
      err(
        form({
          tolerations: [{ ...newTolerationRow(), key: "k", effect: "NoSchedule", seconds: "30" }],
        }),
      ),
    ).toMatch(/only with effect NoExecute/);
    expect(
      err(
        form({
          tolerations: [{ ...newTolerationRow(), key: "k", effect: "NoExecute", seconds: "x" }],
        }),
      ),
    ).toMatch(/whole number/);
    expect(
      err(
        form({
          mode: "json",
          json: '{"tolerations":[{"key":"k","operator":"Exists","value":"v"}]}',
        }),
      ),
    ).toMatch(/value must be empty/);
  });

  it("prefills a spread constraint that passes as is", () => {
    const r = buildScheduling(
      form({ spread: [newSpreadRow({ "amp.yugabyte.com/cluster-id": "c1" })] }),
    );
    expect(r.ok && r.value.topology_spread_constraints).toEqual([
      {
        maxSkew: 1,
        topologyKey: "topology.kubernetes.io/zone",
        whenUnsatisfiable: "ScheduleAnyway",
        labelSelector: { matchLabels: { "amp.yugabyte.com/cluster-id": "c1" } },
      },
    ]);
  });

  it("applies the controller's spread rules", () => {
    const row = newSpreadRow({ a: "b" });
    expect(err(form({ spread: [{ ...row, maxSkew: "0" }] }))).toMatch(/maxSkew must be at least 1/);
    expect(err(form({ spread: [{ ...row, topologyKey: " " }] }))).toMatch(
      /topologyKey is required/,
    );
    // Each endpoint is one pod, so an empty selector matches nothing and the
    // controller refuses it.
    expect(err(form({ spread: [{ ...row, labels: [] }] }))).toMatch(/must select something/);
    expect(err(form({ spread: [row, row] }))).toMatch(/duplicate topologyKey/);
    expect(
      err(
        form({ spread: [{ ...row, extra: { matchExpressions: [{ key: "x", operator: "In" }] } }] }),
      ),
    ).toMatch(/In needs values/);
    expect(
      err(
        form({
          spread: [
            {
              ...row,
              extra: { matchExpressions: [{ key: "x", operator: "Exists", values: ["y"] }] },
            },
          ],
        }),
      ),
    ).toMatch(/Exists takes no values/);
    expect(err(form({ spread: [{ ...row, extra: { minDomains: 2 } }] }))).toMatch(
      /only with whenUnsatisfiable DoNotSchedule/,
    );
  });
});

describe("draftOf", () => {
  // Fields the form has no control for must survive a form edit untouched.
  it("round-trips a stored constraint, including fields the form does not show", () => {
    const stored: TopologySpreadConstraint[] = [
      {
        maxSkew: 2,
        topologyKey: "kubernetes.io/hostname",
        whenUnsatisfiable: "DoNotSchedule",
        labelSelector: {
          matchLabels: { "amp.yugabyte.com/cluster-id": "c1" },
          matchExpressions: [{ key: "amp.yugabyte.com/ep-type", operator: "In", values: ["rw"] }],
        },
        minDomains: 2,
        matchLabelKeys: ["pod-template-hash"],
      },
    ];
    const r = buildScheduling(draftOf(undefined, stored));
    expect(r.ok && r.value.topology_spread_constraints).toEqual(stored);
  });
});

describe("switchMode", () => {
  it("carries the draft into JSON and back", () => {
    const d = form({
      tolerations: [{ ...newTolerationRow(), key: "dedicated", value: "db" }],
      spread: [newSpreadRow({ a: "b" })],
    });
    const json = switchMode(d, "json");
    expect(json.ok).toBe(true);
    if (!json.ok) return;
    expect(JSON.parse(json.value.json).tolerations[0].key).toBe("dedicated");
    const back = switchMode(json.value, "form");
    expect(back.ok && buildScheduling(back.value)).toEqual(buildScheduling(d));
  });

  it("refuses to switch away from JSON that does not parse", () => {
    const r = switchMode(form({ mode: "json", json: "{" }), "form");
    expect(r.ok).toBe(false);
  });
});
