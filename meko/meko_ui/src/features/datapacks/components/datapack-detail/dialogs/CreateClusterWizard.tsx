// Multi-step create-cluster wizard (Cluster → Extensions → Compute → Review).
// Fires ONE composite call (create_cluster with_writer) that provisions the
// database, its `main` branch (seeded with the picked extensions), and a
// read_write endpoint as a single background task; api_server polls it to
// completion and returns the created cluster (this can take up to a minute,
// which the wizard shows a provisioning state for), then we toast + close.
// Mirrors amp_ui's wizard, minus the pool step (a datapack has
// exactly one AMP pool, resolved server-side).
import { useEffect, useMemo, useState } from "react";
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
import { Spinner } from "@/components/ui/spinner";
import { Stepper } from "@/components/ui/stepper";
import { cn } from "@/lib/utils";
import { getApiErrorMessage } from "@/lib/api/mekoApi";
import { ampCoreLimitMessage } from "@/lib/api/ampApi";
import { toast } from "@/hooks/use-toast";
import { useAmpCoreBudget, useCreateCluster } from "@/features/datapacks/hooks/useAmpClusters";
import { useBudgetedCompute } from "@/features/datapacks/hooks/useBudgetedCompute";
import { useAmpCredentials } from "@/features/datapacks/hooks/useAmpCredentials";
import { AmpCredentialsStep } from "../datapack-clusters/AmpCredentialsStep";
import { AMP_COMPUTE_BANNER_KIND } from "@/lib/freeTier";
import { TierLimitBanner } from "@/lib/freeTierBanner";
import { formatVcpuMetricLine } from "@/lib/ampCoreBudget";
import {
  DEFAULT_CPU,
  DEFAULT_MEMORY,
  MEMORY_GIB_PER_VCPU,
  overBudgetMessage,
} from "../datapack-clusters/resize";
import {
  AMP_EXTENSIONS,
  DB_ENGINES,
  engineExtensions,
  engineLabel,
} from "../datapack-clusters/extensions";

const STEPS = ["Cluster", "Extensions", "Compute", "Credentials", "Review"];

export type CreateClusterWizardProps = {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  datapackId: string;
};

export function CreateClusterWizard({ open, onOpenChange, datapackId }: CreateClusterWizardProps) {
  const [step, setStep] = useState(0);
  const [engine, setEngine] = useState("postgres");
  const [name, setName] = useState("");
  // User-picked EXTRA extensions only — the engine's own bundle is added by the
  // controller, so we track and send just the extras (keeps the picker's
  // locked-vs-toggle state unambiguous; the controller also de-dupes).
  const [extensions, setExtensions] = useState<string[]>([]);
  const [error, setError] = useState<{ message: string; upgrade: boolean } | null>(null);

  const createMut = useCreateCluster(datapackId);
  const pending = createMut.isPending;

  // CPU/memory sizing + tier-budget guard, shared with the add-compute form so
  // the prefill/validation edges live in one place (useBudgetedCompute).
  const budget = useBudgetedCompute(datapackId, { enabled: open });
  const { cpu, memory } = budget;
  // Core budget drives the tier-limit banner's "X / Y vCPU used" line on a 402
  // (deduped with useBudgetedCompute's own query by its shared key).
  const { data: coreBudget } = useAmpCoreBudget(datapackId, { enabled: open });

  // cloud_admin credential state for the writer endpoint (generate or type;
  // required by the controller). Re-seeded on open so each cluster gets a fresh
  // generated candidate.
  const creds = useAmpCredentials();

  useEffect(() => {
    if (open) {
      setStep(0);
      setEngine("postgres");
      setName("");
      setExtensions([]);
      budget.reset();
      creds.reset();
      setError(null);
    }
    // Reset only when the dialog opens; budget.reset re-seeds the compute fields.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  const fromEngine = useMemo(() => engineExtensions(engine), [engine]);
  const locked = useMemo(() => new Set(fromEngine), [fromEngine]);
  // Every extension enabled on the cluster: the engine's bundle plus the extras.
  const allExtensions = useMemo(
    () => [...fromEngine, ...extensions.filter((e) => !locked.has(e))],
    [fromEngine, extensions, locked],
  );

  const trimmedName = name.trim();
  const stepValid =
    step === 0 ? Boolean(trimmedName) : step === 2 ? budget.valid : step === 3 ? creds.valid : true;

  const pickEngine = (id: string) => {
    setEngine(id);
    // Drop any extras the new engine now seeds itself, so they render locked
    // (not double-toggled) and aren't re-sent.
    const seeded = new Set(engineExtensions(id));
    setExtensions((prev) => prev.filter((e) => !seeded.has(e)));
  };

  const toggleExtension = (n: string) => {
    if (locked.has(n)) return;
    setExtensions((prev) => (prev.includes(n) ? prev.filter((e) => e !== n) : [...prev, n]));
  };

  const create = async () => {
    if (!trimmedName || pending) return;
    setError(null);
    try {
      await createMut.mutateAsync({
        name: trimmedName,
        cpu: cpu.trim(),
        memory: memory.trim(),
        password: creds.password,
        engine,
        extensions: extensions.filter((e) => !locked.has(e)),
      });
      toast({ title: `Cluster "${trimmedName}" created` });
      onOpenChange(false);
    } catch (e) {
      const upgradeMsg = ampCoreLimitMessage(e);
      setError(
        upgradeMsg
          ? { message: upgradeMsg, upgrade: true }
          : { message: getApiErrorMessage(e, "Could not create the cluster."), upgrade: false },
      );
    }
  };

  const next = () => {
    if (!stepValid) return;
    if (step < STEPS.length - 1) setStep((s) => s + 1);
    else void create();
  };

  return (
    <Dialog
      open={open}
      // While the composite create task runs, swallow every close request
      // (Esc, overlay click, X) so the user can't dismiss the dialog mid-provision.
      onOpenChange={(nextOpen) => {
        if (pending && !nextOpen) return;
        onOpenChange(nextOpen);
      }}
    >
      <DialogContent
        className={cn(
          // p-4 base keeps the shared dialog's phone fix (no 24px gutters on a
          // narrow viewport); sm:p-8 gives desktop room.
          "w-[calc(100vw-2rem)] sm:max-w-2xl max-h-[90vh] overflow-y-auto bg-card border-border text-foreground p-4 sm:p-8",
          // Hide the built-in close (X) while provisioning — the task must finish.
          pending && "[&>button]:hidden",
        )}
        onEscapeKeyDown={(e) => pending && e.preventDefault()}
        onInteractOutside={(e) => pending && e.preventDefault()}
      >
        <DialogHeader>
          <DialogTitle className="text-xl text-foreground">Create cluster</DialogTitle>
          <DialogDescription className="text-muted-foreground">
            A cluster is one Postgres database on branchable storage, with a <code>main</code>{" "}
            branch and a read-write endpoint to connect to.
          </DialogDescription>
        </DialogHeader>

        {/* DialogContent is a `grid gap-4`, so the stepper and body already get
            even 16px rhythm from their siblings — no extra vertical margin. */}
        {!pending ? <Stepper steps={STEPS} current={step} /> : null}

        <div className={cn("min-h-[220px]", pending && "flex items-center justify-center")}>
          {pending ? (
            <div className="flex flex-col items-center gap-3 px-4 text-center" role="status">
              <Spinner className="h-7 w-7 text-brand" />
              <p className="text-sm font-medium text-foreground">Provisioning your cluster…</p>
              <p className="max-w-sm text-xs leading-relaxed text-muted-foreground">
                Creating the database, its <code>main</code> branch, and a read-write endpoint. This
                can take up to a minute — keep this dialog open.
              </p>
            </div>
          ) : (
            <>
              {step === 0 ? (
                <div className="space-y-4">
                  <div className="space-y-2">
                    <Label>Engine</Label>
                    <div className="grid grid-cols-2 gap-3 sm:grid-cols-3">
                      {DB_ENGINES.map((e) => {
                        const selected = engine === e.id;
                        const seeds = engineExtensions(e.id);
                        return (
                          <button
                            key={e.id}
                            type="button"
                            aria-pressed={selected}
                            disabled={e.comingSoon}
                            onClick={() => pickEngine(e.id)}
                            className={cn(
                              "rounded-lg border px-4 py-3 text-left transition-colors",
                              selected
                                ? "border-brand bg-brand/10 ring-1 ring-brand"
                                : "border-border bg-background hover:bg-muted/50",
                              e.comingSoon && "cursor-not-allowed opacity-60 hover:bg-background",
                            )}
                          >
                            <span className="block text-sm font-medium text-foreground">
                              {e.label}
                              {e.sub ? (
                                <span className="ml-1 text-xs text-muted-foreground">{e.sub}</span>
                              ) : null}
                            </span>
                            {e.comingSoon ? (
                              <span className="mt-0.5 block text-[10px] uppercase tracking-wide text-muted-foreground">
                                Coming soon
                              </span>
                            ) : seeds.length ? (
                              <span className="mt-0.5 block truncate font-mono text-[10px] text-muted-foreground">
                                {seeds.join(" · ")}
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
                  <p className="text-xs text-muted-foreground">
                    Enabled on the cluster's <code>main</code> branch — every compute on it mounts
                    them. Pick them now: they can't be changed after the cluster is created.
                    {fromEngine.length
                      ? ` ${engineLabel(engine)} includes ${fromEngine.join(", ")}.`
                      : ""}
                  </p>
                  <div className="space-y-2">
                    {AMP_EXTENSIONS.map((x) => {
                      const isLocked = locked.has(x.name);
                      const on = isLocked || extensions.includes(x.name);
                      return (
                        <button
                          key={x.name}
                          type="button"
                          role="checkbox"
                          aria-checked={on}
                          disabled={isLocked}
                          onClick={() => toggleExtension(x.name)}
                          className={cn(
                            "flex w-full items-start gap-3 rounded-lg border px-4 py-3 text-left transition-colors",
                            on
                              ? "border-brand bg-brand/10"
                              : "border-border bg-background hover:bg-muted/50",
                            isLocked && "cursor-not-allowed opacity-80 hover:bg-brand/10",
                          )}
                        >
                          <span
                            className={cn(
                              "mt-0.5 flex h-4 w-4 shrink-0 items-center justify-center rounded border",
                              // primary is the orange fill with navy text in BOTH themes (~6.9:1);
                              // brand-foreground is white in light mode (~2.74:1, fails WCAG AA —
                              // index.css:124-125), so it can't be used on an orange fill.
                              on
                                ? "border-primary bg-primary text-primary-foreground"
                                : "border-border",
                            )}
                            aria-hidden
                          >
                            {on ? "✓" : ""}
                          </span>
                          <span className="min-w-0">
                            <span className="flex items-baseline gap-2">
                              <span className="font-mono text-sm text-foreground">{x.name}</span>
                              <span className="text-[10px] text-muted-foreground">{x.version}</span>
                              {isLocked ? (
                                <span className="text-[10px] uppercase tracking-wide text-brand">
                                  Enabled
                                </span>
                              ) : null}
                            </span>
                            <span className="block text-xs text-muted-foreground">
                              {x.description}
                            </span>
                          </span>
                        </button>
                      );
                    })}
                  </div>
                </div>
              ) : null}

              {step === 2 ? (
                <div className="space-y-3">
                  <div className="grid grid-cols-2 gap-3">
                    <div className="space-y-2">
                      <Label htmlFor="wiz-cpu">vCPU</Label>
                      <Input
                        id="wiz-cpu"
                        value={cpu}
                        onChange={(e) => budget.setCpu(e.target.value)}
                        placeholder={DEFAULT_CPU}
                      />
                    </div>
                    <div className="space-y-2">
                      <Label htmlFor="wiz-mem">Memory</Label>
                      <Input
                        id="wiz-mem"
                        value={memory}
                        onChange={(e) => budget.setMemory(e.target.value)}
                        placeholder={DEFAULT_MEMORY}
                      />
                    </div>
                  </div>
                  <p className="text-xs text-muted-foreground">
                    Sizes the cluster's read-write endpoint on the <code>main</code> branch.
                    Kubernetes quantities (e.g. 2 / 4Gi); memory tracks AMP's {MEMORY_GIB_PER_VCPU}{" "}
                    GiB-per-vCPU minimum.
                  </p>
                  {budget.overBudget ? (
                    <p className="text-xs text-red-700 dark:text-red-300" role="alert">
                      {overBudgetMessage(budget.remaining)}
                    </p>
                  ) : null}
                </div>
              ) : null}

              {step === 3 ? <AmpCredentialsStep creds={creds} clusterName={trimmedName} /> : null}

              {step === 4 ? (
                <div className="rounded-lg border border-border">
                  <ReviewRow k="Name" v={trimmedName || "—"} mono />
                  <ReviewRow k="Engine" v={engineLabel(engine)} />
                  <ReviewRow
                    k="Extensions"
                    v={allExtensions.length ? allExtensions.join(", ") : "None"}
                  />
                  <ReviewRow k="Writer compute" v={`${cpu} vCPU · ${memory}`} mono />
                  <ReviewRow
                    k="Credentials"
                    v={`cloud_admin · ${creds.mode === "generate" ? "generated" : "custom"} password`}
                  />
                </div>
              ) : null}
            </>
          )}
        </div>

        {error?.upgrade ? (
          <TierLimitBanner
            className="mt-3"
            kind={AMP_COMPUTE_BANNER_KIND}
            metricLine={formatVcpuMetricLine(coreBudget)}
          />
        ) : error ? (
          <div
            role="alert"
            className="mt-3 max-h-32 overflow-y-auto whitespace-pre-wrap break-words rounded-md border border-red-500/30 bg-red-500/10 px-3 py-2 text-sm text-red-700 dark:text-red-300"
          >
            {error.message}
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
          <Button
            type="button"
            onClick={next}
            // On the final step, hold the create until the budget query resolves
            // so an at-cap user can't slip through the loading window (remaining
            // reads null while loading → the guard is briefly off).
            disabled={!stepValid || pending || (step === STEPS.length - 1 && budget.isLoading)}
          >
            {step < STEPS.length - 1 ? (
              "Next"
            ) : pending ? (
              <>
                <Spinner className="mr-2 h-4 w-4" />
                Creating…
              </>
            ) : (
              "Create cluster"
            )}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function ReviewRow({ k, v, mono }: { k: string; v: string; mono?: boolean }) {
  return (
    <div className="flex items-center justify-between gap-4 border-b border-border px-4 py-3 text-sm last:border-b-0">
      <span className="text-muted-foreground">{k}</span>
      <span className={mono ? "font-mono text-foreground" : "font-medium text-foreground"}>
        {v}
      </span>
    </div>
  );
}
