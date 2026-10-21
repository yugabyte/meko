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

/** Whether the mode takes an allowlist: every mode with a load balancer. On an
 *  ingress_* mode the controller accepts one only when the deployment declares
 *  that its shards see client addresses (--deploy-ingress-source-filtering)
 *  and otherwise refuses it with a 400 naming the flag, which the form surfaces. */
export function takesAllowedCidrs(mode: EndpointConnectivityMode): boolean {
  return mode !== "cluster_ip";
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
            // than only hiding the input. Submit drops the block on cluster_ip,
            // but the wizard's Review row prints any typed CIDRs whatever the mode.
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
  const external = value.mode === "nlb_external" || value.mode === "ingress_external";
  if (!external || value.allowedCidrs.trim()) return null;
  return (
    <p className="mt-1.5 text-[11px] text-amber-600 dark:text-amber-500">
      An external load balancer with no allowed CIDRs puts Postgres on the public internet, with
      password authentication as the only barrier.
    </p>
  );
}
