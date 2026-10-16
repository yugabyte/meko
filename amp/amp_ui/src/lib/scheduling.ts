// Endpoint scheduling (tolerations + topology spread constraints): the editable
// draft, its conversion to and from the API shapes, and the checks the
// controller applies, so a bad list is caught at the form rather than as a 400.

import type {
  LabelSelectorRequirement,
  TaintEffect,
  Toleration,
  TolerationOperator,
  TopologySpreadConstraint,
  WhenUnsatisfiable,
} from "@/lib/api/types";

export const ZONE_TOPOLOGY_KEY = "topology.kubernetes.io/zone";
export const CLUSTER_ID_LABEL = "amp.yugabyte.com/cluster-id";
export const STORAGE_POOL_ID_LABEL = "amp.yugabyte.com/storage-pool-id";

export const TOLERATION_OPERATORS: TolerationOperator[] = ["Equal", "Exists"];
export const TAINT_EFFECTS: TaintEffect[] = ["NoSchedule", "PreferNoSchedule", "NoExecute"];
export const WHEN_UNSATISFIABLE: WhenUnsatisfiable[] = ["ScheduleAnyway", "DoNotSchedule"];
const SELECTOR_OPERATORS = ["In", "NotIn", "Exists", "DoesNotExist"];
const NODE_POLICIES = ["Honor", "Ignore"];

export type TolerationRow = {
  key: string;
  operator: TolerationOperator;
  value: string;
  /** "" matches every effect. */
  effect: TaintEffect | "";
  seconds: string;
};

export type LabelRow = { key: string; value: string };

/** The fields a spread constraint can carry that have no form control. */
export type SpreadExtra = Omit<
  TopologySpreadConstraint,
  "maxSkew" | "topologyKey" | "whenUnsatisfiable" | "labelSelector"
> & { matchExpressions?: LabelSelectorRequirement[] };

export type SpreadRow = {
  maxSkew: string;
  topologyKey: string;
  whenUnsatisfiable: WhenUnsatisfiable;
  labels: LabelRow[];
  /** Carried through a form edit untouched, so editing a constraint that has
   *  matchExpressions or minDomains does not silently drop them. Edited in JSON. */
  extra: SpreadExtra;
};

export type SchedulingDraft = {
  mode: "form" | "json";
  tolerations: TolerationRow[];
  spread: SpreadRow[];
  json: string;
};

export type SchedulingValue = {
  tolerations: Toleration[];
  topology_spread_constraints: TopologySpreadConstraint[];
};

type Result<T> = { ok: true; value: T } | { ok: false; error: string };

export const EMPTY_SCHEDULING: SchedulingDraft = {
  mode: "form",
  tolerations: [],
  spread: [],
  json: "",
};

export function newTolerationRow(): TolerationRow {
  return { key: "", operator: "Equal", value: "", effect: "NoSchedule", seconds: "" };
}

/** ScheduleAnyway: a single-pod endpoint left Pending by an unsatisfiable
 *  constraint is an outage, so the strict mode is something to opt into. */
export function newSpreadRow(selector: Record<string, string>): SpreadRow {
  return {
    maxSkew: "1",
    topologyKey: ZONE_TOPOLOGY_KEY,
    whenUnsatisfiable: "ScheduleAnyway",
    labels: Object.entries(selector).map(([key, value]) => ({ key, value })),
    extra: {},
  };
}

export function draftOf(
  tolerations?: Toleration[],
  constraints?: TopologySpreadConstraint[],
): SchedulingDraft {
  return {
    mode: "form",
    tolerations: (tolerations ?? []).map((t) => ({
      key: t.key ?? "",
      operator: t.operator ?? "Equal",
      value: t.value ?? "",
      effect: t.effect ?? "",
      seconds: t.tolerationSeconds === undefined ? "" : String(t.tolerationSeconds),
    })),
    spread: (constraints ?? []).map((c) => {
      const { maxSkew, topologyKey, whenUnsatisfiable, labelSelector, ...rest } = c;
      const extra: SpreadExtra = { ...rest };
      if (labelSelector?.matchExpressions?.length) {
        extra.matchExpressions = labelSelector.matchExpressions;
      }
      return {
        maxSkew: String(maxSkew),
        topologyKey,
        whenUnsatisfiable,
        labels: Object.entries(labelSelector?.matchLabels ?? {}).map(([key, value]) => ({
          key,
          value,
        })),
        extra,
      };
    }),
    json: "",
  };
}

const isInt = (s: string) => /^-?\d+$/.test(s.trim());

function tolerationOf(r: TolerationRow): Result<Toleration> {
  const t: Toleration = { operator: r.operator };
  const key = r.key.trim();
  if (key) t.key = key;
  if (r.operator === "Equal" && r.value.trim()) t.value = r.value.trim();
  if (r.effect) t.effect = r.effect;
  if (r.seconds.trim()) {
    if (!isInt(r.seconds)) return { ok: false, error: "tolerationSeconds must be a whole number" };
    t.tolerationSeconds = Number(r.seconds.trim());
  }
  return { ok: true, value: t };
}

function spreadOf(r: SpreadRow): Result<TopologySpreadConstraint> {
  if (!isInt(r.maxSkew)) return { ok: false, error: "maxSkew must be a whole number" };
  const matchLabels: Record<string, string> = {};
  for (const l of r.labels) {
    const key = l.key.trim();
    if (!key && !l.value.trim()) continue;
    if (!key) return { ok: false, error: `label value "${l.value}" has no key` };
    if (key in matchLabels) return { ok: false, error: `label ${key} appears twice` };
    matchLabels[key] = l.value.trim();
  }
  const { matchExpressions, ...rest } = r.extra;
  return {
    ok: true,
    value: {
      ...rest,
      maxSkew: Number(r.maxSkew.trim()),
      topologyKey: r.topologyKey.trim(),
      whenUnsatisfiable: r.whenUnsatisfiable,
      labelSelector: {
        ...(Object.keys(matchLabels).length ? { matchLabels } : {}),
        ...(matchExpressions?.length ? { matchExpressions } : {}),
      },
    },
  };
}

export function validateToleration(t: Toleration): string | null {
  const op = t.operator ?? "Equal";
  if (!TOLERATION_OPERATORS.includes(op)) return `operator ${op} must be Equal or Exists`;
  if (!t.key && op !== "Exists") return "an empty key needs operator Exists";
  if (op === "Exists" && t.value) return "value must be empty with operator Exists";
  if (t.effect !== undefined && !TAINT_EFFECTS.includes(t.effect)) {
    return `effect ${t.effect} must be NoSchedule, PreferNoSchedule or NoExecute`;
  }
  if (t.tolerationSeconds !== undefined) {
    if (!Number.isInteger(t.tolerationSeconds)) return "tolerationSeconds must be a whole number";
    if (t.effect !== "NoExecute") return "tolerationSeconds applies only with effect NoExecute";
  }
  return null;
}

export function validateSpread(c: TopologySpreadConstraint): string | null {
  if (!Number.isInteger(c.maxSkew) || c.maxSkew < 1) return "maxSkew must be at least 1";
  if (!c.topologyKey) return "topologyKey is required";
  if (!WHEN_UNSATISFIABLE.includes(c.whenUnsatisfiable)) {
    return "whenUnsatisfiable must be DoNotSchedule or ScheduleAnyway";
  }
  const labels = c.labelSelector?.matchLabels ?? {};
  const exprs = c.labelSelector?.matchExpressions ?? [];
  if (Object.keys(labels).length === 0 && exprs.length === 0) {
    return `the label selector must select something, e.g. ${CLUSTER_ID_LABEL}`;
  }
  for (const k of Object.keys(labels)) if (!k) return "a matchLabels key is empty";
  for (const [i, e] of exprs.entries()) {
    if (!e.key) return `matchExpressions[${i}] needs a key`;
    if (!SELECTOR_OPERATORS.includes(e.operator)) {
      return `matchExpressions[${i}] operator must be In, NotIn, Exists or DoesNotExist`;
    }
    const n = e.values?.length ?? 0;
    if ((e.operator === "In" || e.operator === "NotIn") && n === 0) {
      return `matchExpressions[${i}]: ${e.operator} needs values`;
    }
    if ((e.operator === "Exists" || e.operator === "DoesNotExist") && n > 0) {
      return `matchExpressions[${i}]: ${e.operator} takes no values`;
    }
  }
  if (c.minDomains !== undefined) {
    if (!Number.isInteger(c.minDomains) || c.minDomains < 1) return "minDomains must be at least 1";
    if (c.whenUnsatisfiable !== "DoNotSchedule") {
      return "minDomains applies only with whenUnsatisfiable DoNotSchedule";
    }
  }
  for (const p of ["nodeAffinityPolicy", "nodeTaintsPolicy"] as const) {
    if (c[p] !== undefined && !NODE_POLICIES.includes(c[p])) return `${p} must be Honor or Ignore`;
  }
  return null;
}

function validate(v: SchedulingValue): Result<SchedulingValue> {
  for (const [i, t] of v.tolerations.entries()) {
    const err = validateToleration(t);
    if (err) return { ok: false, error: `Toleration ${i + 1}: ${err}` };
  }
  const seen = new Set<string>();
  for (const [i, c] of v.topology_spread_constraints.entries()) {
    const err = validateSpread(c);
    if (err) return { ok: false, error: `Spread constraint ${i + 1}: ${err}` };
    const pair = `${c.topologyKey}\u0000${c.whenUnsatisfiable}`;
    if (seen.has(pair)) {
      return {
        ok: false,
        error: `Spread constraint ${i + 1}: duplicate topologyKey ${c.topologyKey} with ${c.whenUnsatisfiable}`,
      };
    }
    seen.add(pair);
  }
  return { ok: true, value: v };
}

function parseJson(text: string): Result<SchedulingValue> {
  if (!text.trim())
    return { ok: true, value: { tolerations: [], topology_spread_constraints: [] } };
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch (e) {
    return { ok: false, error: `Invalid JSON: ${e instanceof Error ? e.message : String(e)}` };
  }
  if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
    return {
      ok: false,
      error: "Expected an object with tolerations and topology_spread_constraints",
    };
  }
  const {
    tolerations = [],
    topology_spread_constraints = [],
    ...unknown
  } = parsed as Record<string, unknown>;
  const stray = Object.keys(unknown);
  if (stray.length) return { ok: false, error: `Unknown field: ${stray.join(", ")}` };
  if (!Array.isArray(tolerations) || !Array.isArray(topology_spread_constraints)) {
    return { ok: false, error: "tolerations and topology_spread_constraints must be arrays" };
  }
  return {
    ok: true,
    value: {
      tolerations: tolerations as Toleration[],
      topology_spread_constraints: topology_spread_constraints as TopologySpreadConstraint[],
    },
  };
}

/** The lists the draft means, validated. */
export function buildScheduling(d: SchedulingDraft): Result<SchedulingValue> {
  if (d.mode === "json") {
    const parsed = parseJson(d.json);
    return parsed.ok ? validate(parsed.value) : parsed;
  }
  const tolerations: Toleration[] = [];
  for (const [i, r] of d.tolerations.entries()) {
    const t = tolerationOf(r);
    if (!t.ok) return { ok: false, error: `Toleration ${i + 1}: ${t.error}` };
    tolerations.push(t.value);
  }
  const constraints: TopologySpreadConstraint[] = [];
  for (const [i, r] of d.spread.entries()) {
    const c = spreadOf(r);
    if (!c.ok) return { ok: false, error: `Spread constraint ${i + 1}: ${c.error}` };
    constraints.push(c.value);
  }
  return validate({ tolerations, topology_spread_constraints: constraints });
}

/** Switch the draft's editor. Fails, leaving the draft as it is, when what is
 *  there now does not parse — converting it would lose the operator's input. */
export function switchMode(
  d: SchedulingDraft,
  mode: SchedulingDraft["mode"],
): Result<SchedulingDraft> {
  if (mode === d.mode) return { ok: true, value: d };
  const built = buildScheduling(d);
  if (!built.ok) return built;
  if (mode === "json") {
    return { ok: true, value: { ...d, mode, json: JSON.stringify(built.value, null, 2) } };
  }
  return {
    ok: true,
    value: draftOf(built.value.tolerations, built.value.topology_spread_constraints),
  };
}

export function describeToleration(t: Toleration): string {
  const op = t.operator ?? "Equal";
  const match = !t.key ? "*" : op === "Exists" ? t.key : `${t.key}=${t.value ?? ""}`;
  const effect = t.effect ? `:${t.effect}` : "";
  const secs = t.tolerationSeconds !== undefined ? ` (${t.tolerationSeconds}s)` : "";
  return `${match}${effect}${secs}`;
}

export function describeSpread(c: TopologySpreadConstraint): string {
  const sel = [
    ...Object.entries(c.labelSelector?.matchLabels ?? {}).map(([k, v]) => `${k}=${v}`),
    ...(c.labelSelector?.matchExpressions ?? []).map(
      (e) => `${e.key} ${e.operator}${e.values?.length ? ` (${e.values.join(",")})` : ""}`,
    ),
  ].join(", ");
  return `${c.topologyKey} skew ${c.maxSkew} ${c.whenUnsatisfiable} [${sel}]`;
}
