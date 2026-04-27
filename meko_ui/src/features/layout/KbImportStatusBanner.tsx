/**
 * Global strip when a knowledge-base import job is running (configure wizard or elsewhere).
 */
import { Loader2 } from "lucide-react";
import { cn } from "@/lib/utils";
import { useKbImportJobDetail } from "@/hooks/useKbImportJob";

export function KbImportStatusBanner() {
  const { phase, datapackName } = useKbImportJobDetail();
  const active = phase === "creating" || phase === "polling";
  if (!active || !datapackName) return null;

  return (
    <div
      className={cn(
        "flex shrink-0 items-center justify-center gap-2 border-b border-blue-500/30 bg-blue-950/40 px-4 py-2 text-sm text-blue-100",
      )}
      role="status"
      aria-live="polite"
    >
      <Loader2 className="h-4 w-4 shrink-0 animate-spin text-blue-300" aria-hidden />
      <span>
        Importing knowledge into <span className="font-semibold text-white">{datapackName}</span>…
      </span>
    </div>
  );
}
