// Lesson 2 of "Deploy an agent": share the Chapter-1 datapack with the
// account the shared AgentK deployment authenticates as. Same UI as
// CollaborateStage (Lesson 4) -- invite form + members + pending invites --
// except the recipient/grant are fixed (MEKO_LABS_AGENTK_ADMIN_EMAIL,
// contributor), not a user choice: this is sharing with a configured
// service account, not inviting a teammate. api_server auto-accepts invites
// to that one configured address instead of leaving them pending, so
// AgentK typically appears in Members immediately after sending, rather
// than sitting in Pending invites the way a real teammate's invite would.

import { useEffect, useRef, useState } from "react";

import { inviteDatapackShare, getApiErrorMessage } from "@/lib/api/mekoApi";
import { useDatapackOutgoingShares } from "@/features/datapacks/components/datapack-share/useDatapackOutgoingShares";
import {
  DATAPACK_SHARE_GRANT_CONTRIBUTOR,
  DATAPACK_SHARE_GRANT_OWNER,
  isDatapackGrantEligibleToShare,
} from "@/lib/datapackShareGrants";
import { readRuntimeEnv } from "@/lib/runtimeEnv";

import { AgentGrantCard } from "../demo-apps/AgentGrantCard";
import { LabStageNeedsDatapack } from "../lab-runner/LabStageNeedsDatapack";
import { LabStageHeader } from "./LabStageHeader";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";

// Same env var name as api_server's MEKO_LABS_AGENTK_ADMIN_EMAIL
// (readRuntimeEnv, not import.meta.env directly, so an operator can set it
// at deploy time without a rebuild -- see lib/runtimeEnv.ts).
const AGENTK_ADMIN_EMAIL = readRuntimeEnv("MEKO_LABS_AGENTK_ADMIN_EMAIL") ?? "";

/** This lab's agent. Not threaded through as a prop because the stage is
 *  already specific to it -- the configured admin address below names it. */
const AGENT_DISPLAY_NAME = "AgentK";

export function ShareWithAgentStage({
  session,
  onGoToCreateDatapack,
  onComplete,
}: {
  session: ReturnType<typeof useLabRunnerSession>;
  onGoToCreateDatapack: () => void;
  /** Satisfies this stage's gate once AgentK actually holds a grant. */
  onComplete: () => void;
}) {
  if (!session.datapackId) {
    return <LabStageNeedsDatapack onGoToCreateDatapack={onGoToCreateDatapack} />;
  }
  return (
    <ShareWithAgentStageContent
      datapackId={session.datapackId}
      session={session}
      onComplete={onComplete}
    />
  );
}

function ShareWithAgentStageContent({
  datapackId,
  session,
  onComplete,
}: {
  datapackId: string;
  session: ReturnType<typeof useLabRunnerSession>;
  onComplete: () => void;
}) {
  const [sharing, setSharing] = useState(false);
  const [shareError, setShareError] = useState<string | null>(null);
  const { data, loading, error, refetch } = useDatapackOutgoingShares(datapackId);
  const members = data?.members ?? [];
  const pendingInvites = data?.pending_invites ?? [];

  // Same is_owner resolution CollaborateStage uses (the BE tags the OWNER's
  // own row as "maintainer").
  const me = members.find((m) => m.user_email_id === session.currentUser?.email);
  const sharedByRole = me ? (me.is_owner ? DATAPACK_SHARE_GRANT_OWNER : me.grant) : null;
  const priorShareCount = members.filter((m) => !m.is_owner).length + pendingInvites.length;

  // AgentK typically shows up in Members immediately (api_server auto-accepts
  // its one configured address -- see this file's header comment), but check
  // pending invites too as a defensive fallback in case auto-accept hasn't
  // landed yet in the deployed api_server. Either way, sending another
  // invite to the same fixed recipient would just be a redundant duplicate.
  const agentkEmail = AGENTK_ADMIN_EMAIL.trim().toLowerCase();
  const alreadySharedWithAgent =
    agentkEmail.length > 0 &&
    (members.some((m) => m.user_email_id?.trim().toLowerCase() === agentkEmail) ||
      pendingInvites.some((p) => p.recipient_email_id?.trim().toLowerCase() === agentkEmail));

  // Derived from the share list rather than from the invite click: a user who
  // shared in an earlier run arrives with this already true, and should not
  // be asked to share again to get past the gate.
  //
  // `onComplete` is deliberately read through a ref rather than listed as a
  // dependency. It is a fresh arrow on every parent render, so depending on
  // its identity re-runs this effect every render -- which is only harmless
  // while the callback stays idempotent, and is a render loop the moment it
  // isn't.
  const onCompleteRef = useRef(onComplete);
  onCompleteRef.current = onComplete;
  useEffect(() => {
    if (alreadySharedWithAgent) onCompleteRef.current();
  }, [alreadySharedWithAgent]);

  const refreshShares = () => void refetch({ silent: true });

  // Same gate CollaborateStage applies -- almost always true here since
  // this is the user's own lab-created datapack, kept for consistency.
  const canInvite = isDatapackGrantEligibleToShare({ grant: sharedByRole });

  const handleShare = async () => {
    setSharing(true);
    setShareError(null);
    try {
      await inviteDatapackShare(
        datapackId,
        { recipient_email: agentkEmail, grant: DATAPACK_SHARE_GRANT_CONTRIBUTOR },
        {},
      );
      // api_server auto-accepts this one configured address, so the grant is
      // live immediately -- refetching is what flips alreadySharedWithAgent
      // and releases the gate.
      refreshShares();
    } catch (err) {
      setShareError(getApiErrorMessage(err, "Couldn't share the datapack with AgentK."));
    } finally {
      setSharing(false);
    }
  };

  return (
    <div className="flex flex-1 flex-col gap-6 overflow-y-auto p-6">
      <LabStageHeader
        eyebrow="Chapter 2 · Step 2"
        title="Share the datapack, not a copy"
        lede={`Sharing is a grant, not a transfer. ${AGENT_DISPLAY_NAME} reads the live datapack, so a rule you edit at 4pm lands in the 4:01 review. Nothing is exported, duplicated, or re-embedded.`}
        tags={["Sharing"]}
      />

      {!AGENTK_ADMIN_EMAIL ? (
        <p className="text-sm text-destructive">
          This lab isn't configured with an AgentK account to share with yet.
        </p>
      ) : !canInvite ? (
        <p className="text-sm text-muted-foreground">
          You need maintainer access on this datapack to share it.
        </p>
      ) : (
        <AgentGrantCard
          agentDisplayName={AGENT_DISPLAY_NAME}
          agentEmail={AGENTK_ADMIN_EMAIL}
          grant={DATAPACK_SHARE_GRANT_CONTRIBUTOR}
          shared={alreadySharedWithAgent}
          sharing={sharing}
          onShare={() => void handleShare()}
        />
      )}

      {(error || shareError) && <p className="text-sm text-destructive">{shareError ?? error}</p>}

      <p className="text-xs text-muted-foreground">
        Several humans and agents can hold this grant at once. Concurrent reads while someone edits are
        fine: readers see the new version on their next query. Manage everyone on this datapack from its
        own sharing page.
      </p>
    </div>
  );
}
