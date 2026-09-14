// Stage 5: a direct port of DatapackSharePage.tsx's own composition (see
// its lines 44-60, 169-200), minus that page's chrome, invite-mode tabs,
// and LinkSharingSection/TransferOwnershipSection (both still coming-soon
// stubs there, not gated on role -- not worth porting yet).

import { InviteByEmailSection } from "@/features/datapacks/components/datapack-share/InviteByEmailSection";
import { MembersSection } from "@/features/datapacks/components/datapack-share/MembersSection";
import { PendingInvitesSection } from "@/features/datapacks/components/datapack-share/PendingInvitesSection";
import { useDatapackOutgoingShares } from "@/features/datapacks/components/datapack-share/useDatapackOutgoingShares";
import {
  DATAPACK_SHARE_GRANT_OWNER,
  isDatapackGrantEligibleToShare,
} from "@/lib/datapackShareGrants";

import { LabStageNeedsDatapack } from "../lab-runner/LabStageNeedsDatapack";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import { LabStageHeader } from "./LabStageHeader";

export function CollaborateStage({
  session,
  onGoToCreateDatapack,
}: {
  session: ReturnType<typeof useLabRunnerSession>;
  onGoToCreateDatapack: () => void;
}) {
  if (!session.datapackId) {
    return <LabStageNeedsDatapack onGoToCreateDatapack={onGoToCreateDatapack} />;
  }
  return <CollaborateStageContent datapackId={session.datapackId} session={session} />;
}

function CollaborateStageContent({
  datapackId,
  session,
}: {
  datapackId: string;
  session: ReturnType<typeof useLabRunnerSession>;
}) {
  const { data, loading, error, refetch } = useDatapackOutgoingShares(datapackId);
  const members = data?.members ?? [];
  const pendingInvites = data?.pending_invites ?? [];

  // The member list tags the OWNER's own row as "maintainer" (BE quirk) —
  // use is_owner to resolve the real role, same as DatapackSharePage.
  const me = members.find((m) => m.user_email_id === session.currentUser?.email);
  const sharedByRole = me ? (me.is_owner ? DATAPACK_SHARE_GRANT_OWNER : me.grant) : null;
  const priorShareCount = members.filter((m) => !m.is_owner).length + pendingInvites.length;

  const refreshShares = () => void refetch({ silent: true });

  // Same owner/maintainer gate every other entry point to DatapackSharePage
  // enforces (DatapackDetailPage, DatapacksTilesSection) -- accessibleDatapacks
  // also admits contributors (see useLabRunnerSession), and InviteByEmailSection
  // itself doesn't gate on role, so without this a contributor gets a live
  // send-invite form backed only by the BE's 403.
  const canInvite = isDatapackGrantEligibleToShare({ grant: sharedByRole });

  return (
    <div className="flex flex-1 flex-col gap-6 overflow-y-auto p-4">
      <LabStageHeader
        eyebrow="Chapter 3 · Step 4"
        title="Collaborate"
        lede="Share this datapack with teammates so their agents read the same knowledge and memories yours just did."
        tags={["Sharing"]}
      />
      {error && <p className="text-sm text-destructive">{error}</p>}
      {canInvite && (
        <InviteByEmailSection
          datapackId={datapackId}
          currentUserEmail={session.currentUser?.email}
          sharedByRole={sharedByRole}
          priorShareCount={priorShareCount}
          onInviteSent={refreshShares}
        />
      )}
      <MembersSection
        currentUser={session.currentUser}
        datapackName={session.datapackName ?? ""}
        datapackId={datapackId}
        members={members}
        loading={loading}
        onMembersChanged={refreshShares}
      />
      <PendingInvitesSection
        datapackId={datapackId}
        invites={pendingInvites}
        loading={loading}
        onChanged={refreshShares}
      />
    </div>
  );
}
