// Members card on the datapack share page — tab strip (Humans / Agents /
// All), a search + role-filter + CSV-export toolbar, and the roster
// list. The three inline widgets that used to live here (`MemberRow`,
// `MembersToolbar`, `MembersList`) are now their own files; this file
// composes them and owns filter state so tab switches don't drop it.
import { useMemo, useState } from "react";
import { Users } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader } from "@/components/ui/card";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import type { CurrentUser, DatapackOutgoingMember } from "@/lib/api/mekoApi";
import { MembersList } from "@/features/datapacks/components/datapack-share/MembersList";
import { MembersToolbar } from "@/features/datapacks/components/datapack-share/MembersToolbar";
import { isCurrentUserOwner } from "@/features/datapacks/components/datapack-share/MemberRow";
import { ShareComingSoonPlaceholder } from "@/features/datapacks/components/datapack-share/ShareComingSoonPlaceholder";
import {
  EMPTY_SHARE_MEMBER_FILTER,
  filterShareMembers,
  isShareMemberFilterActive,
  type ShareMemberFilter,
} from "@/features/datapacks/components/datapack-share/shareMemberFilter";
import { cn } from "@/lib/utils";

type Props = {
  currentUser: CurrentUser | null;
  datapackName: string;
  datapackId: string;
  members: DatapackOutgoingMember[];
  loading: boolean;
  onMembersChanged: () => void | Promise<void>;
};

// Tab pill treatment shared by the three tabs so the card header stays
// uncluttered. Neutral text by default, foreground + slight weight on
// the active tab. No background swap — the section card already
// provides the surface.
const membersTabTriggerClass = cn(
  "rounded-md px-2.5 py-1.5 text-xs font-medium text-muted-foreground shadow-none transition-colors",
  "data-[state=active]:bg-transparent data-[state=active]:text-foreground data-[state=active]:shadow-none data-[state=active]:font-semibold",
  "data-[state=inactive]:hover:text-foreground/90",
);

export function MembersSection({
  currentUser,
  datapackName,
  datapackId,
  members,
  loading,
  onMembersChanged,
}: Props) {
  const humanCount = members.length;
  const canManage = isCurrentUserOwner(members, currentUser);
  const [filter, setFilter] = useState<ShareMemberFilter>(EMPTY_SHARE_MEMBER_FILTER);
  const visibleMembers = useMemo(() => filterShareMembers(members, filter), [members, filter]);
  const filtering = isShareMemberFilterActive(filter);

  const listProps = {
    members: visibleMembers,
    datapackName,
    datapackId,
    currentUser,
    canManage,
    loading,
    onMembersChanged,
    emptyMessage: filtering ? "No members match your search." : "No members yet.",
  };
  const toolbar = (
    <MembersToolbar
      filter={filter}
      onFilterChange={setFilter}
      visibleCount={visibleMembers.length}
      totalCount={humanCount}
    />
  );

  return (
    <Tabs defaultValue="humans" className="w-full">
      <Card className="border-border bg-card/50 shadow-sm">
        <CardHeader className="flex flex-row flex-wrap items-center justify-between gap-3 space-y-0 pb-4">
          <div className="flex min-w-0 flex-wrap items-center gap-2">
            <Users className="h-5 w-5 shrink-0 text-brand" aria-hidden />
            <h2 className="text-base font-semibold tracking-tight text-foreground">Members</h2>
            <Badge
              variant="brand"
              className="shrink-0 px-2 py-0.5 text-xs font-semibold tabular-nums"
            >
              {humanCount}
            </Badge>
          </div>
          <div className="flex flex-wrap items-center justify-end gap-2 sm:gap-3">
            <TabsList className="flex h-auto flex-wrap justify-end gap-1 bg-transparent p-0">
              <TabsTrigger value="humans" className={membersTabTriggerClass}>
                Humans
              </TabsTrigger>
              <TabsTrigger value="agents" className={membersTabTriggerClass}>
                Agents
              </TabsTrigger>
              <TabsTrigger value="all" className={membersTabTriggerClass}>
                All
              </TabsTrigger>
            </TabsList>
          </div>
        </CardHeader>
        <CardContent className="pt-0">
          <TabsContent value="humans" className="mt-0 space-y-3 outline-none">
            {toolbar}
            <MembersList {...listProps} />
          </TabsContent>
          <TabsContent value="agents" className="mt-0 outline-none">
            <ShareComingSoonPlaceholder aria-label="Agent members — coming soon" />
          </TabsContent>
          <TabsContent value="all" className="mt-0 space-y-3 outline-none">
            {toolbar}
            <MembersList {...listProps} />
          </TabsContent>
        </CardContent>
      </Card>
    </Tabs>
  );
}
