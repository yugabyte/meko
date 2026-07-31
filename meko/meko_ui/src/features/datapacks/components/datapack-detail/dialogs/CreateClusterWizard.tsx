// Multi-step create-cluster wizard (Cluster → Compute → Review). Fires ONE
// composite call (create_project with_writer) that provisions the database, its
// `main` branch, and a read_write endpoint as a single background task;
// api_server polls it to completion and returns the created cluster (well under
// a minute), then we toast + close. Replaces the old single-field CreateClusterDialog.
import { useEffect, useState } from "react";
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
import { Stepper } from "@/components/ui/stepper";
import { cn } from "@/lib/utils";
import { getApiErrorMessage } from "@/lib/api/mekoApi";
import { toast } from "@/hooks/use-toast";
import { useCreateCluster } from "@/features/datapacks/hooks/useAmpClusters";
import { DEFAULT_CPU, DEFAULT_MEMORY } from "../datapack-clusters/resize";

const STEPS = ["Cluster", "Compute", "Review"];

// PostgreSQL 18 is the only engine AMP supports today; the rest are roadmap,
// shown greyed as "coming soon" so the direction is visible without implying support.
const DB_ENGINES: { id: string; label: string; comingSoon?: boolean }[] = [
  { id: "postgres", label: "PostgreSQL 18" },
  { id: "aidb", label: "AI Database", comingSoon: true },
  { id: "oracle", label: "Oracle", comingSoon: true },
  { id: "sqlserver", label: "SQL Server", comingSoon: true },
  { id: "documentdb", label: "DocumentDB", comingSoon: true },
];

export type CreateClusterWizardProps = {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  datapackId: string;
};

export function CreateClusterWizard({ open, onOpenChange, datapackId }: CreateClusterWizardProps) {
  const [step, setStep] = useState(0);
  const [engine, setEngine] = useState("postgres");
  const [name, setName] = useState("");
  const [cpu, setCpu] = useState(DEFAULT_CPU);
  const [memory, setMemory] = useState(DEFAULT_MEMORY);
  const [error, setError] = useState<string | null>(null);

  const createMut = useCreateCluster(datapackId);
  const pending = createMut.isPending;

  useEffect(() => {
    if (open) {
      setStep(0);
      setEngine("postgres");
      setName("");
      setCpu(DEFAULT_CPU);
      setMemory(DEFAULT_MEMORY);
      setError(null);
    }
  }, [open]);

  const trimmedName = name.trim();
  const stepValid =
    step === 0 ? Boolean(trimmedName) : step === 1 ? Boolean(cpu.trim() && memory.trim()) : true;

  const create = async () => {
    if (!trimmedName || pending) return;
    setError(null);
    try {
      await createMut.mutateAsync({ name: trimmedName, cpu: cpu.trim(), memory: memory.trim() });
      toast({ title: `Cluster "${trimmedName}" created` });
      onOpenChange(false);
    } catch (e) {
      setError(getApiErrorMessage(e, "Could not create the cluster."));
    }
  };

  const next = () => {
    if (!stepValid) return;
    if (step < STEPS.length - 1) setStep((s) => s + 1);
    else void create();
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="w-[calc(100vw-2rem)] sm:max-w-lg max-h-[85vh] overflow-y-auto bg-card border-border text-foreground">
        <DialogHeader>
          <DialogTitle className="text-xl text-foreground">Create cluster</DialogTitle>
          <DialogDescription className="text-muted-foreground">
            A cluster is one Postgres database on branchable storage, with a <code>main</code> branch
            and a read-write endpoint to connect to.
          </DialogDescription>
        </DialogHeader>

        <Stepper steps={STEPS} current={step} className="my-4" />

        <div className="min-h-[220px]">
          {step === 0 ? (
            <div className="space-y-4">
              <div className="space-y-2">
                <Label>Engine</Label>
                <div className="grid grid-cols-2 gap-2 sm:grid-cols-3">
                  {DB_ENGINES.map((e) => {
                    const selected = engine === e.id;
                    return (
                      <button
                        key={e.id}
                        type="button"
                        disabled={e.comingSoon}
                        onClick={() => setEngine(e.id)}
                        className={cn(
                          "rounded-lg border px-3 py-2.5 text-left transition-colors",
                          selected
                            ? "border-brand bg-brand/10"
                            : "border-border bg-background hover:bg-muted/50",
                          e.comingSoon && "cursor-not-allowed opacity-60 hover:bg-background",
                        )}
                      >
                        <span className="block text-sm font-medium text-foreground">{e.label}</span>
                        {e.comingSoon ? (
                          <span className="mt-0.5 block text-[10px] uppercase tracking-wide text-muted-foreground">
                            Coming soon
                          </span>
                        ) : null}
                      </button>
                    );
                  })}
                </div>
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
          ) : null}

          {step === 1 ? (
            <div className="space-y-3">
              <div className="grid grid-cols-2 gap-3">
                <div className="space-y-2">
                  <Label htmlFor="wiz-cpu">vCPU</Label>
                  <Input
                    id="wiz-cpu"
                    value={cpu}
                    onChange={(e) => setCpu(e.target.value)}
                    placeholder={DEFAULT_CPU}
                  />
                </div>
                <div className="space-y-2">
                  <Label htmlFor="wiz-mem">Memory</Label>
                  <Input
                    id="wiz-mem"
                    value={memory}
                    onChange={(e) => setMemory(e.target.value)}
                    placeholder={DEFAULT_MEMORY}
                  />
                </div>
              </div>
              <p className="text-xs text-muted-foreground">
                Sizes the cluster's read-write endpoint on the <code>main</code> branch. Kubernetes
                quantities (e.g. 2 / 4Gi).
              </p>
            </div>
          ) : null}

          {step === 2 ? (
            <div className="rounded-lg border border-border">
              <ReviewRow k="Name" v={trimmedName || "—"} mono />
              <ReviewRow k="Engine" v="PostgreSQL 18" />
              <ReviewRow k="Writer compute" v={`${cpu} vCPU · ${memory}`} mono />
            </div>
          ) : null}
        </div>

        {error ? (
          <div
            role="alert"
            className="mt-3 max-h-32 overflow-y-auto whitespace-pre-wrap break-words rounded-md border border-red-500/30 bg-red-500/10 px-3 py-2 text-sm text-red-700 dark:text-red-300"
          >
            {error}
          </div>
        ) : null}

        <DialogFooter className="gap-2 sm:justify-between">
          <Button
            type="button"
            variant="outline"
            onClick={() => (step === 0 ? onOpenChange(false) : setStep((s) => s - 1))}
            disabled={pending}
          >
            {step === 0 ? "Cancel" : "Back"}
          </Button>
          <Button type="button" onClick={next} disabled={!stepValid || pending}>
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
      <span className={mono ? "font-mono text-foreground" : "font-medium text-foreground"}>{v}</span>
    </div>
  );
}
