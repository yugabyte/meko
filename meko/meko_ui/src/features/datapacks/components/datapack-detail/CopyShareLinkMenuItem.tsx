// Actions-dropdown item on the details page: copies the datapack's active share
// link, or opens the share page when there is none.
import { Link2 } from "lucide-react";
import { DropdownMenuItem } from "@/components/ui/dropdown-menu";
import { useShareLinkAction } from "./useShareLinkAction";

export function CopyShareLinkMenuItem({ datapackId }: { datapackId: string }) {
  const action = useShareLinkAction(datapackId, true);
  return (
    <DropdownMenuItem className="gap-2 cursor-pointer focus:bg-muted/20" onSelect={action.run}>
      <Link2 className="h-4 w-4 shrink-0" aria-hidden />
      {action.label}
    </DropdownMenuItem>
  );
}
