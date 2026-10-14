// Full members roster page reached from the "View all" link on the
// DatapackDetailPage's Members tile. Owners + maintainers see everyone;
// viewers / contributors get the same forbidden state the tile shows.
//
// Mirrors the chrome (AppWorkspaceHeader → breadcrumbs → max-w-7xl main)
// of the other datapack sub-pages (SharedKnowledge, MemorySummary, …)
// so the navigation feel is consistent.

import { useEffect, useMemo, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { Users } from "lucide-react";
import { toast } from "@/hooks/use-toast";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";
import { DatapackBreadcrumbs } from "@/features/datapacks/components/datapack-detail/DatapackBreadcrumbs";
import { MemberRow } from "@/features/datapacks/components/datapack-members/MemberRow";
import { RemoveMemberDialog } from "@/features/datapacks/components/datapack-members/RemoveMemberDialog";
import {
  memberDisplayName,
  sortMembersForDisplay,
} from "@/features/datapacks/components/datapack-members/memberDisplay";
import { useDatapackName } from "@/features/datapacks/hooks/useDatapackName";
import { shareLinkKey } from "@/features/datapacks/hooks/useDatapackShareLink";
import { datapackDetailPath, ROUTES } from "@/app/routes/paths";
import {
  getApiErrorMessage,
  getCurrentUser,
  isMekoApiRequestError,
  listDatapackMembers,
  updateDatapackMemberGrant,
  type CurrentUser,
  type DatapackOutgoingMember,
} from "@/lib/api/mekoApi";
import { removeDatapackMember } from "@/lib/api/shareLinksApi";
import { isDatapackGrantEligibleToShare, type DatapackShareGrant } from "@/lib/datapackShareGrants";

type Props = {
  datapackId: string;
};

export default function DatapackMembersListPage({ datapackId }: Props) {
  const datapackName = useDatapackName(datapackId);
  const [members, setMembers] = useState<DatapackOutgoingMember[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [currentUser, setCurrentUser] = useState<CurrentUser | null>(null);
  // `user_id` of the row whose role is currently being PATCHed. Keeps
  // the select disabled mid-flight so a fast double-pick can't race
  // itself or skip the optimistic-then-reconcile reload below.
  const [updatingUserId, setUpdatingUserId] = useState<string | null>(null);
  const [removeTarget, setRemoveTarget] = useState<DatapackOutgoingMember | null>(null);
  const [removing, setRemoving] = useState(false);

  // Caller identity drives the "You" pill on the row that represents
  // the caller themselves. Best-effort: failing to resolve just means
  // the pill is omitted everywhere.
  useEffect(() => {
    const ac = new AbortController();
    void getCurrentUser({ signal: ac.signal })
      .then((u) => {
        if (!ac.signal.aborted) setCurrentUser(u);
      })
      .catch(() => {
        // Silent — drop the "You" pill if we can't tell.
      });
    return () => ac.abort();
  }, []);

  useEffect(() => {
    const ac = new AbortController();
    setLoading(true);
    setError(null);
    void listDatapackMembers(datapackId, { signal: ac.signal })
      .then((resp) => {
        if (ac.signal.aborted) return;
        setMembers(resp.members ?? []);
      })
      .catch((e) => {
        if (ac.signal.aborted) return;
        if ((e as { name?: string })?.name === "CanceledError") return;
        setError(
          isMekoApiRequestError(e) && e.httpStatus === 403
            ? "Members of this datapack aren't visible to viewers."
            : getApiErrorMessage(e, "Could not load members."),
        );
        setMembers([]);
      })
      .finally(() => {
        if (!ac.signal.aborted) setLoading(false);
      });
    return () => ac.abort();
  }, [datapackId]);

  const ordered = sortMembersForDisplay(members);

  // Role-management is gated on the caller's own grant on this datapack:
  // only owner or maintainer can change other members' roles. The
  // `isDatapackGrantEligibleToShare` helper encodes that same predicate
  // (its docstring: "Owner and maintainer may open sharing / manage
  // members") — reused here so the rule stays in one place. The owner's
  // row carries `grant === 'maintainer'` (BE convention in
  // GetOutgoingMembers), so the helper returns true for owners too.
  const canManageRoles = useMemo(() => {
    if (!currentUser) return false;
    const me = members.find((m) => m.user_id === currentUser.account_id);
    if (!me) return false;
    return isDatapackGrantEligibleToShare({ grant: me.grant });
  }, [currentUser, members]);

  // Reload members from the BE — used to reconcile after a successful
  // PATCH and as a hard rollback on failure.
  const queryClient = useQueryClient();
  // Role edits and removals change the link card's member counts.
  const refreshShareLink = () =>
    void queryClient.invalidateQueries({ queryKey: shareLinkKey(datapackId) });

  const reloadMembers = async (signal?: AbortSignal) => {
    try {
      const resp = await listDatapackMembers(datapackId, { signal });
      if (signal?.aborted) return;
      setMembers(resp.members ?? []);
    } catch {
      // Best-effort: leave whatever's on screen.
    }
  };

  const handleRoleChange = async (userID: string, newGrant: DatapackShareGrant) => {
    setUpdatingUserId(userID);
    // Snapshot the list BEFORE the optimistic flip so the catch path
    // can revert instantly, independent of the reload-from-server call
    // (which might itself fail or be slow). `updatingUserId` already
    // serialises role-changes to one in-flight, so restoring this
    // snapshot can't clobber a concurrent edit.
    const previousMembers = members;
    setMembers((prev) => prev.map((m) => (m.user_id === userID ? { ...m, grant: newGrant } : m)));
    try {
      await updateDatapackMemberGrant(datapackId, userID, { grant: newGrant });
      toast({ title: "Role updated" });
      refreshShareLink();
      // Reconcile from the server in case the BE normalised the grant
      // or another change landed concurrently.
      await reloadMembers();
    } catch (err) {
      // Immediate, network-independent revert — UI shows truth on the
      // same tick as the toast.
      setMembers(previousMembers);
      toast({
        title: "Couldn't update role",
        description: getApiErrorMessage(err, "Role change failed."),
      });
      // Fire-and-forget reconcile so the canonical list still lands
      // when the network recovers, without blocking the revert.
      void reloadMembers();
    } finally {
      setUpdatingUserId(null);
    }
  };

  const handleRemove = async (member: DatapackOutgoingMember) => {
    setRemoving(true);
    try {
      await removeDatapackMember(datapackId, member.user_id);
      setMembers((prev) => prev.filter((m) => m.user_id !== member.user_id));
      toast({ title: `${memberDisplayName(member)} was removed` });
      refreshShareLink();
      setRemoveTarget(null);
    } catch (err) {
      toast({
        title: "Couldn't remove member",
        description: getApiErrorMessage(err, "Removal failed."),
        variant: "destructive",
      });
    } finally {
      setRemoving(false);
    }
  };

  return (
    <div className="flex h-screen flex-1 flex-col overflow-y-auto bg-background text-foreground">
      <AppWorkspaceHeader />

      <main className="mx-auto flex w-full max-w-7xl flex-1 flex-col space-y-6 px-1 pb-12 pr-1 pt-6">
        <DatapackBreadcrumbs
          items={[
            { label: "Datapacks", href: ROUTES.datapacks },
            { label: datapackName, href: datapackDetailPath(datapackId) },
            { label: "Members" },
          ]}
        />

        <header className="flex items-center gap-2">
          <Users className="h-5 w-5 text-blue-400" aria-hidden />
          <h1 className="text-lg font-semibold text-foreground">Members</h1>
          <span className="rounded-full bg-muted px-2 py-0.5 text-xs font-medium text-muted-foreground tabular-nums">
            {loading ? "…" : ordered.length}
          </span>
        </header>

        {error ? (
          <div className="rounded-md border border-amber-500/40 bg-amber-500/10 px-4 py-3 text-sm text-amber-800 dark:text-amber-100">
            {error}
          </div>
        ) : null}

        {loading ? (
          <p className="rounded-lg border border-dashed border-border bg-muted/5 px-4 py-8 text-center text-sm text-muted-foreground">
            Loading members…
          </p>
        ) : ordered.length === 0 ? (
          <p className="rounded-lg border border-dashed border-border bg-muted/5 px-4 py-8 text-center text-sm text-muted-foreground">
            No members to display.
          </p>
        ) : (
          <ul className="space-y-2">
            {ordered.map((m) => {
              const isYou = currentUser != null && m.user_id === currentUser.account_id;
              return (
                <MemberRow
                  key={`${m.user_id}${m.is_owner ? "-owner" : ""}`}
                  member={m}
                  isYou={isYou}
                  // Show the editable dropdown only when (a) the caller
                  // is owner/maintainer, (b) the row isn't the owner row
                  // (owner can't be changed), and (c) the row isn't the
                  // caller themselves (no self-demotion).
                  canEditRole={canManageRoles && !m.is_owner && !isYou}
                  isUpdating={updatingUserId === m.user_id}
                  // One change at a time: the role-change revert restores a
                  // snapshot, which would bring back a member removed meanwhile.
                  actionsLocked={updatingUserId !== null || removing}
                  onRoleChange={handleRoleChange}
                  onRemove={setRemoveTarget}
                />
              );
            })}
          </ul>
        )}
      </main>
      <RemoveMemberDialog
        member={removeTarget}
        datapackName={datapackName}
        removing={removing}
        onCancel={() => setRemoveTarget(null)}
        onConfirm={(m) => void handleRemove(m)}
      />
    </div>
  );
}
