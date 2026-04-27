/**
 * Final wizard step: confirms basic setup and marks configure-complete in localStorage on mount.
 * Status narrative uses the same “Coming Soon” overlay pattern as link sharing until telemetry exists.
 */
import { useEffect } from "react";
import { CheckCircle2 } from "lucide-react";
import { markDatapackWizardConfigureDone } from "@/lib/datapackConfigureStatus";

type Props = {
  datapackName: string;
};

export function ConfigureWizardStatusStep({ datapackName }: Props) {
  useEffect(() => {
    markDatapackWizardConfigureDone(datapackName);
  }, [datapackName]);

  return (
    <div
      className="relative min-h-[140px] overflow-hidden rounded-xl border border-dashed border-border bg-muted/5"
      role="status"
      aria-label="Configuration status — coming soon"
    >
      <div className="pointer-events-none select-none p-6 blur-[3px] opacity-60 sm:p-8">
        <div className="flex flex-col gap-3 sm:flex-row sm:items-start sm:gap-4">
          <CheckCircle2 className="h-10 w-10 shrink-0 text-emerald-500/90" aria-hidden />
          <div className="min-w-0 space-y-2">
            <h2 className="text-lg font-semibold tracking-tight text-foreground">Datapack successfully configured</h2>
            <p className="text-sm leading-relaxed text-muted-foreground">
              Basic setup for <span className="font-medium text-foreground">{datapackName}</span> is complete. You can
              open the datapack to connect clients, upload knowledge, and invite collaborators.
            </p>
          </div>
        </div>
      </div>

      <div className="absolute inset-0 flex items-center justify-center bg-background/50 backdrop-blur-[2px]">
        <p className="rounded-lg border border-border bg-card px-4 py-2 text-sm font-semibold tracking-tight text-foreground shadow-sm">
          Coming Soon
        </p>
      </div>
    </div>
  );
}
