// ConnectivityField — choose how clients reach an endpoint's Postgres port, and
// (for the load-balanced modes) which sources may. Presentation-only; emits
// { mode, allowedCidrs }. Shared by the create-cluster wizard and the
// provision-compute row so the two cannot drift on the wording or the warning.

import { cn } from "@/lib/utils";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { DEFAULT_ENDPOINT_CONNECTIVITY, type EndpointConnectivityMode } from "@/lib/api/types";

export type Connectivity = {
  mode: EndpointConnectivityMode;
  /** Raw comma-separated input, kept as typed so a half-finished entry isn't
   *  reformatted under the cursor. `parseCidrs` splits it at submit. */
  allowedCidrs: string;
};

export const DEFAULT_CONNECTIVITY: Connectivity = {
  mode: DEFAULT_ENDPOINT_CONNECTIVITY,
  allowedCidrs: "",
};

/** Split the field into the array the API takes. Empty entries are dropped, so a
 *  trailing comma is not an empty CIDR the controller would reject. */
export function parseCidrs(raw: string): string[] {
  return raw
    .split(",")
    .map((c) => c.trim())
    .filter(Boolean);
}

/** Whether the mode takes an allowlist, which is NOT the same question as
 *  whether it has a load balancer. An ingress endpoint has one but shares it, so
 *  `loadBalancerSourceRanges` — a per-Service field — cannot express a
 *  per-endpoint allowlist; the controller refuses `allowed_cidrs` there with a
 *  400 until the shard's own middleware does the filtering. Offering the field
 *  anyway would collect a value the API rejects. */
export function takesAllowedCidrs(mode: EndpointConnectivityMode): boolean {
  return mode === "nlb_internal" || mode === "nlb_external";
}

/** The label shown for each mode, also used by the wizard's Review step. */
export const CONNECTIVITY_LABELS: Record<EndpointConnectivityMode, string> = {
  cluster_ip: "in-cluster",
  ingress_internal: "shared ingress (internal)",
  ingress_external: "shared ingress (external)",
  nlb_internal: "NLB (internal)",
  nlb_external: "NLB (external)",
};

export function ConnectivityField({
  value,
  onChange,
  layout = "stacked",
  className,
}: {
  value: Connectivity;
  onChange: (v: Connectivity) => void;
  /** "row" matches the inline provision-compute controls; "stacked" the wizard. */
  layout?: "row" | "stacked";
  className?: string;
}) {
  const cidrs = takesAllowedCidrs(value.mode);
  const row = layout === "row";

  return (
    <div className={cn(row ? "contents" : "space-y-3", className)}>
      <div className={cn("flex flex-col gap-1", !row && "space-y-1.5")}>
        <Label
          className={cn(
            row ? "text-[10px] uppercase tracking-wide text-muted-foreground" : "text-xs",
          )}
        >
          Connectivity
        </Label>
        <Select
          value={value.mode}
          onValueChange={(v) => {
            const mode = v as EndpointConnectivityMode;
            // Drop the allowlist when the new mode does not take one, rather
            // than only hiding the input. The value lives in the caller's state
            // and both submit paths send it unconditionally, so a CIDR typed on
            // an nlb_* mode would otherwise still be posted after switching to
            // an ingress_* one — earning a 400 that names a field no longer on
            // screen. Not a problem before this commit: cluster_ip was the only
            // mode hiding the input, and its connectivity block is dropped whole.
            onChange({
              ...value,
              mode,
              allowedCidrs: takesAllowedCidrs(mode) ? value.allowedCidrs : "",
            });
          }}
        >
          <SelectTrigger className={row ? "h-8 w-36" : undefined}>
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {(Object.keys(CONNECTIVITY_LABELS) as EndpointConnectivityMode[]).map((m) => (
              <SelectItem key={m} value={m}>
                {CONNECTIVITY_LABELS[m]}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>

      {cidrs && (
        <div className={cn("flex flex-col gap-1", !row && "space-y-1.5")}>
          <Label
            htmlFor="connectivity-cidrs"
            className={cn(
              row ? "text-[10px] uppercase tracking-wide text-muted-foreground" : "text-xs",
            )}
          >
            Allowed CIDRs
          </Label>
          <Input
            id="connectivity-cidrs"
            value={value.allowedCidrs}
            onChange={(e) => onChange({ ...value, allowedCidrs: e.target.value })}
            placeholder="10.0.0.0/16, 1.2.3.4/32"
            className={cn("font-mono", row ? "h-8 w-56 text-xs" : undefined)}
          />
        </div>
      )}
    </div>
  );
}

/** The warning for an external load balancer left open to everything. Rendered
 *  by the caller, so each layout can place it where its own errors go. */
export function ConnectivityWarning({ value }: { value: Connectivity }) {
  // ingress_external is unconditional: it takes no allowlist at all, so there is
  // no entry the user could make that would narrow it.
  if (value.mode === "ingress_external") {
    return (
      <p className="mt-1.5 text-[11px] text-amber-600 dark:text-amber-500">
        An external ingress shard puts Postgres on the public internet, with password authentication
        as the only barrier. Per-endpoint allowlists are not available on the shared modes yet — use
        NLB (external) for an endpoint that needs one.
      </p>
    );
  }
  if (value.mode !== "nlb_external" || value.allowedCidrs.trim()) return null;
  return (
    <p className="mt-1.5 text-[11px] text-amber-600 dark:text-amber-500">
      An external load balancer with no allowed CIDRs puts Postgres on the public internet, with
      password authentication as the only barrier.
    </p>
  );
}
