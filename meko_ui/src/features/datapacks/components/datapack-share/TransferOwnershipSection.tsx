import { Shield } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { DatapackShareSectionCard } from "@/features/datapacks/components/datapack-share/DatapackShareSectionCard";

export function TransferOwnershipSection() {
  return (
    <DatapackShareSectionCard
      title="Transfer ownership"
      icon={<Shield className="text-destructive/90" aria-hidden />}
      tone="danger"
      trailing={
        <Tooltip>
          <TooltipTrigger asChild>
            <span className="inline-flex shrink-0" tabIndex={0}>
              <Button
                type="button"
                variant="outline"
                size="sm"
                disabled
                className="cursor-not-allowed border-destructive/50 text-destructive opacity-70 hover:bg-transparent"
              >
                Transfer
              </Button>
            </span>
          </TooltipTrigger>
          <TooltipContent side="left">Coming Soon</TooltipContent>
        </Tooltip>
      }
    >
      <p className="text-sm text-muted-foreground max-w-2xl">
        Transfer this datapack to another admin. You will lose owner controls unless they grant them
        back.
      </p>
    </DatapackShareSectionCard>
  );
}
