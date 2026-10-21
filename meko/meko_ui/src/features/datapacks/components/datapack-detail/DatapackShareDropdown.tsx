// Share split button on the datapack-detail header (MEKO-1009). One obvious
// entry point for every share path:
//
//   - Invite People          → opens InvitePeopleDialog, which wraps
//                              InviteHumansSection — single OR bulk (CSV /
//                              pasted list), self-filter, telemetry
//   - Create / Copy share link → opens CreateShareLinkDialog when no link row
//                                exists yet, copies the active link directly
//                                when one does, or routes to the Share page
//                                when a row exists but is off (see
//                                useShareLinkAction for why)
//   - Settings                → the Share page's root
//
// The dropdown itself is gated on isDatapackGrantEligibleToShare at the call
// site (owner + maintainer). The link item is further gated on
// canManageLinkSharing — owner-only in the UI — since a shareable URL goes
// further than a named invite does.

import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { ChevronDown, Link2, Settings as SettingsIcon, Share2, UserPlus } from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { datapackSharePath } from "@/app/routes/paths";
import type { DatapackShareGrant } from "@/lib/datapackShareGrants";
import { CreateShareLinkDialog } from "./CreateShareLinkDialog";
import { InvitePeopleDialog } from "./InvitePeopleDialog";
import { useShareLinkAction } from "./useShareLinkAction";

type Props = {
  datapackId: string;
  /** True when the caller is the datapack owner AND the deployment has link
   *  sharing on. Hides the link item for a maintainer (who can still invite
   *  people explicitly). */
  canManageLinkSharing: boolean;
  /** Forwarded to InvitePeopleDialog → InviteHumansSection, which uses it to
   *  drop self-invites from the payload. */
  currentUserEmail?: string;
  /** Caller's grant on this datapack, forwarded for the datapack_shared
   *  telemetry event's shared_by_role property. */
  sharedByRole?: DatapackShareGrant | null;
};

export function DatapackShareDropdown({
  datapackId,
  canManageLinkSharing,
  currentUserEmail,
  sharedByRole,
}: Props) {
  const navigate = useNavigate();
  const [inviteOpen, setInviteOpen] = useState(false);
  const [createLinkOpen, setCreateLinkOpen] = useState(false);
  const linkAction = useShareLinkAction(datapackId, canManageLinkSharing, {
    onCreate: () => setCreateLinkOpen(true),
  });

  return (
    <>
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button
            type="button"
            variant="outline"
            className="gap-2 border-border bg-background/80"
            aria-label="Share this datapack"
          >
            <Share2 className="h-4 w-4 shrink-0" aria-hidden />
            Share
            <ChevronDown className="h-4 w-4 shrink-0 opacity-80" aria-hidden />
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end" className="w-56">
          <DropdownMenuItem
            className="gap-2 cursor-pointer focus:bg-muted/20"
            onSelect={() => setInviteOpen(true)}
          >
            <UserPlus className="h-4 w-4 shrink-0" aria-hidden />
            Invite People
          </DropdownMenuItem>

          {canManageLinkSharing ? (
            <DropdownMenuItem
              className="gap-2 cursor-pointer focus:bg-muted/20"
              onSelect={linkAction.run}
            >
              <Link2 className="h-4 w-4 shrink-0" aria-hidden />
              {linkAction.label}
            </DropdownMenuItem>
          ) : null}

          <DropdownMenuSeparator />

          <DropdownMenuItem
            className="gap-2 cursor-pointer focus:bg-muted/20"
            onSelect={() => navigate(datapackSharePath(datapackId))}
          >
            <SettingsIcon className="h-4 w-4 shrink-0" aria-hidden />
            Settings
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>

      <InvitePeopleDialog
        open={inviteOpen}
        onOpenChange={setInviteOpen}
        datapackId={datapackId}
        currentUserEmail={currentUserEmail}
        sharedByRole={sharedByRole}
      />
      {canManageLinkSharing ? (
        <CreateShareLinkDialog
          open={createLinkOpen}
          onOpenChange={setCreateLinkOpen}
          datapackId={datapackId}
        />
      ) : null}
    </>
  );
}
