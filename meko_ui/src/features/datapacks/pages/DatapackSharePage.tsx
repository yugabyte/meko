import { Share2 } from "lucide-react";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";
import { DatapackBreadcrumbs } from "@/features/datapacks/components/datapack-detail/DatapackBreadcrumbs";
import { InviteByEmailSection } from "@/features/datapacks/components/datapack-share/InviteByEmailSection";
import { LinkSharingSection } from "@/features/datapacks/components/datapack-share/LinkSharingSection";
import { MembersSection } from "@/features/datapacks/components/datapack-share/MembersSection";
import { TransferOwnershipSection } from "@/features/datapacks/components/datapack-share/TransferOwnershipSection";
import { datapackDetailPath, ROUTES } from "@/app/routes/paths";
import type { CurrentUser } from "@/lib/mekoApi";

type Props = {
  datapackName: string;
  currentUser: CurrentUser | null;
};

export default function DatapackSharePage({ datapackName, currentUser }: Props) {
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

        <div className="space-y-6">
          <InviteByEmailSection />
          <MembersSection currentUser={currentUser} />
          <LinkSharingSection />
          <TransferOwnershipSection />
        </div>
      </main>
    </div>
  );
}
