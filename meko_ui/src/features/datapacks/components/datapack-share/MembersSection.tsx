// Renders human members from GET member-list: list layout, owner row shows crown + “Owner”, grant Select when editable.
// Agents tab is a coming-soon placeholder; Humans and All show the same member list (no activity metadata).
import { useState } from "react";
import { Crown, Users } from "lucide-react";
import { Card, CardContent, CardHeader } from "@/components/ui/card";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import type { CurrentUser, DatapackOutgoingMember, DatapackShareGrant } from "@/lib/mekoApi";
import {
  getApiErrorMessage,
  removeDatapackMemberShare,
  updateDatapackMemberGrant,
} from "@/lib/mekoApi";
import { ShareComingSoonPlaceholder } from "@/features/datapacks/components/datapack-share/ShareComingSoonPlaceholder";
import { SHARE_INVITE_ROLES, shareGrantLabel } from "@/features/datapacks/components/datapack-share/shareInviteRoles";
import { toast } from "@/hooks/use-toast";
import { cn } from "@/lib/utils";

type Props = {
  currentUser: CurrentUser | null;
  datapackName: string;
  datapackId?: string;
  members: DatapackOutgoingMember[];
  loading: boolean;
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

function MemberRow({
  member,
  datapackName,
  datapackId,
  currentUserEmail,
  canManage,
  onMembersChanged,
}: {
  member: DatapackOutgoingMember;
  datapackName: string;
  datapackId?: string;
  currentUserEmail: string | null | undefined;
  canManage: boolean;
  onMembersChanged: () => void | Promise<void>;
}) {
  const [grantBusy, setGrantBusy] = useState(false);
  const [removeBusy, setRemoveBusy] = useState(false);
  const isYou = emailsMatch(member.user_email_id, currentUserEmail);
  // Owners never change grant here; non-owners see read-only grant unless the signed-in user owns the datapack.
  const grantReadOnly = member.is_owner || !canManage;

  const handleGrantChange = async (next: DatapackShareGrant) => {
    if (next === member.grant || grantReadOnly) return;
    setGrantBusy(true);
    try {
      await updateDatapackMemberGrant(datapackName, member.user_id, { grant: next }, { datapackId });
      toast({ title: "Role updated" });
      await onMembersChanged();
    } catch (err) {
      toast({
        variant: "destructive",
        title: "Could not update role",
        description: getApiErrorMessage(err, "Something went wrong. Try again."),
      });
    } finally {
      setGrantBusy(false);
    }
  };

  const handleRemove = async () => {
    if (!canManage || member.is_owner) return;
    setRemoveBusy(true);
    try {
      await removeDatapackMemberShare(datapackName, member.user_id, { datapackId });
      toast({ title: "Access removed" });
      await onMembersChanged();
    } catch (err) {
      toast({
        variant: "destructive",
        title: "Could not remove member",
        description: getApiErrorMessage(err, "Something went wrong. Try again."),
      });
    } finally {
      setRemoveBusy(false);
    }
  };

  const roleControl = member.is_owner ? (
    <div className="flex h-9 items-center gap-2 pl-2 mr-4 text-sm font-medium text-foreground" title="Datapack owner">
      <Crown className="h-4 w-4 shrink-0 text-amber-400" aria-hidden />
      <span>Owner</span>
    </div>
  ) : grantReadOnly ? (
    <div className="flex h-9 min-w-[148px] items-center rounded-md border border-border bg-muted/30 px-3 text-xs text-muted-foreground">
      {shareGrantLabel(member.grant)}
    </div>
  ) : (
    <div
      className="flex h-9 min-w-[80px] items-center rounded-md border border-border bg-muted/30 px-3 text-xs text-muted-foreground">
      {shareGrantLabel(member.grant as DatapackShareGrant)}
    </div>
    /* TODO: We will addd back dropdown for enabling owner to change permissions */
    // <Select value={member.grant} onValueChange={(v) => void handleGrantChange(v as DatapackShareGrant)} disabled={grantBusy}>
    //   <SelectTrigger className="h-9 w-[min(100%,11rem)] min-w-[148px] text-xs bg-background/80 sm:w-44">
    //     <SelectValue />
    //   </SelectTrigger>
    //   <SelectContent>
    //     {SHARE_INVITE_ROLES.map((r) => (
    //       <SelectItem key={r.id} value={r.id}>
    //         {r.label}
    //       </SelectItem>
    //     ))}
    //   </SelectContent>
    // </Select>
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
            <span className="truncate text-sm font-medium text-foreground">{member.user_name || member.user_email_id}</span>
            {isYou ? (
              <Badge
                variant="outline"
                className="shrink-0 border-blue-500/35 bg-blue-500/10 px-2 py-0 text-[10px] font-semibold uppercase tracking-wide text-blue-200"
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
    </div>
  );
}

function MembersList({
  members,
  datapackName,
  datapackId,
  currentUser,
  canManage,
  loading,
  onMembersChanged,
}: {
  members: DatapackOutgoingMember[];
  datapackName: string;
  datapackId?: string;
  currentUser: CurrentUser | null;
  canManage: boolean;
  loading: boolean;
  onMembersChanged: () => void | Promise<void>;
}) {
  if (loading && members.length === 0) {
    return (
      <div className="space-y-2">
        <div className="h-[72px] animate-pulse rounded-lg border border-border bg-muted/20" />
        <div className="h-[72px] animate-pulse rounded-lg border border-border bg-muted/20" />
      </div>
    );
  }

  if (members.length === 0) {
    return (
      <p className="rounded-lg border border-dashed border-border px-4 py-8 text-center text-sm text-muted-foreground">
        No members yet.
      </p>
    );
  }

  return (
    <div
      className={cn(
        "divide-y divide-border overflow-hidden rounded-lg border border-border bg-background/50",
        loading && "opacity-70",
      )}
    >
      {members.map((m) => (
        <MemberRow
          key={m.user_id}
          member={m}
          datapackName={datapackName}
          datapackId={datapackId}
          currentUserEmail={currentUser?.email}
          canManage={canManage}
          onMembersChanged={onMembersChanged}
        />
      ))}
    </div>
  );
}

const membersTabTriggerClass = cn(
  "rounded-md px-2.5 py-1.5 text-xs font-medium text-muted-foreground shadow-none transition-colors",
  "data-[state=active]:bg-transparent data-[state=active]:text-foreground data-[state=active]:shadow-none data-[state=active]:font-semibold",
  "data-[state=inactive]:hover:text-foreground/90",
);

export function MembersSection({ currentUser, datapackName, datapackId, members, loading, onMembersChanged }: Props) {
  const humanCount = members.length;
  // Only the member row marked is_owner whose email matches the session may edit others’ roles or remove them.
  const canManage = members.some((m) => m.is_owner && emailsMatch(m.user_email_id, currentUser?.email));

  return (
    <Tabs defaultValue="humans" className="w-full">
      <Card className="border-border bg-card/50 shadow-sm">
        <CardHeader className="flex flex-row flex-wrap items-center justify-between gap-3 space-y-0 pb-4">
          <div className="flex min-w-0 flex-wrap items-center gap-2">
            <Users className="h-5 w-5 shrink-0 text-emerald-400" aria-hidden />
            <h2 className="text-base font-semibold tracking-tight text-foreground">Members</h2>
            <Badge
              variant="outline"
              className="shrink-0 border-emerald-500/45 bg-emerald-500/15 px-2 py-0.5 text-xs font-semibold tabular-nums text-emerald-200"
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
          <TabsContent value="humans" className="mt-0 space-y-2 outline-none">
            <MembersList
              members={members}
              datapackName={datapackName}
              datapackId={datapackId}
              currentUser={currentUser}
              canManage={canManage}
              loading={loading}
              onMembersChanged={onMembersChanged}
            />
          </TabsContent>
          <TabsContent value="agents" className="mt-0 outline-none">
            <ShareComingSoonPlaceholder aria-label="Agent members — coming soon" />
          </TabsContent>
          <TabsContent value="all" className="mt-0 space-y-2 outline-none">
            <MembersList
              members={members}
              datapackName={datapackName}
              datapackId={datapackId}
              currentUser={currentUser}
              canManage={canManage}
              loading={loading}
              onMembersChanged={onMembersChanged}
            />
          </TabsContent>
        </CardContent>
      </Card>
    </Tabs>
  );
}
