// Placeholder card for transferring datapack ownership (feature not enabled yet).
// Uses ShareComingSoonPlaceholder with a blurred mock row so it matches LinkSharingSection on the share page.
import { Shield } from "lucide-react";
import { DatapackShareSectionCard } from "@/features/datapacks/components/datapack-share/DatapackShareSectionCard";
import { ShareComingSoonPlaceholder } from "@/features/datapacks/components/datapack-share/ShareComingSoonPlaceholder";

export function TransferOwnershipSection() {
  return (
    <DatapackShareSectionCard
      title="Transfer ownership"
      icon={<Shield className="text-destructive/90" aria-hidden />}
      tone="danger"
      trailing={
        <span className="rounded-full border border-border bg-muted/40 px-2 py-0.5 text-[10px] font-semibold uppercase tracking-wide text-muted-foreground">
          coming soon
        </span>
      }
    >
      <ShareComingSoonPlaceholder
        aria-label="Transfer ownership — coming soon"
        blurredPreview={
          <div className="flex flex-col gap-4 sm:flex-row sm:items-end sm:justify-between">
            <div className="min-w-0 space-y-1">
              <p className="text-sm font-medium text-foreground">
                Transfer this datapack to another maintainer
              </p>
              <p className="text-xs leading-relaxed text-muted-foreground">
                You will lose owner controls unless they grant them back after the transfer.
              </p>
            </div>
            <div
              className="h-9 shrink-0 self-start rounded-md border border-border bg-muted px-8 sm:self-center"
              aria-hidden
            />
          </div>
        }
      />
    </DatapackShareSectionCard>
  );
}
