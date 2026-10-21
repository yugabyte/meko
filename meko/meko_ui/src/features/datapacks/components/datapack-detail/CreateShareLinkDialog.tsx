// First-time Create share link flow for a datapack: pick a role, confirm,
// and get an active + copied link in one API call. Opened from the Share
// dropdown ONLY when the datapack has no link row at all (see
// useShareLinkAction). Opening this dialog for an off-but-existing link
// would send a default grant on top of the saved one, overwriting the role
// and moving every member who joined through that link — so the dropdown
// routes that case to the Share page's Switch, which sends {status} alone.
// For role changes, resets, and redemption counts after creation, the Share
// page's ShareLinkSection remains the canonical surface.

import { useState } from "react";
import { Loader2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { toast } from "@/hooks/use-toast";
import { formatDatapackShareGrantCamelCase } from "@/lib/datapackShareGrants";
import { getApiErrorMessage } from "@/lib/api/mekoApi";
import {
  SHARE_LINK_GRANTS,
  SHARE_LINK_STATUS_ACTIVE,
  type ShareLinkGrant,
} from "@/lib/api/shareLinksApi";
import {
  copyShareLinkUrl,
  useUpdateDatapackShareLink,
} from "@/features/datapacks/hooks/useDatapackShareLink";

type Props = {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  datapackId: string;
};

export function CreateShareLinkDialog({ open, onOpenChange, datapackId }: Props) {
  const update = useUpdateDatapackShareLink(datapackId);
  const [grant, setGrant] = useState<ShareLinkGrant>("viewer");

  const confirm = async () => {
    try {
      // One API call enables the link AND sets its role: the server accepts both
      // fields in a single PUT body (ShareLinkUpdate), so we don't have to
      // round-trip twice the way the Share page's standalone Switch + Select do.
      const link = await update.mutateAsync({ status: SHARE_LINK_STATUS_ACTIVE, grant });
      if (link?.token) {
        // copyShareLinkUrl toasts on its own result (Link copied / Couldn't copy);
        // the modal closes either way — the link is active on the server regardless.
        await copyShareLinkUrl(link.token);
      }
      onOpenChange(false);
    } catch (err) {
      toast({
        title: "Couldn't create the link",
        description: getApiErrorMessage(err, ""),
        variant: "destructive",
      });
    }
  };

  return (
    <Dialog open={open} onOpenChange={(o) => !update.isPending && onOpenChange(o)}>
      <DialogContent className="bg-card border-border sm:max-w-md">
        <DialogHeader>
          <DialogTitle className="text-foreground">Create share link</DialogTitle>
          <DialogDescription className="text-muted-foreground">
            Anyone with the link can join this datapack. They sign in or sign up, then join without
            an invite.
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-4 py-2">
          <div className="space-y-2">
            <Label htmlFor="create-share-link-role" className="text-sm font-medium text-foreground">
              Role joiners get
            </Label>
            <Select
              value={grant}
              disabled={update.isPending}
              onValueChange={(g) => setGrant(g as ShareLinkGrant)}
            >
              <SelectTrigger id="create-share-link-role" className="sm:w-56">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {SHARE_LINK_GRANTS.map((g) => (
                  <SelectItem key={g} value={g}>
                    {formatDatapackShareGrantCamelCase(g)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <p className="text-xs text-muted-foreground">
              You can change the role, reset the link, or turn it off later from the Share page.
            </p>
          </div>
        </div>

        <DialogFooter className="gap-2 sm:gap-2">
          <Button
            type="button"
            variant="outline"
            onClick={() => onOpenChange(false)}
            disabled={update.isPending}
          >
            Cancel
          </Button>
          <Button type="button" onClick={() => void confirm()} disabled={update.isPending}>
            {update.isPending ? (
              <>
                <Loader2 className="mr-1.5 h-4 w-4 animate-spin shrink-0" aria-hidden />
                Creating…
              </>
            ) : (
              "Create and copy link"
            )}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
