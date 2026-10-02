// Renders the vertical stack of MemberRows plus loading + empty
// states. Kept split from MembersSection so the section can compose
// toolbar + list without duplicating the loading skeleton across
// Humans / All tabs.
import type { CurrentUser, DatapackOutgoingMember } from "@/lib/api/mekoApi";
import { cn } from "@/lib/utils";
import { MemberRow } from "@/features/datapacks/components/datapack-share/MemberRow";

export type MembersListProps = {
  members: DatapackOutgoingMember[];
  datapackName: string;
  datapackId: string;
  currentUser: CurrentUser | null;
  canManage: boolean;
  loading: boolean;
  onMembersChanged: () => void | Promise<void>;
  /** Copy for the empty state — differs when the roster is non-empty but filtered to nothing. */
  emptyMessage?: string;
};

export function MembersList({
  members,
  datapackName,
  datapackId,
  currentUser,
  canManage,
  loading,
  onMembersChanged,
  emptyMessage = "No members yet.",
}: MembersListProps) {
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
        {emptyMessage}
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
