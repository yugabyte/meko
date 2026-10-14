// Icon button beside the details page's Share button: copies the datapack's
// active share link, or opens the share page when there is none.
import { Link2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { useShareLinkAction } from "./useShareLinkAction";

export function CopyShareLinkButton({ datapackId }: { datapackId: string }) {
  const action = useShareLinkAction(datapackId, true);
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <Button
          type="button"
          variant="outline"
          size="icon"
          className="shrink-0 border-border bg-background/80"
          aria-label={action.label}
          onClick={action.run}
        >
          <Link2 className="h-4 w-4 shrink-0" aria-hidden />
        </Button>
      </TooltipTrigger>
      <TooltipContent>{action.label}</TooltipContent>
    </Tooltip>
  );
}
