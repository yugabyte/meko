// Create-cluster wizard: a multi-step flow (Cluster -> Extensions -> Compute ->
// Review) that creates a cluster and its writer endpoint. It composes the
// reusable components/ui primitives (Stepper, ExtensionsField, ComputeSizeField)
// and fires ONE composite call — createCluster (create_cluster with_writer) —
// which the controller runs as a single background task: cluster + `main` branch
// (seeded with the picked extensions) + read_write endpoint.
// The wizard doesn't wait for it; it toasts and deeplinks to Tasks, so the work
// survives navigating away. No read replica — not supported today.

import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Stepper } from "@/components/ui/stepper";
import { ComputeSizeField, type ComputeSize } from "@/components/ui/compute-size-field";
import {
  ConnectivityField,
  ConnectivityWarning,
  CONNECTIVITY_LABELS,
  DEFAULT_CONNECTIVITY,
  parseCidrs,
  type Connectivity,
} from "@/components/ui/connectivity-field";
import { DB_ENGINES, EngineField, engineExtensions } from "@/components/ui/engine-field";
import { ExtensionsField, useExtensionOptions } from "@/components/ui/extensions-field";
import { useCreateClusterWithWriter } from "@/lib/api/queries";
import { isValidMemory } from "@/lib/compute";
import { DEFAULT_ENDPOINT_CPU, DEFAULT_ENDPOINT_MEMORY, type StoragePool } from "@/lib/api/types";

const STEPS = ["Cluster", "Extensions", "Compute", "Review"];

export function CreateClusterWizard({
  open,
  onOpenChange,
  pools,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  pools: StoragePool[];
}) {
  const [step, setStep] = useState(0);
  const [engine, setEngine] = useState("postgres");
  const [name, setName] = useState("");
  const [poolId, setPoolId] = useState("");
  const [extensions, setExtensions] = useState<string[]>([]);
  // Live catalog, so an image an admin registered is selectable here.
  const extCatalog = useExtensionOptions();
  const [size, setSize] = useState<ComputeSize>({
    cpu: DEFAULT_ENDPOINT_CPU,
    memory: DEFAULT_ENDPOINT_MEMORY,
  });
  const [connectivity, setConnectivity] = useState<Connectivity>(DEFAULT_CONNECTIVITY);

  const navigate = useNavigate();
  const createCluster = useCreateClusterWithWriter();
  const pending = createCluster.isPending;

  useEffect(() => {
    if (open) {
      setStep(0);
      setEngine("postgres");
      setName("");
      setPoolId("");
      setExtensions([]);
      setSize({ cpu: DEFAULT_ENDPOINT_CPU, memory: DEFAULT_ENDPOINT_MEMORY });
      setConnectivity(DEFAULT_CONNECTIVITY);
    }
  }, [open]);

  // The controller unions the engine's bundle into the request's extensions, so
  // the wizard shows them as already-on rather than sending them twice.
  const fromEngine = engineExtensions(engine);
  const engineLabel = DB_ENGINES.find((e) => e.id === engine)?.label ?? engine;
  // What the cluster ends up with: the engine's bundle plus anything picked.
  const allExtensions = [...fromEngine, ...extensions.filter((n) => !fromEngine.includes(n))];
  const poolName = pools.find((p) => p.id === poolId)?.name ?? "";
  const trimmedName = name.trim();
  // The compute step also needs a memory quantity the controller will accept: a
  // unit-less one is a 400, and it would only surface after the review step.
  const stepValid =
    step === 0
      ? Boolean(trimmedName && poolId)
      : step === 2
        ? Boolean(size.cpu.trim()) && isValidMemory(size.memory)
        : true;

  function create() {
    // One composite call: the controller creates the cluster, its `main` branch,
    // and the read_write endpoint as a single background task. We don't wait for
    // it — toast + deeplink to Tasks and let the worker finish independently.
    createCluster.mutate(
      {
        poolId,
        name: trimmedName,
        cpu: size.cpu.trim(),
        memory: size.memory.trim(),
        connectivity: connectivity.mode,
        allowedCidrs: parseCidrs(connectivity.allowedCidrs),
        engine,
        extensions,
      },
      {
        onSuccess: () => {
          toast.success(
            `Creating cluster "${trimmedName}" (${size.cpu} vCPU / ${size.memory}) — provisioning in the background.`,
          );
          onOpenChange(false);
          navigate("/tasks");
        },
        onError: (err) =>
          toast.error(err instanceof Error ? err.message : "Couldn't start cluster creation"),
      },
    );
  }

  function next() {
    if (!stepValid) return;
    if (step < STEPS.length - 1) setStep((s) => s + 1);
    else create();
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-xl">
        <DialogHeader>
          <DialogTitle>Create cluster</DialogTitle>
          <DialogDescription>
            A cluster is one Postgres database on branchable storage, with a <code>main</code>{" "}
            branch and a write endpoint.
          </DialogDescription>
        </DialogHeader>

        <Stepper steps={STEPS} current={step} className="my-4" />

        <div className="min-h-[240px]">
          {step === 0 && (
            <div className="space-y-4">
              <div className="space-y-2">
                <Label>Engine</Label>
                <EngineField value={engine} onChange={setEngine} />
              </div>
              <div className="space-y-2">
                <Label htmlFor="wiz-pool">Shared pool</Label>
                <Select value={poolId} onValueChange={setPoolId}>
                  <SelectTrigger id="wiz-pool">
                    <SelectValue placeholder="Select a pool" />
                  </SelectTrigger>
                  <SelectContent>
                    {pools.map((pool) => (
                      <SelectItem key={pool.id} value={pool.id}>
                        {pool.name}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-2">
                <Label htmlFor="wiz-name">Cluster name</Label>
                <Input
                  id="wiz-name"
                  autoFocus
                  placeholder="e.g. orders"
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === "Enter" && stepValid) next();
                  }}
                />
              </div>
            </div>
          )}

          {step === 1 && (
            <div className="space-y-3">
              <p className="text-sm text-muted-foreground">
                Enabled on the cluster's <code>main</code> branch — every compute on it mounts them.
                You can add more later from a branch's Extensions dialog.
              </p>
              {fromEngine.length > 0 && (
                <p className="text-sm text-muted-foreground">
                  {engineLabel} includes <span className="font-mono">{fromEngine.join(", ")}</span>.
                </p>
              )}
              {extCatalog.isError && (
                <p className="text-sm text-destructive">
                  Could not load the extension catalog; showing the built-in set only. Anything an
                  admin registered is missing from this list.
                </p>
              )}
              <ExtensionsField
                value={extensions}
                onChange={setExtensions}
                enabled={fromEngine}
                options={extCatalog.options}
                showRestartTag={false}
              />
            </div>
          )}

          {step === 2 && (
            <div className="space-y-3">
              <p className="text-sm text-muted-foreground">
                Sizes the cluster's read-write endpoint on the <code>main</code> branch.
              </p>
              <ComputeSizeField value={size} onChange={setSize} />
              <div className="pt-1">
                <ConnectivityField value={connectivity} onChange={setConnectivity} />
                <ConnectivityWarning value={connectivity} />
              </div>
            </div>
          )}

          {step === 3 && (
            <div className="space-y-2 rounded-lg border border-border">
              <ReviewRow k="Name" v={trimmedName || "—"} mono />
              <ReviewRow k="Engine" v={engineLabel} />
              <ReviewRow k="Shared pool" v={poolName} mono />
              <ReviewRow
                k="Extensions"
                v={allExtensions.length ? allExtensions.join(", ") : "None"}
                mono={allExtensions.length > 0}
              />
              <ReviewRow k="Writer compute" v={`${size.cpu} vCPU · ${size.memory}`} mono />
              <ReviewRow
                k="Connectivity"
                v={
                  CONNECTIVITY_LABELS[connectivity.mode] +
                  (parseCidrs(connectivity.allowedCidrs).length
                    ? ` · ${parseCidrs(connectivity.allowedCidrs).join(", ")}`
                    : "")
                }
                mono={connectivity.mode !== "cluster_ip"}
              />
            </div>
          )}
        </div>

        <DialogFooter className="gap-2 sm:justify-between">
          <Button
            type="button"
            variant="ghost"
            onClick={() => (step === 0 ? onOpenChange(false) : setStep((s) => s - 1))}
            disabled={pending}
          >
            {step === 0 ? "Cancel" : "Back"}
          </Button>
          <Button type="button" variant="brand" onClick={next} disabled={!stepValid || pending}>
            {step < STEPS.length - 1 ? "Next" : pending ? "Creating…" : "Create cluster"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function ReviewRow({ k, v, mono }: { k: string; v: string; mono?: boolean }) {
  return (
    <div className="flex items-center justify-between gap-4 border-b border-border px-4 py-2.5 text-sm last:border-b-0">
      <span className="text-muted-foreground">{k}</span>
      <span className={mono ? "font-mono" : "font-medium"}>{v}</span>
    </div>
  );
}
