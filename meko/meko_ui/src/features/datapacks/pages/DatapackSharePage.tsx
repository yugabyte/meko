// Owner-facing page to invite collaborators, review pending invites, and manage member grants for a datapack.
// Loads member-list + outgoing-shares via useDatapackOutgoingShares; child actions use silent refetch to stay fresh.
import { useState } from "react";
import { Bot, Mail, Share2 } from "lucide-react";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";
import { DatapackBreadcrumbs } from "@/features/datapacks/components/datapack-detail/DatapackBreadcrumbs";
import { InviteAgentByTokenSection } from "@/features/datapacks/components/datapack-share/InviteAgentByTokenSection";
import { InviteByEmailSection } from "@/features/datapacks/components/datapack-share/InviteByEmailSection";
import { LinkSharingSection } from "@/features/datapacks/components/datapack-share/LinkSharingSection";
import { MembersSection } from "@/features/datapacks/components/datapack-share/MembersSection";
import { PendingInvitesSection } from "@/features/datapacks/components/datapack-share/PendingInvitesSection";
import { ShareComingSoonPlaceholder } from "@/features/datapacks/components/datapack-share/ShareComingSoonPlaceholder";
import { TransferOwnershipSection } from "@/features/datapacks/components/datapack-share/TransferOwnershipSection";
import { useDatapackOutgoingShares } from "@/features/datapacks/components/datapack-share/useDatapackOutgoingShares";
import { useDatapackName } from "@/features/datapacks/hooks/useDatapackName";
import { datapackDetailPath, ROUTES } from "@/app/routes/paths";
import { DATAPACK_SHARE_GRANT_OWNER } from "@/lib/datapackShareGrants";
import type { CurrentUser } from "@/lib/api/mekoApi";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader } from "@/components/ui/card";
import { InlineErrorBanner } from "@/components/ui/inline-error-banner";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { cn } from "@/lib/utils";

type Props = {
  datapackId: string;
  currentUser: CurrentUser | null;
};

type InviteMode = "person" | "agent";

/** Full-width segmented tabs: equal columns, centered label, bottom rule spans the whole cell (not content width). */
const inviteTabTriggerClass = cn(
  "relative flex w-full min-w-0 min-h-[3rem] items-center justify-center gap-0 whitespace-normal rounded-none border-0 border-b-2 border-border bg-transparent px-3 py-3 text-sm font-normal leading-normal shadow-none ring-0 ring-offset-0 transition-colors",
  "hover:bg-muted/25 focus-visible:z-10 focus-visible:ring-2 focus-visible:ring-blue-500 focus-visible:ring-offset-2 focus-visible:ring-offset-background",
  "data-[state=active]:text-inherit data-[state=active]:shadow-none data-[state=active]:ring-0 data-[state=active]:ring-offset-0",
  "data-[state=inactive]:text-muted-foreground data-[state=inactive]:opacity-90 data-[state=inactive]:hover:bg-muted/20 data-[state=inactive]:hover:opacity-100",
);

export default function DatapackSharePage({ datapackId, currentUser }: Props) {
  const datapackName = useDatapackName(datapackId);
  const [inviteMode, setInviteMode] = useState<InviteMode>("person");
  const { data, loading, error, refetch } = useDatapackOutgoingShares(datapackId);
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
  const refreshShares = () => void refetch({ silent: true });

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
            <p className="font-medium text-red-700 dark:text-red-100">Could not fetch member and invite details</p>
            <p className="mt-1 text-xs font-normal text-red-700 dark:text-red-200/85">{error}</p>
          </InlineErrorBanner>
        ) : null}

        <div className="space-y-6">
          <Card className="border-border bg-card/50 shadow-sm">
            <Tabs value={inviteMode} onValueChange={(v) => setInviteMode(v as InviteMode)}>
              <CardHeader className="space-y-0 px-6 pb-0 pt-6">
                <TabsList className="grid h-auto w-full min-w-0 grid-cols-2 gap-0 rounded-none border-0 bg-transparent p-0">
                  <TabsTrigger
                    value="person"
                    className={cn(
                      inviteTabTriggerClass,
                      "group border-r border-border/70",
                      // Soft blue tint in light mode, deep blue glow in
                      // dark — `blue-950/25` on a white page rendered
                      // as a washed-out dark-grey block.
                      "data-[state=active]:border-b-brand data-[state=active]:bg-brand/10 data-[state=active]:text-foreground",
                    )}
                  >
                    <span className="inline-flex max-w-full items-center justify-center gap-2.5">
                      <Mail
                        className="block size-4 shrink-0 text-muted-foreground group-data-[state=active]:text-brand"
                        aria-hidden
                      />
                      <span className="inline-flex min-h-[1.25rem] max-w-full flex-wrap items-baseline justify-center gap-x-0 text-center whitespace-normal">
                        <span className="mt-0.5 font-semibold leading-none group-data-[state=active]:text-foreground">
                          Invite a human&nbsp;
                        </span>
                        <span className="font-normal leading-none text-muted-foreground group-data-[state=active]:text-muted-foreground">
                          {" "}
                          · by email
                        </span>
                      </span>
                    </span>
                  </TabsTrigger>
                  <TabsTrigger
                    value="agent"
                    className={cn(
                      inviteTabTriggerClass,
                      "group",
                      // Soft violet tint in light mode, deep violet
                      // glow in dark — same rationale as the human tab
                      // above.
                      "data-[state=active]:border-b-violet-500 data-[state=active]:bg-violet-100 dark:data-[state=active]:bg-violet-950/20 data-[state=active]:text-foreground",
                    )}
                  >
                    <span className="inline-flex max-w-full items-center justify-center gap-2 sm:gap-2.5">
                      <Bot
                        className="block size-4 shrink-0 text-muted-foreground group-data-[state=active]:text-brand"
                        aria-hidden
                      />
                      <span className="inline-flex min-h-[1.25rem] max-w-full flex-wrap items-baseline justify-center gap-x-0 text-center whitespace-normal">
                        <span className="mt-1 font-semibold leading-none group-data-[state=active]:text-foreground">
                          Invite an agent&nbsp;
                        </span>
                        <span className="font-normal leading-none text-muted-foreground group-data-[state=active]:text-muted-foreground">
                          {" "}
                          · by token
                        </span>
                      </span>
                    </span>
                  </TabsTrigger>
                </TabsList>
              </CardHeader>
              <CardContent className="p-6 pt-6">
                <TabsContent value="person" className="mt-0 outline-none focus-visible:outline-none">
                  <InviteByEmailSection
                    datapackId={datapackId}
                    currentUserEmail={currentUser?.email}
                    sharedByRole={sharedByRole}
                    priorShareCount={priorShareCount}
                    onInviteSent={refreshShares}
                  />
                </TabsContent>
                <TabsContent value="agent" className="mt-0 outline-none focus-visible:outline-none">
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
            onMembersChanged={refreshShares}
          />
          <PendingInvitesSection
            datapackId={datapackId}
            invites={pendingInvites}
            loading={loading}
            onChanged={refreshShares}
          />
          <LinkSharingSection />
          <TransferOwnershipSection />
        </div>
      </main>
    </div>
  );
}
