// Invite-people modal opened from the datapack header's Share dropdown.
// Thin wrapper around InviteHumansSection so single + bulk (CSV / pasted
// list) invites, the self-filter, the "already member" shortcut, telemetry
// (datapack_shared with is_first_share / total_shares) and the single-vs-
// bulk endpoint routing come through the same code the Share page uses.
//
// Closes on a FULLY CLEAN batch (every address became a new invite). A mixed
// batch — some already members, some failed — keeps the dialog open so the
// user can read the per-row BulkInviteResultsPanel. The "Go to Members"
// toast action also gets rewritten here: the detail page has no
// #datapack-share-members anchor so the default scroll would no-op, so
// from the modal we navigate to the Share page instead.

import { useNavigate } from "react-router-dom";
import { Dialog, DialogContent, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { InviteHumansSection } from "@/features/datapacks/components/datapack-share/InviteHumansSection";
import { datapackSharePath } from "@/app/routes/paths";
import type { DatapackShareGrant } from "@/lib/datapackShareGrants";

type Props = {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  datapackId: string;
  /** The caller's email, so InviteHumansSection can drop self-invites from
   *  the payload before submit. */
  currentUserEmail?: string;
  /** The caller's grant on this datapack, read into the datapack_shared
   *  telemetry event as shared_by_role. */
  sharedByRole?: DatapackShareGrant | null;
};

export function InvitePeopleDialog({
  open,
  onOpenChange,
  datapackId,
  currentUserEmail,
  sharedByRole,
}: Props) {
  const navigate = useNavigate();
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="bg-card border-border sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle className="text-foreground">Invite people</DialogTitle>
        </DialogHeader>
        <InviteHumansSection
          datapackId={datapackId}
          currentUserEmail={currentUserEmail}
          sharedByRole={sharedByRole}
          onInvitesSent={(allClean) => {
            if (allClean) onOpenChange(false);
          }}
          onGoToMembers={() => {
            onOpenChange(false);
            navigate(datapackSharePath(datapackId));
          }}
        />
      </DialogContent>
    </Dialog>
  );
}
