// Owner-facing page to invite collaborators, review pending invites, and manage member grants for a datapack.
// Loads member-list + outgoing-shares via useDatapackOutgoingShares; child actions use silent refetch to stay fresh.
import { useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { Bot, Share2, UsersRound } from "lucide-react";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";
import { DatapackBreadcrumbs } from "@/features/datapacks/components/datapack-detail/DatapackBreadcrumbs";
import { InviteAgentByTokenSection } from "@/features/datapacks/components/datapack-share/InviteAgentByTokenSection";
import { InviteHumansSection } from "@/features/datapacks/components/datapack-share/InviteHumansSection";
import { InviteModeTab } from "@/features/datapacks/components/datapack-share/InviteModeTab";
import { LinkSharingSection } from "@/features/datapacks/components/datapack-share/LinkSharingSection";
import { ShareLinkSection } from "@/features/datapacks/components/datapack-share/ShareLinkSection";
import { shareLinkKey } from "@/features/datapacks/hooks/useDatapackShareLink";
import { isLinkSharingEnabled } from "@/lib/linkSharingFeature";
import { MembersSection } from "@/features/datapacks/components/datapack-share/MembersSection";
import { PendingInvitesSection } from "@/features/datapacks/components/datapack-share/PendingInvitesSection";
import { ShareComingSoonPlaceholder } from "@/features/datapacks/components/datapack-share/ShareComingSoonPlaceholder";
import { TransferOwnershipSection } from "@/features/datapacks/components/datapack-share/TransferOwnershipSection";
import { useDatapackOutgoingShares } from "@/features/datapacks/components/datapack-share/useDatapackOutgoingShares";
import { useDatapackName } from "@/features/datapacks/hooks/useDatapackName";
import { datapackDetailPath, ROUTES } from "@/app/routes/paths";
import {
  DATAPACK_SHARE_GRANT_OWNER,
  isDatapackGrantEligibleToManageLinkSharing,
} from "@/lib/datapackShareGrants";
import type { CurrentUser } from "@/lib/api/mekoApi";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader } from "@/components/ui/card";
import { InlineErrorBanner } from "@/components/ui/inline-error-banner";
import { Tabs, TabsContent, TabsList } from "@/components/ui/tabs";

type Props = {
  datapackId: string;
  currentUser: CurrentUser | null;
};

type InviteMode = "humans" | "agent";

export default function DatapackSharePage({ datapackId, currentUser }: Props) {
  const datapackName = useDatapackName(datapackId);
  const [inviteMode, setInviteMode] = useState<InviteMode>("humans");
  const { data, loading, error, membersHidden, refetch } = useDatapackOutgoingShares(datapackId);
  const members = data?.members ?? [];
  const pendingInvites = data?.pending_invites ?? [];

  // Telemetry inputs for the datapack_shared event: the sharer's own role on this
  // datapack, and how many shares already exist (accepted members excl. owner +
  // pending invites) — drives is_first_share / total_shares.
  //
  // The member list tags the OWNER's own row as "maintainer" (BE quirk in
  // GetOutgoingMembers), so reading `.grant` would report every owner as a
  // maintainer. Use the is_owner flag to resolve the real role.
  const me = members.find((m) => m.user_email_id === currentUser?.email);
  const sharedByRole = me ? (me.is_owner ? DATAPACK_SHARE_GRANT_OWNER : me.grant) : null;
  const priorShareCount = members.filter((m) => !m.is_owner).length + pendingInvites.length;

  /** After invites or member mutations, refresh lists without toggling the global loading skeleton. */
  const queryClient = useQueryClient();
  const refreshShares = () => void refetch({ silent: true });
  // A member edit changes the link card's counts.
  const refreshSharesAndLink = () => {
    refreshShares();
    void queryClient.invalidateQueries({ queryKey: shareLinkKey(datapackId) });
  };

  return (
    <div className="flex-1 h-screen overflow-y-auto bg-background text-foreground flex flex-col">
      <AppWorkspaceHeader />

      <main className="flex-1 p-8 max-w-6xl mx-auto w-full space-y-8 pb-16">
        <DatapackBreadcrumbs
          items={[
            { label: "Datapacks", href: ROUTES.datapacks },
            { label: datapackName, href: datapackDetailPath(datapackId) },
            { label: "Share" },
          ]}
        />

        <div className="space-y-2">
          <div className="flex flex-wrap items-center gap-3">
            <Share2 className="h-8 w-8 shrink-0 text-brand" aria-hidden />
            <h1 className="text-2xl sm:text-3xl font-semibold tracking-tight text-foreground truncate">
              Share {datapackName}
            </h1>
          </div>
          <p className="text-sm text-muted-foreground max-w-2xl leading-relaxed">
            Give humans and agents access to this datapack&apos;s knowledge and memories.
          </p>
        </div>

        {error ? (
          <InlineErrorBanner
            action={
              <Button
                type="button"
                size="sm"
                variant="outline"
                className="shrink-0 border-red-400/40 bg-red-950/20 text-red-700 dark:text-red-100 hover:bg-red-950/40 hover:text-red-50"
                onClick={() => void refetch()}
              >
                Retry
              </Button>
            }
          >
            <p className="font-medium text-red-700 dark:text-red-100">
              Could not fetch member and invite details
            </p>
            <p className="mt-1 text-xs font-normal text-red-700 dark:text-red-200/85">{error}</p>
          </InlineErrorBanner>
        ) : null}

        {loading && !error ? (
          // Until the member list answers we can't tell a viewer (refused it)
          // from a manager, so neither the invite controls nor the notice show.
          <Card
            className="h-40 animate-pulse border-border bg-card/50 shadow-sm"
            aria-busy="true"
          />
        ) : membersHidden ? (
          <Card className="border-border bg-card/50 p-6 shadow-sm">
            <p className="text-sm font-medium text-foreground">
              You have viewer access to this datapack
            </p>
            <p className="mt-1 text-sm text-muted-foreground">
              Its owners and maintainers manage who it&apos;s shared with. Members aren&apos;t
              visible to viewers.
            </p>
          </Card>
        ) : (
          <div className="space-y-6">
            <Card className="border-border bg-card/50 shadow-sm">
              <Tabs value={inviteMode} onValueChange={(v) => setInviteMode(v as InviteMode)}>
                <CardHeader className="space-y-0 px-6 pb-0 pt-6">
                  <TabsList className="grid h-auto w-full min-w-0 grid-cols-1 gap-0 rounded-none border-0 bg-transparent p-0 sm:grid-cols-2">
                    <InviteModeTab
                      value="humans"
                      icon={UsersRound}
                      primaryLabel="Invite humans"
                      secondaryLabel="one or many"
                      activeTone="brand"
                      withDivider
                    />
                    <InviteModeTab
                      value="agent"
                      icon={Bot}
                      primaryLabel="Invite an agent"
                      secondaryLabel="by token"
                      activeTone="violet"
                    />
                  </TabsList>
                </CardHeader>
                <CardContent className="p-6 pt-6">
                  <TabsContent
                    value="humans"
                    className="mt-0 outline-none focus-visible:outline-none"
                  >
                    <InviteHumansSection
                      datapackId={datapackId}
                      currentUserEmail={currentUser?.email}
                      sharedByRole={sharedByRole}
                      priorShareCount={priorShareCount}
                      onInvitesSent={refreshShares}
                    />
                  </TabsContent>
                  <TabsContent
                    value="agent"
                    className="mt-0 outline-none focus-visible:outline-none"
                  >
                    <ShareComingSoonPlaceholder
                      aria-label="Invite an agent by token — coming soon"
                      blurredPreview={<InviteAgentByTokenSection />}
                    />
                  </TabsContent>
                </CardContent>
              </Tabs>
            </Card>

            <MembersSection
              currentUser={currentUser}
              datapackName={datapackName}
              datapackId={datapackId}
              members={members}
              loading={loading}
              onMembersChanged={refreshSharesAndLink}
            />
            <PendingInvitesSection
              datapackId={datapackId}
              invites={pendingInvites}
              loading={loading}
              onChanged={refreshShares}
            />
            {/* Link sharing is UI-gated to the owner. The API (GET/PUT
                /datapacks/:id/share-link) still accepts a maintainer today;
                see isDatapackGrantEligibleToManageLinkSharing. A maintainer
                never sees this card — they still invite people explicitly
                through the Invite humans card above. */}
            {isLinkSharingEnabled() &&
            isDatapackGrantEligibleToManageLinkSharing({ grant: sharedByRole }) ? (
              <ShareLinkSection datapackId={datapackId} onMembersMoved={refreshShares} />
            ) : isLinkSharingEnabled() ? null : (
              <LinkSharingSection />
            )}
            <TransferOwnershipSection />
          </div>
        )}
      </main>
    </div>
  );
}
