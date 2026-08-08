// Create-cluster wizard: a multi-step flow (Cluster -> Compute -> Review) that
// creates a cluster and its writer endpoint. It composes the reusable
// components/ui primitives (Stepper, ComputeSizeField) and fires ONE composite
// call — createCluster (create_cluster with_writer) — which the controller runs
// as a single background task: cluster + `main` branch + read_write endpoint.
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
import { EngineField } from "@/components/ui/engine-field";
import { useCreateClusterWithWriter } from "@/lib/api/queries";
import {
  DEFAULT_ENDPOINT_CPU,
  DEFAULT_ENDPOINT_MEMORY,
  type StoragePool,
} from "@/lib/api/types";

const STEPS = ["Cluster", "Compute", "Review"];

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
  const [size, setSize] = useState<ComputeSize>({
    cpu: DEFAULT_ENDPOINT_CPU,
    memory: DEFAULT_ENDPOINT_MEMORY,
  });

  const navigate = useNavigate();
  const createCluster = useCreateClusterWithWriter();
  const pending = createCluster.isPending;

  useEffect(() => {
    if (open) {
      setStep(0);
      setEngine("postgres");
      setName("");
      setPoolId("");
      setSize({ cpu: DEFAULT_ENDPOINT_CPU, memory: DEFAULT_ENDPOINT_MEMORY });
    }
  }, [open]);

  const poolName = pools.find((p) => p.id === poolId)?.name ?? "";
  const trimmedName = name.trim();
  const stepValid =
    step === 0 ? Boolean(trimmedName && poolId) : step === 1 ? Boolean(size.cpu.trim() && size.memory.trim()) : true;

  function create() {
    // One composite call: the controller creates the cluster, its `main` branch,
    // and the read_write endpoint as a single background task. We don't wait for
    // it — toast + deeplink to Tasks and let the worker finish independently.
    createCluster.mutate(
      { poolId, name: trimmedName, cpu: size.cpu.trim(), memory: size.memory.trim() },
      {
        onSuccess: () => {
          toast.success(
            `Creating cluster "${trimmedName}" (${size.cpu} vCPU / ${size.memory}) — provisioning in the background.`,
          );
          onOpenChange(false);
          navigate("/tasks");
        },
        onError: (err) =>
          toast.error(
            err instanceof Error ? err.message : "Couldn't start cluster creation",
          ),
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
            A cluster is one Postgres database on branchable storage, with a{" "}
            <code>main</code> branch and a write endpoint.
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
                Sizes the cluster's read-write endpoint on the <code>main</code> branch.
              </p>
              <ComputeSizeField value={size} onChange={setSize} />
            </div>
          )}

          {step === 2 && (
            <div className="space-y-2 rounded-lg border border-border">
              <ReviewRow k="Name" v={trimmedName || "—"} mono />
              <ReviewRow k="Engine" v="PostgreSQL 18" />
              <ReviewRow k="Shared pool" v={poolName} mono />
              <ReviewRow k="Writer compute" v={`${size.cpu} vCPU · ${size.memory}`} mono />
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
