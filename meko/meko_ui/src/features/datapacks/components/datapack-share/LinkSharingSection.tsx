// Placeholder card for org-scoped link sharing on the datapack share page (feature not enabled yet).
// Uses ShareComingSoonPlaceholder with a blurred mock toggle so the section matches the rest of the layout.
import { Link2 } from "lucide-react";
import { DatapackShareSectionCard } from "@/features/datapacks/components/datapack-share/DatapackShareSectionCard";
import { ShareComingSoonPlaceholder } from "@/features/datapacks/components/datapack-share/ShareComingSoonPlaceholder";

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
      <ShareComingSoonPlaceholder
        aria-label="Link sharing — coming soon"
        blurredPreview={
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
        }
      />
    </DatapackShareSectionCard>
  );
}
