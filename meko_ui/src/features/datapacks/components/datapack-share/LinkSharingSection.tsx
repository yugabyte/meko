import { Link2 } from "lucide-react";
import { DatapackShareSectionCard } from "@/features/datapacks/components/datapack-share/DatapackShareSectionCard";

export function LinkSharingSection() {
  return (
    <DatapackShareSectionCard
      title="Link sharing"
      icon={<Link2 className="text-muted-foreground" aria-hidden />}
      trailing={
        <span className="rounded-full border border-border bg-muted/40 px-2 py-0.5 text-[10px] font-semibold uppercase tracking-wide text-muted-foreground">
          org only
        </span>
      }
    >
      <div
        className="relative min-h-[112px] overflow-hidden rounded-lg border border-dashed border-border bg-muted/5"
        role="status"
        aria-label="Link sharing — coming soon"
      >
        {/* Placeholder UI (blurred), similar spirit to non-Overview tabs on DatapackDetailPage */}
        <div className="pointer-events-none select-none px-4 py-5 blur-[3px] opacity-60">
          <div className="flex flex-col gap-4 sm:flex-row sm:items-center sm:justify-between">
            <div className="min-w-0 space-y-1">
              <p className="text-sm font-medium text-foreground">
                Anyone at your organization with the link can view this datapack
              </p>
              <p className="text-xs leading-relaxed text-muted-foreground">
                Toggle to generate a shareable link for teammates in your org.
              </p>
            </div>
            <div
              className="h-7 w-12 shrink-0 self-start rounded-full border border-border bg-muted sm:self-center"
              aria-hidden
            />
          </div>
        </div>

        <div className="absolute inset-0 flex items-center justify-center bg-background/50 backdrop-blur-[2px]">
          <p className="rounded-lg border border-border bg-card px-4 py-2 text-sm font-semibold tracking-tight text-foreground shadow-sm">
            Coming Soon
          </p>
        </div>
      </div>
    </DatapackShareSectionCard>
  );
}
