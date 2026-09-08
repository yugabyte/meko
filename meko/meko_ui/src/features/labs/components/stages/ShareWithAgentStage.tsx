// Lesson 2 of "Deploy an agent": share the Chapter-1 datapack with the
// account the shared AgentK deployment authenticates as. Same UI as
// CollaborateStage (Lesson 4) -- invite form + members + pending invites --
// except the recipient/grant are fixed (MEKO_LABS_AGENTK_ADMIN_EMAIL,
// contributor), not a user choice: this is sharing with a configured
// service account, not inviting a teammate. api_server auto-accepts invites
// to that one configured address instead of leaving them pending, so
// AgentK typically appears in Members immediately after sending, rather
// than sitting in Pending invites the way a real teammate's invite would.

import { InviteByEmailSection } from "@/features/datapacks/components/datapack-share/InviteByEmailSection";
import { MembersSection } from "@/features/datapacks/components/datapack-share/MembersSection";
import { PendingInvitesSection } from "@/features/datapacks/components/datapack-share/PendingInvitesSection";
import { useDatapackOutgoingShares } from "@/features/datapacks/components/datapack-share/useDatapackOutgoingShares";
import {
  DATAPACK_SHARE_GRANT_CONTRIBUTOR,
  DATAPACK_SHARE_GRANT_OWNER,
  isDatapackGrantEligibleToShare,
} from "@/lib/datapackShareGrants";
import { readRuntimeEnv } from "@/lib/runtimeEnv";

import { LabStageNeedsDatapack } from "../lab-runner/LabStageNeedsDatapack";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";

// Same env var name as api_server's MEKO_LABS_AGENTK_ADMIN_EMAIL
// (readRuntimeEnv, not import.meta.env directly, so an operator can set it
// at deploy time without a rebuild -- see lib/runtimeEnv.ts).
const AGENTK_ADMIN_EMAIL = readRuntimeEnv("MEKO_LABS_AGENTK_ADMIN_EMAIL") ?? "";

export function ShareWithAgentStage({
  session,
  onGoToCreateDatapack,
}: {
  session: ReturnType<typeof useLabRunnerSession>;
  onGoToCreateDatapack: () => void;
}) {
  if (!session.datapackId) {
    return <LabStageNeedsDatapack onGoToCreateDatapack={onGoToCreateDatapack} />;
  }
  return <ShareWithAgentStageContent datapackId={session.datapackId} session={session} />;
}

function ShareWithAgentStageContent({
  datapackId,
  session,
}: {
  datapackId: string;
  session: ReturnType<typeof useLabRunnerSession>;
}) {
  const { data, loading, error, refetch } = useDatapackOutgoingShares(datapackId);
  const members = data?.members ?? [];
  const pendingInvites = data?.pending_invites ?? [];

  // Same is_owner resolution CollaborateStage uses (the BE tags the OWNER's
  // own row as "maintainer").
  const me = members.find((m) => m.user_email_id === session.currentUser?.email);
  const sharedByRole = me ? (me.is_owner ? DATAPACK_SHARE_GRANT_OWNER : me.grant) : null;
  const priorShareCount = members.filter((m) => !m.is_owner).length + pendingInvites.length;

  const refreshShares = () => void refetch({ silent: true });

  // Same gate CollaborateStage applies -- almost always true here since
  // this is the user's own lab-created datapack, kept for consistency.
  const canInvite = isDatapackGrantEligibleToShare({ grant: sharedByRole });

  return (
    <div className="flex flex-1 flex-col gap-6 overflow-y-auto p-4">
      <p className="text-sm text-muted-foreground">
        AgentK reads your datapack under its own account, not yours — share it once so the
        shared instance (or your own dedicated deployment) can see what you just built.
      </p>
      {!AGENTK_ADMIN_EMAIL && (
        <p className="text-sm text-destructive">
          This lab isn't configured with an AgentK account to share with yet.
        </p>
      )}
      {error && <p className="text-sm text-destructive">{error}</p>}
      {canInvite && AGENTK_ADMIN_EMAIL && (
        <InviteByEmailSection
          datapackId={datapackId}
          currentUserEmail={session.currentUser?.email}
          sharedByRole={sharedByRole}
          priorShareCount={priorShareCount}
          onInviteSent={refreshShares}
          lockedEmail={AGENTK_ADMIN_EMAIL}
          lockedGrant={DATAPACK_SHARE_GRANT_CONTRIBUTOR}
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
