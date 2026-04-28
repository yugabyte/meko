// Owner-facing page to invite collaborators, review pending invites, and manage member grants for a datapack.
// Loads members + pending invites once via useDatapackOutgoingShares; child actions use silent refetch to stay fresh.
import { Share2, AlertCircle } from "lucide-react";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";
import { DatapackBreadcrumbs } from "@/features/datapacks/components/datapack-detail/DatapackBreadcrumbs";
import { InviteByEmailSection } from "@/features/datapacks/components/datapack-share/InviteByEmailSection";
import { LinkSharingSection } from "@/features/datapacks/components/datapack-share/LinkSharingSection";
import { MembersSection } from "@/features/datapacks/components/datapack-share/MembersSection";
import { PendingInvitesSection } from "@/features/datapacks/components/datapack-share/PendingInvitesSection";
import { TransferOwnershipSection } from "@/features/datapacks/components/datapack-share/TransferOwnershipSection";
import { useDatapackOutgoingShares } from "@/features/datapacks/components/datapack-share/useDatapackOutgoingShares";
import { datapackDetailPath, ROUTES } from "@/app/routes/paths";
import type { CurrentUser } from "@/lib/mekoApi";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";

type Props = {
  datapackName: string;
  currentUser: CurrentUser | null;
};

export default function DatapackSharePage({ datapackName, currentUser }: Props) {
  const { data, loading, error, refetch } = useDatapackOutgoingShares(datapackName);
  const members = data?.members ?? [];
  const pendingInvites = data?.pending_invites ?? [];

  /** After invites or member mutations, refresh lists without toggling the global loading skeleton. */
  const refreshShares = () => void refetch({ silent: true });

  return (
    <div className="flex-1 h-screen overflow-y-auto bg-background text-foreground flex flex-col">
      <AppWorkspaceHeader />

      <main className="flex-1 p-8 max-w-6xl mx-auto w-full space-y-8 pb-16">
        <DatapackBreadcrumbs
          items={[
            { label: "Datapacks", href: ROUTES.datapacks },
            { label: datapackName, href: datapackDetailPath(datapackName) },
            { label: "Share" },
          ]}
        />

        <div className="space-y-2">
          <div className="flex flex-wrap items-center gap-3">
            <Share2 className="h-8 w-8 shrink-0 text-blue-400" aria-hidden />
            <h1 className="text-2xl sm:text-3xl font-semibold tracking-tight text-foreground truncate">
              Share {datapackName}
            </h1>
          </div>
          <p className="text-sm text-muted-foreground max-w-2xl leading-relaxed">
            Give humans and agents access to this datapack&apos;s knowledge and memories.
          </p>
        </div>

        {error ? (
          <Alert variant="destructive">
            <AlertCircle className="h-4 w-4" aria-hidden />
            <AlertTitle>Could not load sharing</AlertTitle>
            <AlertDescription className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
              <span>{error}</span>
              <Button type="button" size="sm" variant="outline" className="shrink-0 border-destructive/40 bg-transparent" onClick={() => void refetch()}>
                Retry
              </Button>
            </AlertDescription>
          </Alert>
        ) : null}

        <div className="space-y-6">
          <InviteByEmailSection datapackName={datapackName} onInviteSent={refreshShares} />
          <PendingInvitesSection
            datapackName={datapackName}
            invites={pendingInvites}
            loading={loading}
            onChanged={refreshShares}
          />
          <MembersSection
            currentUser={currentUser}
            datapackName={datapackName}
            members={members}
            loading={loading}
            onMembersChanged={refreshShares}
          />
          <LinkSharingSection />
          <TransferOwnershipSection />
        </div>
      </main>
    </div>
  );
}
