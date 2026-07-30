import { CircleCheck, Folder, Link2Off, Loader2 } from "lucide-react";
import { WIZARD_STEP_ID, type WizardStepId } from "./wizardSteps";

export type WizardHeaderConnectionStatus = "loading" | "idle" | "connected";

type Props = {
  /** Shown after “SETUP ” — typically `configureDraftName` with fallbacks. */
  datapackDisplayName: string;
  stepId: WizardStepId;
  /** Agent / MCP detection — default idle until wired to real signals. */
  connectionStatus?: WizardHeaderConnectionStatus;
  /** Create flow: optional description passed through navigation state (configure step). */
  createFlowDescription?: string | null;
};

function subtitleForStep(stepId: WizardStepId): string {
  switch (stepId) {
    case WIZARD_STEP_ID.configure:
      return "Name your datapack and add an optional description.";
    case WIZARD_STEP_ID.install:
      return "Meko is trying to help with the agent detection.";
    case WIZARD_STEP_ID.sync:
      return "Proceed with importing knowledge files.";
    case WIZARD_STEP_ID.import:
      return "Review your imports and complete setup.";
    default:
      return "";
  }
}

function ConnectionBadge({ status }: { status: WizardHeaderConnectionStatus }) {
  if (status === "loading") {
    return (
      <span
        className="inline-flex items-center gap-1.5 rounded-full border border-amber-500/45 bg-amber-500/10 px-2.5 py-0.5 text-xs font-medium text-amber-700 dark:text-amber-200"
        aria-live="polite"
      >
        <Loader2 className="h-3.5 w-3.5 shrink-0 animate-spin text-amber-400" aria-hidden />
        Connecting
      </span>
    );
  }
  if (status === "connected") {
    return (
      <span className="inline-flex items-center gap-1.5 rounded-full border border-emerald-500/45 bg-emerald-500/10 px-2.5 py-0.5 text-xs font-medium text-emerald-700 dark:text-emerald-200">
        <CircleCheck className="h-3.5 w-3.5 shrink-0 text-emerald-400" aria-hidden />
        Connected
      </span>
    );
  }
  return (
    <span className="inline-flex items-center gap-1.5 rounded-full border border-border bg-muted/40 px-2.5 py-0.5 text-xs font-medium text-muted-foreground">
      <Link2Off className="h-3.5 w-3.5 shrink-0 opacity-80" aria-hidden />
      Not connected
    </span>
  );
}

export function ConfigureWizardPageHeader({
  datapackDisplayName,
  stepId,
  connectionStatus = "idle",
  createFlowDescription,
}: Props) {
  const subtitle = subtitleForStep(stepId);
  const showCreateDescription =
    stepId === WIZARD_STEP_ID.configure && Boolean(createFlowDescription?.trim());

  return (
    <div className="flex flex-col gap-4 sm:flex-row sm:items-start sm:gap-5">
      <div
        className="flex h-12 w-12 shrink-0 items-center justify-center rounded-lg border-2 border-brand/45 bg-input/90 shadow-sm"
        aria-hidden
      >
        <Folder className="h-6 w-6 text-brand" strokeWidth={1.5} />
      </div>
      <div className="min-w-0 flex-1 space-y-2">
        <div className="flex flex-wrap items-center gap-2 sm:gap-3">
          <h1 className="text-2xl font-semibold tracking-tight text-foreground sm:text-3xl">
            <span className="text-muted-foreground">Setup </span>
            <span className="text-foreground">{datapackDisplayName || "—"}</span>
          </h1>
          {/* <ConnectionBadge status={connectionStatus} /> */}
        </div>
        {subtitle ? (
          <p className="max-w-3xl text-sm leading-relaxed text-muted-foreground">{subtitle}</p>
        ) : null}
        {showCreateDescription ? (
          <p className="max-w-3xl text-sm leading-relaxed text-muted-foreground/95">
            {createFlowDescription?.trim()}
          </p>
        ) : null}
      </div>
    </div>
  );
}
