// Renders human members from outgoing-shares with role editing and removal when the viewer is the owner.
// Agents tab is a coming-soon placeholder; Humans and All currently show the same member list from the API.
import { useState } from "react";
import { Users } from "lucide-react";
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
import { DatapackShareSectionCard } from "@/features/datapacks/components/datapack-share/DatapackShareSectionCard";
import { ShareComingSoonPlaceholder } from "@/features/datapacks/components/datapack-share/ShareComingSoonPlaceholder";
import { SHARE_INVITE_ROLES, shareGrantLabel } from "@/features/datapacks/components/datapack-share/shareInviteRoles";
import { toast } from "@/hooks/use-toast";
import { cn } from "@/lib/utils";

type Props = {
  currentUser: CurrentUser | null;
  datapackName: string;
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
  currentUserEmail,
  canManage,
  onMembersChanged,
}: {
  member: DatapackOutgoingMember;
  datapackName: string;
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
      await updateDatapackMemberGrant(datapackName, member.user_id, { grant: next });
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
      await removeDatapackMemberShare(datapackName, member.user_id);
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

  return (
    <div className="flex items-center gap-3 rounded-lg border border-border bg-background/50 px-3 py-3">
      <div
        className="flex h-9 w-9 shrink-0 items-center justify-center rounded-full bg-muted text-sm font-semibold text-foreground"
        aria-hidden
      >
        {memberInitial(member)}
      </div>
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-center gap-2">
          <span className="truncate text-sm font-medium text-foreground">{member.user_name || member.user_email_id}</span>
          {member.is_owner ? (
            <Badge variant="secondary" className="shrink-0 text-[10px] font-semibold uppercase tracking-wide">
              Owner
            </Badge>
          ) : null}
          {isYou ? <span className="text-xs font-normal text-muted-foreground">(you)</span> : null}
        </div>
        <div className="truncate text-xs text-muted-foreground">{member.user_email_id}</div>
      </div>
      <div className="flex shrink-0 items-center gap-2">
        {grantReadOnly ? (
          <div className="flex h-8 min-w-[140px] items-center rounded-md border border-border bg-muted/30 px-2 text-xs text-muted-foreground">
            {shareGrantLabel(member.grant)}
          </div>
        ) : (
          <Select
            value={member.grant}
            onValueChange={(v) => void handleGrantChange(v as DatapackShareGrant)}
            disabled={grantBusy}
          >
            <SelectTrigger className="h-8 w-[140px] text-xs bg-background/80">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {SHARE_INVITE_ROLES.map((r) => (
                <SelectItem key={r.id} value={r.id}>
                  {r.label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        )}
        {member.is_owner ? (
          <Button type="button" variant="ghost" size="icon" className="h-8 w-8 text-muted-foreground" disabled aria-label="Owner cannot be removed">
            <span className="text-lg leading-none" aria-hidden>
              ×
            </span>
          </Button>
        ) : (
          <Button
            type="button"
            variant="ghost"
            size="icon"
            className="h-8 w-8 text-muted-foreground hover:text-destructive"
            disabled={!canManage || removeBusy}
            aria-label={`Remove ${member.user_name || "member"}`}
            onClick={() => void handleRemove()}
          >
            <span className="text-lg leading-none" aria-hidden>
              {removeBusy ? "…" : "×"}
            </span>
          </Button>
        )}
      </div>
    </div>
  );
}

function MembersList({
  members,
  datapackName,
  currentUser,
  canManage,
  loading,
  onMembersChanged,
}: {
  members: DatapackOutgoingMember[];
  datapackName: string;
  currentUser: CurrentUser | null;
  canManage: boolean;
  loading: boolean;
  onMembersChanged: () => void | Promise<void>;
}) {
  if (loading && members.length === 0) {
    return (
      <div className="space-y-2">
        <div className="h-[68px] animate-pulse rounded-lg border border-border bg-muted/20" />
        <div className="h-[68px] animate-pulse rounded-lg border border-border bg-muted/20" />
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
    <div className={cn("space-y-2", loading && "opacity-70")}>
      {members.map((m) => (
        <MemberRow
          key={m.user_id}
          member={m}
          datapackName={datapackName}
          currentUserEmail={currentUser?.email}
          canManage={canManage}
          onMembersChanged={onMembersChanged}
        />
      ))}
    </div>
  );
}

export function MembersSection({ currentUser, datapackName, members, loading, onMembersChanged }: Props) {
  const humanCount = members.length;
  // Only the member row marked is_owner whose email matches the session may edit others’ roles or remove them.
  const canManage = members.some((m) => m.is_owner && emailsMatch(m.user_email_id, currentUser?.email));

  return (
    <DatapackShareSectionCard
      title="Members"
      icon={<Users className="text-emerald-400" aria-hidden />}
      trailing={
        <span className="rounded-full bg-muted px-2 py-0.5 text-xs font-medium text-muted-foreground">
          {humanCount}
        </span>
      }
    >
      <Tabs defaultValue="humans" className="w-full">
        <TabsList className="mb-4 h-9 w-full justify-start bg-muted/50 p-1 sm:w-auto">
          <TabsTrigger value="humans" className="text-xs sm:text-sm">
            Humans
            <span className="ml-1.5 rounded-md bg-background/80 px-1.5 py-0 text-[10px] text-muted-foreground tabular-nums">
              {humanCount}
            </span>
          </TabsTrigger>
          <TabsTrigger value="agents" className="text-xs sm:text-sm">
            Agents
          </TabsTrigger>
          <TabsTrigger value="all" className="text-xs sm:text-sm">
            All
            <span className="ml-1.5 rounded-md bg-background/80 px-1.5 py-0 text-[10px] text-muted-foreground tabular-nums">
              {humanCount}
            </span>
          </TabsTrigger>
        </TabsList>
        <TabsContent value="humans" className="mt-0 space-y-2">
          <MembersList
            members={members}
            datapackName={datapackName}
            currentUser={currentUser}
            canManage={canManage}
            loading={loading}
            onMembersChanged={onMembersChanged}
          />
        </TabsContent>
        <TabsContent value="agents" className="mt-0">
          <ShareComingSoonPlaceholder aria-label="Agent members — coming soon" />
        </TabsContent>
        <TabsContent value="all" className="mt-0 space-y-2">
          <MembersList
            members={members}
            datapackName={datapackName}
            currentUser={currentUser}
            canManage={canManage}
            loading={loading}
            onMembersChanged={onMembersChanged}
          />
        </TabsContent>
      </Tabs>
    </DatapackShareSectionCard>
  );
}
