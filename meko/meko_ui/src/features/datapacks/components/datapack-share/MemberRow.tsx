// One member row inside the Members roster. Owner rows show a crown +
// static "Owner" label; every other row shows the current grant as a
// read-only pill (for non-managers) or a Select whose change is
// gated behind the confirmation dialog (for owners).
//
// Extracted out of MembersSection so the roster component focuses on
// list + filter + toolbar composition rather than per-row lifecycle.
import { useState } from "react";
import { Crown } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import type { CurrentUser, DatapackOutgoingMember, DatapackShareGrant } from "@/lib/api/mekoApi";
import { getApiErrorMessage, updateDatapackMemberGrant } from "@/lib/api/mekoApi";
import { DatapackChangeRoleConfirmDialog } from "@/features/datapacks/components/datapack-share/DatapackChangeRoleConfirmDialog";
import {
  SHARE_INVITE_ROLES,
  shareGrantLabel,
} from "@/features/datapacks/components/datapack-share/shareInviteRoles";
import { toast } from "@/hooks/use-toast";

export type MemberRowProps = {
  member: DatapackOutgoingMember;
  datapackName: string;
  datapackId: string;
  currentUserEmail: string | null | undefined;
  canManage: boolean;
  onMembersChanged: () => void | Promise<void>;
};

function emailsMatch(a: string | null | undefined, b: string | null | undefined): boolean {
  if (!a || !b) return false;
  return a.trim().toLowerCase() === b.trim().toLowerCase();
}

function memberInitial(member: DatapackOutgoingMember): string {
  const n = member.user_name?.trim();
  if (n) return n.slice(0, 1).toUpperCase();
  const e = member.user_email_id?.trim();
  if (e) return e.slice(0, 1).toUpperCase();
  return "?";
}

export function MemberRow({
  member,
  datapackName,
  datapackId,
  currentUserEmail,
  canManage,
  onMembersChanged,
}: MemberRowProps) {
  const [grantBusy, setGrantBusy] = useState(false);
  // The role the user just picked from the dropdown but hasn't yet
  // confirmed. Drives the confirmation modal; cleared on
  // cancel/success. Keeping it as the pending target (not just a
  // boolean) lets the modal say "from Contributor to Maintainer" using
  // the exact value the user clicked.
  const [pendingGrant, setPendingGrant] = useState<DatapackShareGrant | null>(null);
  const isYou = emailsMatch(member.user_email_id, currentUserEmail);
  // Owners never change grant here; non-owners see read-only grant unless the signed-in user owns the datapack.
  const grantReadOnly = member.is_owner || !canManage;

  const handleGrantPick = (next: DatapackShareGrant) => {
    if (next === member.grant || grantReadOnly) return;
    setPendingGrant(next);
  };

  const handleGrantConfirm = async () => {
    if (!pendingGrant) return;
    setGrantBusy(true);
    try {
      await updateDatapackMemberGrant(datapackId, member.user_id, { grant: pendingGrant });
      toast({ title: "Role updated" });
      setPendingGrant(null);
      await onMembersChanged();
    } catch (err) {
      // Re-throw so the dialog surfaces the error inline and keeps the
      // modal open; the busy state below resets in `finally`.
      toast({
        variant: "destructive",
        title: "Could not update role",
        description: getApiErrorMessage(err, "Something went wrong. Try again."),
      });
      throw err;
    } finally {
      setGrantBusy(false);
    }
  };

  const roleControl = member.is_owner ? (
    <div
      className="flex h-9 items-center gap-2 pl-2 mr-4 text-sm font-medium text-foreground"
      title="Datapack owner"
    >
      <Crown className="h-4 w-4 shrink-0 text-amber-400" aria-hidden />
      <span>Owner</span>
    </div>
  ) : grantReadOnly ? (
    <div className="flex h-9 min-w-[148px] items-center rounded-md border border-border bg-muted/30 px-3 text-xs text-muted-foreground">
      {shareGrantLabel(member.grant)}
    </div>
  ) : (
    // Controlled Select: value stays at the CURRENT grant until the
    // user confirms in the modal — that way cancelling the dialog
    // leaves the dropdown reflecting reality (no re-sync needed).
    <Select
      value={member.grant}
      onValueChange={(v) => handleGrantPick(v as DatapackShareGrant)}
      disabled={grantBusy}
    >
      <SelectTrigger className="h-9 w-[min(100%,11rem)] min-w-[148px] text-xs bg-background/80 sm:w-44">
        <SelectValue />
      </SelectTrigger>
      <SelectContent>
        {SHARE_INVITE_ROLES.map((r) => (
          <SelectItem key={r.id} value={r.id}>
            {r.label}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  );

  return (
    <div className="flex w-full items-center justify-between gap-4 px-3 py-3.5">
      <div className="flex min-w-0 flex-1 items-center gap-3">
        <div
          className="flex h-9 w-9 shrink-0 items-center justify-center rounded-full bg-muted text-sm font-semibold text-foreground"
          aria-hidden
        >
          {memberInitial(member)}
        </div>
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-2">
            <span className="truncate text-sm font-medium text-foreground">
              {member.user_name || member.user_email_id}
            </span>
            {isYou ? (
              <Badge
                variant="brand"
                className="shrink-0 px-2 py-0 text-[10px] font-semibold uppercase tracking-wide"
              >
                you
              </Badge>
            ) : null}
          </div>
          <div className="truncate text-xs text-muted-foreground">{member.user_email_id}</div>
        </div>
      </div>
      <div className="flex shrink-0 items-center justify-end gap-2">
        <span className="flex shrink-0 items-center justify-end gap-2">{roleControl}</span>
      </div>
      {pendingGrant ? (
        <DatapackChangeRoleConfirmDialog
          open={pendingGrant !== null}
          onOpenChange={(next) => {
            if (!next) setPendingGrant(null);
          }}
          memberName={member.user_name || member.user_email_id || "this member"}
          datapackName={datapackName}
          fromRoleLabel={shareGrantLabel(member.grant)}
          toRoleLabel={shareGrantLabel(pendingGrant)}
          onConfirm={handleGrantConfirm}
        />
      ) : null}
    </div>
  );
}

// Helper the parent uses to decide if the signed-in user can manage grants.
// Kept here so both MemberRow and MembersSection agree on the definition
// (owner row with email == session email).
export function isCurrentUserOwner(
  members: DatapackOutgoingMember[],
  currentUser: CurrentUser | null,
): boolean {
  return members.some((m) => m.is_owner && emailsMatch(m.user_email_id, currentUser?.email));
}
