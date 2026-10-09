// The Compute section under a branch card: lists provisioned compute instances
// and the controls to provision a new read-write / read-only endpoint.

import { useEffect, useState } from "react";
import { Loader2 } from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { useCreateEndpoint } from "@/hooks/useEndpoints";
import type { Endpoint, EndpointRole } from "@/lib/api/types";
import { DEFAULT_ENDPOINT_CPU, DEFAULT_ENDPOINT_MEMORY } from "@/lib/api/types";
import {
  ConnectivityField,
  ConnectivityWarning,
  DEFAULT_CONNECTIVITY,
  parseCidrs,
  type Connectivity,
} from "@/components/ui/connectivity-field";
import { isValidMemory } from "@/lib/compute";
import { dataBranchingMessages as msg } from "@/lib/messages";
import { ComputeInstance } from "./ComputeInstance";

export function EndpointsSection({
  poolId,
  clusterId,
  branchId,
  endpoints,
  loading,
  isDefaultBranch,
}: {
  poolId: string;
  clusterId: string;
  branchId: string;
  endpoints: Endpoint[];
  loading: boolean;
  isDefaultBranch: boolean;
}) {
  const [role, setRole] = useState<EndpointRole>("read_write");
  // CPU + memory reserved for the compute pod, pre-filled with the controller's
  // default shape (DEFAULT_ENDPOINT_CPU/MEMORY, which follow its memory-per-vCPU
  // ratio). Sent as request == limit → Guaranteed QoS.
  const [cpu, setCpu] = useState(DEFAULT_ENDPOINT_CPU);
  const [memory, setMemory] = useState(DEFAULT_ENDPOINT_MEMORY);
  // How clients reach this compute. The nlb_* modes each provision their own
  // cloud load balancer, so they are opt-in rather than the default.
  const [connectivity, setConnectivity] = useState<Connectivity>(DEFAULT_CONNECTIVITY);
  // The password for the compute's default user. Required for a writer.
  const [password, setPassword] = useState("");
  const createEndpoint = useCreateEndpoint();
  // The controller rejects a memory quantity with no unit; block the click
  // instead of sending one and reporting the 400 back as a toast.
  const memoryValid = isValidMemory(memory);
  // The controller requires a password for a writer and refuses one for a
  // replica, so the button follows the same rule rather than sending a 400.
  const needsPassword = role === "read_write";
  const passwordValid = !needsPassword || password.length > 0;

  // A branch admits at most one read-write compute (multiple read-only are
  // fine). Once a read-write exists, disable that option and force the
  // provisioner to read-only. Same rule for the source branch and every fork,
  // since each branch has its own EndpointsSection + endpoints list.
  const hasReadWrite = endpoints.some((ep) => ep.role === "read_write");
  useEffect(() => {
    if (hasReadWrite && role === "read_write") setRole("read_only");
  }, [hasReadWrite, role]);

  function provision() {
    createEndpoint.mutate(
      {
        poolId,
        clusterId,
        branchId,
        role,
        cpu: cpu.trim() || DEFAULT_ENDPOINT_CPU,
        memory: memory.trim(),
        connectivity: connectivity.mode,
        allowedCidrs: parseCidrs(connectivity.allowedCidrs),
        // Branch-wide, so only the writer may set it; sending one on a
        // read-only endpoint is a 400.
        ...(role === "read_write" ? { password } : {}),
      },
      {
        onSuccess: (endpoint) => {
          toast.success(msg.provisionSuccess(endpoint.role.replace("_", "-")));
          setPassword("");
        },
        onError: (err) => toast.error(err instanceof Error ? err.message : msg.provisionError),
      },
    );
  }

  return (
    <div className="mt-3 border-t border-border pt-3">
      <div className="mb-2 text-xs font-semibold uppercase tracking-wide text-muted-foreground">
        Compute
      </div>
      {loading ? (
        <p className="flex items-center gap-1.5 text-xs text-muted-foreground">
          <Loader2 className="h-3 w-3 animate-spin" />
          Loading compute…
        </p>
      ) : endpoints.length ? (
        <ul className="space-y-2">
          {endpoints.map((ep) => (
            <ComputeInstance
              key={ep.id}
              ep={ep}
              poolId={poolId}
              isDefaultBranch={isDefaultBranch}
            />
          ))}
        </ul>
      ) : (
        <p className="text-sm text-muted-foreground">{msg.noCompute}</p>
      )}

      <div className="mt-3 flex flex-wrap items-end gap-2">
        <Select value={role} onValueChange={(v) => setRole(v as EndpointRole)}>
          <SelectTrigger className="h-8 w-36">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="read_write" disabled={hasReadWrite}>
              read-write
            </SelectItem>
            <SelectItem value="read_only">read-only</SelectItem>
          </SelectContent>
        </Select>
        <div className="flex flex-col gap-1">
          <Label
            htmlFor="ep-cpu"
            className="text-[10px] uppercase tracking-wide text-muted-foreground"
          >
            CPU
          </Label>
          <Input
            id="ep-cpu"
            value={cpu}
            onChange={(e) => setCpu(e.target.value)}
            placeholder={DEFAULT_ENDPOINT_CPU}
            className="h-8 w-20 font-mono text-xs"
          />
        </div>
        <div className="flex flex-col gap-1">
          <Label
            htmlFor="ep-memory"
            className="text-[10px] uppercase tracking-wide text-muted-foreground"
          >
            Memory
          </Label>
          <Input
            id="ep-memory"
            value={memory}
            onChange={(e) => setMemory(e.target.value)}
            placeholder={DEFAULT_ENDPOINT_MEMORY}
            aria-invalid={!memoryValid}
            aria-describedby={memoryValid ? undefined : "ep-memory-hint"}
            className="h-8 w-24 font-mono text-xs"
          />
        </div>
        {/* Roles live in the branch's catalog, which every endpoint on it shares,
            so only the writer creates one — the controller rejects these on a
            read-only endpoint rather than rotating the writer's credential. */}
        {role === "read_write" && (
          <div className="flex flex-col gap-1">
            <Label
              htmlFor="ep-password"
              className="text-[10px] uppercase tracking-wide text-muted-foreground"
            >
              Password
            </Label>
            <Input
              id="ep-password"
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              placeholder="required"
              title={msg.passwordHint}
              aria-invalid={!passwordValid}
              className="h-8 w-32 font-mono text-xs"
            />
          </div>
        )}
        <ConnectivityField value={connectivity} onChange={setConnectivity} layout="row" />
        <Button
          variant="outline"
          size="sm"
          onClick={provision}
          disabled={createEndpoint.isPending || !memoryValid || !passwordValid}
        >
          {createEndpoint.isPending ? "Provisioning…" : "Provision compute"}
        </Button>
      </div>
      <ConnectivityWarning value={connectivity} />
      {!memoryValid && (
        <p id="ep-memory-hint" className="mt-1.5 text-[11px] text-destructive">
          {msg.memoryNeedsUnit}
        </p>
      )}
      {!passwordValid && (
        <p className="mt-1.5 text-[11px] text-destructive">{msg.passwordRequired}</p>
      )}
      {hasReadWrite && (
        <p className="mt-1.5 text-[11px] text-muted-foreground">
          This branch already has a read-write compute — additional compute must be read-only.
        </p>
      )}
    </div>
  );
}
