// Full members roster page reached from the "View all" link on the
// DatapackDetailPage's Members tile. Owners + maintainers see everyone;
// viewers / contributors get the same forbidden state the tile shows.
//
// Mirrors the chrome (AppWorkspaceHeader → breadcrumbs → max-w-7xl main)
// of the other datapack sub-pages (SharedKnowledge, MemorySummary, …)
// so the navigation feel is consistent.

import { useEffect, useMemo, useState } from "react";
import { Crown, Loader2, Users } from "lucide-react";
import { toast } from "@/hooks/use-toast";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";
import { DatapackBreadcrumbs } from "@/features/datapacks/components/datapack-detail/DatapackBreadcrumbs";
import { datapackDetailPath, ROUTES } from "@/app/routes/paths";
import { cn } from "@/lib/utils";
import {
  getApiErrorMessage,
  getCurrentUser,
  listDatapackMembers,
  updateDatapackMemberGrant,
  type CurrentUser,
  type DatapackOutgoingMember,
} from "@/lib/api/mekoApi";
import { shareInviteRoleRowPresentation } from "@/features/datapacks/components/datapack-share/shareInviteRoles";
import {
  DATAPACK_SHARE_GRANT_CONTRIBUTOR,
  DATAPACK_SHARE_GRANT_MAINTAINER,
  DATAPACK_SHARE_GRANT_VIEWER,
  isDatapackGrantEligibleToShare,
  type DatapackShareGrant,
  formatDatapackShareGrantCamelCase,
  isDatapackShareGrant,
} from "@/lib/datapackShareGrants";

// Grants offered in the role-change dropdown. Owner is intentionally
// omitted — there's exactly one and the BE doesn't allow transferring it
// through this endpoint.
const SELECTABLE_GRANTS: DatapackShareGrant[] = [
  DATAPACK_SHARE_GRANT_VIEWER,
  DATAPACK_SHARE_GRANT_CONTRIBUTOR,
  DATAPACK_SHARE_GRANT_MAINTAINER,
];

type Props = {
  datapackName: string;
  datapackId?: string;
};

// --- Local helpers (kept private to this page; the tile has its own
// copies because the two surfaces have slightly different needs and a
// shared file would couple them more than it's worth). ---

function sortMembersForDisplay(members: DatapackOutgoingMember[]): DatapackOutgoingMember[] {
  const owners = members.filter((m) => m.is_owner);
  const rest = members.filter((m) => !m.is_owner);
  rest.sort((a, b) =>
    (a.user_name || "").localeCompare(b.user_name || "", undefined, { sensitivity: "base" }),
  );
  return [...owners, ...rest];
}

function memberDisplayName(m: DatapackOutgoingMember): string {
  const n = m.user_name?.trim();
  if (n) return n;
  return m.user_email_id?.trim() || "Member";
}

function memberInitials(m: DatapackOutgoingMember): string {
  const name = m.user_name?.trim() || "";
  const parts = name.split(/\s+/).filter(Boolean);
  if (parts.length >= 2) {
    return `${parts[0].charAt(0)}${parts[parts.length - 1].charAt(0)}`.toUpperCase();
  }
  if (parts.length === 1 && parts[0].length >= 2) {
    return parts[0].slice(0, 2).toUpperCase();
  }
  if (parts.length === 1) return parts[0].charAt(0).toUpperCase();
  const e = m.user_email_id?.trim().charAt(0);
  return e ? e.toUpperCase() : "?";
}

function resolvedGrant(m: DatapackOutgoingMember): DatapackShareGrant {
  if (isDatapackShareGrant(m.grant)) return m.grant;
  return DATAPACK_SHARE_GRANT_VIEWER;
}

export default function DatapackMembersListPage({ datapackName, datapackId }: Props) {
  const [members, setMembers] = useState<DatapackOutgoingMember[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [currentUser, setCurrentUser] = useState<CurrentUser | null>(null);
  // `user_id` of the row whose role is currently being PATCHed. Keeps
  // the select disabled mid-flight so a fast double-pick can't race
  // itself or skip the optimistic-then-reconcile reload below.
  const [updatingUserId, setUpdatingUserId] = useState<string | null>(null);

  // Caller identity drives the "You" pill on the row that represents
  // the caller themselves. Best-effort: failing to resolve just means
  // the pill is omitted everywhere.
  useEffect(() => {
    const ac = new AbortController();
    void getCurrentUser({ signal: ac.signal })
      .then((u) => {
        if (!ac.signal.aborted) setCurrentUser(u);
      })
      .catch(() => {
        // Silent — drop the "You" pill if we can't tell.
      });
    return () => ac.abort();
  }, []);

  useEffect(() => {
    const ac = new AbortController();
    setLoading(true);
    setError(null);
    void listDatapackMembers(datapackName, { signal: ac.signal, datapackId })
      .then((resp) => {
        if (ac.signal.aborted) return;
        setMembers(resp.members ?? []);
      })
      .catch((e) => {
        if (ac.signal.aborted) return;
        if ((e as { name?: string })?.name === "CanceledError") return;
        setError(getApiErrorMessage(e, "Could not load members."));
        setMembers([]);
      })
      .finally(() => {
        if (!ac.signal.aborted) setLoading(false);
      });
    return () => ac.abort();
  }, [datapackName, datapackId]);

  const ordered = sortMembersForDisplay(members);

  // Role-management is gated on the caller's own grant on this datapack:
  // only owner or maintainer can change other members' roles. The
  // `isDatapackGrantEligibleToShare` helper encodes that same predicate
  // (its docstring: "Owner and maintainer may open sharing / manage
  // members") — reused here so the rule stays in one place. The owner's
  // row carries `grant === 'maintainer'` (BE convention in
  // GetOutgoingMembers), so the helper returns true for owners too.
  const canManageRoles = useMemo(() => {
    if (!currentUser) return false;
    const me = members.find((m) => m.user_id === currentUser.account_id);
    if (!me) return false;
    return isDatapackGrantEligibleToShare({ grant: me.grant });
  }, [currentUser, members]);

  // Reload members from the BE — used to reconcile after a successful
  // PATCH and as a hard rollback on failure.
  const reloadMembers = async (signal?: AbortSignal) => {
    try {
      const resp = await listDatapackMembers(datapackName, { signal, datapackId });
      if (signal?.aborted) return;
      setMembers(resp.members ?? []);
    } catch {
      // Best-effort: leave whatever's on screen.
    }
  };

  const handleRoleChange = async (userID: string, newGrant: DatapackShareGrant) => {
    setUpdatingUserId(userID);
    // Snapshot the list BEFORE the optimistic flip so the catch path
    // can revert instantly, independent of the reload-from-server call
    // (which might itself fail or be slow). `updatingUserId` already
    // serialises role-changes to one in-flight, so restoring this
    // snapshot can't clobber a concurrent edit.
    const previousMembers = members;
    setMembers((prev) =>
      prev.map((m) => (m.user_id === userID ? { ...m, grant: newGrant } : m)),
    );
    try {
      await updateDatapackMemberGrant(datapackName, userID, { grant: newGrant }, { datapackId });
      toast({ title: "Role updated" });
      // Reconcile from the server in case the BE normalised the grant
      // or another change landed concurrently.
      await reloadMembers();
    } catch (err) {
      // Immediate, network-independent revert — UI shows truth on the
      // same tick as the toast.
      setMembers(previousMembers);
      toast({
        title: "Couldn't update role",
        description: getApiErrorMessage(err, "Role change failed."),
      });
      // Fire-and-forget reconcile so the canonical list still lands
      // when the network recovers, without blocking the revert.
      void reloadMembers();
    } finally {
      setUpdatingUserId(null);
    }
  };

  return (
    <div className="flex h-screen flex-1 flex-col overflow-y-auto bg-background text-foreground">
      <AppWorkspaceHeader />

      <main className="mx-auto flex w-full max-w-7xl flex-1 flex-col space-y-6 px-1 pb-12 pr-1 pt-6">
        <DatapackBreadcrumbs
          items={[
            { label: "Datapacks", href: ROUTES.datapacks },
            { label: datapackName, href: datapackDetailPath(datapackName) },
            { label: "Members" },
          ]}
        />

        <header className="flex items-center gap-2">
          <Users className="h-5 w-5 text-blue-400" aria-hidden />
          <h1 className="text-lg font-semibold text-foreground">Members</h1>
          <span className="rounded-full bg-muted px-2 py-0.5 text-xs font-medium text-muted-foreground tabular-nums">
            {loading ? "…" : ordered.length}
          </span>
        </header>

        {error ? (
          <div className="rounded-md border border-amber-500/40 bg-amber-500/10 px-4 py-3 text-sm text-amber-800 dark:text-amber-100">
            {error}
          </div>
        ) : null}

        {loading ? (
          <p className="rounded-lg border border-dashed border-border bg-muted/5 px-4 py-8 text-center text-sm text-muted-foreground">
            Loading members…
          </p>
        ) : ordered.length === 0 ? (
          <p className="rounded-lg border border-dashed border-border bg-muted/5 px-4 py-8 text-center text-sm text-muted-foreground">
            No members to display.
          </p>
        ) : (
          <ul className="space-y-2">
            {ordered.map((m) => {
              const isYou = currentUser != null && m.user_id === currentUser.account_id;
              return (
                <MemberRow
                  key={`${m.user_id}${m.is_owner ? "-owner" : ""}`}
                  member={m}
                  isYou={isYou}
                  // Show the editable dropdown only when (a) the caller
                  // is owner/maintainer, (b) the row isn't the owner row
                  // (owner can't be changed), and (c) the row isn't the
                  // caller themselves (no self-demotion).
                  canEditRole={canManageRoles && !m.is_owner && !isYou}
                  isUpdating={updatingUserId === m.user_id}
                  onRoleChange={handleRoleChange}
                />
              );
            })}
          </ul>
        )}
      </main>
    </div>
  );
}

function MemberRow({
  member,
  isYou,
  canEditRole,
  isUpdating,
  onRoleChange,
}: {
  member: DatapackOutgoingMember;
  isYou: boolean;
  canEditRole: boolean;
  isUpdating: boolean;
  onRoleChange: (userID: string, grant: DatapackShareGrant) => void;
}) {
  const primaryName = memberDisplayName(member);
  const email = member.user_email_id?.trim() ?? "";
  const showEmailBelow = email && primaryName.toLowerCase() !== email.toLowerCase();

  if (member.is_owner) {
    return (
      <li
        className={cn(
          "flex items-center gap-3 rounded-lg border border-border bg-card px-4 py-3",
          isYou && "ring-1 ring-border/60",
        )}
      >
        <Avatar tone="emerald" label={memberInitials(member)} />
        <div className="min-w-0 flex-1">
          <div className="flex min-w-0 items-center gap-2">
            <div className="truncate text-sm font-medium text-foreground">{primaryName}</div>
            {isYou ? <YouPill /> : null}
          </div>
          <div className="mt-0.5 flex items-center gap-1.5 text-xs text-muted-foreground">
            <Crown className="h-3 w-3 shrink-0 text-amber-500/90" aria-hidden />
            <span>Owner</span>
          </div>
          {showEmailBelow ? (
            <div className="mt-1 truncate text-[11px] text-muted-foreground/90" title={email}>
              {email}
            </div>
          ) : null}
        </div>
      </li>
    );
  }

  const grant = resolvedGrant(member);
  const roleRow = shareInviteRoleRowPresentation(grant);
  const RoleIcon = roleRow?.Icon;

  return (
    <li
      className={cn(
        "flex items-center gap-3 rounded-lg border border-border bg-card px-4 py-3",
        isYou && "ring-1 ring-border/60",
      )}
    >
      <Avatar tone="slate" label={memberInitials(member)} />
      <div className="min-w-0 flex-1">
        <div className="flex min-w-0 items-center gap-2">
          <div className="truncate text-sm font-medium text-foreground">{primaryName}</div>
          {isYou ? <YouPill /> : null}
        </div>
        <div className="mt-0.5 flex items-center gap-1.5 text-xs text-muted-foreground">
          {roleRow && RoleIcon ? (
            <>
              <RoleIcon className={cn("h-3 w-3 shrink-0", roleRow.iconClassName)} aria-hidden />
              <span>{roleRow.label}</span>
            </>
          ) : (
            <span>{formatDatapackShareGrantCamelCase(grant)}</span>
          )}
        </div>
        {showEmailBelow ? (
          <div className="mt-1 truncate text-[11px] text-muted-foreground/90" title={email}>
            {email}
          </div>
        ) : null}
      </div>
      {canEditRole ? (
        <div className="flex shrink-0 items-center gap-2">
          <label className="sr-only" htmlFor={`role-${member.user_id}`}>
            Change role for {primaryName}
          </label>
          <select
            id={`role-${member.user_id}`}
            value={grant}
            onChange={(e) => {
              const next = e.target.value;
              if (next !== grant && isDatapackShareGrant(next)) {
                onRoleChange(member.user_id, next);
              }
            }}
            disabled={isUpdating}
            className="rounded-md border border-border bg-background px-2 py-1 text-xs text-foreground focus:outline-none focus:ring-1 focus:ring-ring disabled:opacity-50"
          >
            {SELECTABLE_GRANTS.map((g) => (
              <option key={g} value={g}>
                {formatDatapackShareGrantCamelCase(g)}
              </option>
            ))}
          </select>
          {isUpdating ? (
            <Loader2 className="h-3.5 w-3.5 animate-spin text-muted-foreground" aria-hidden />
          ) : null}
        </div>
      ) : null}
    </li>
  );
}

/** Round avatar with role-themed background. Two tones mirror the
 *  ones on the tile rows: emerald for owners, slate for everyone else. */
function Avatar({ tone, label }: { tone: "emerald" | "slate"; label: string }) {
  const cls =
    tone === "emerald"
      ? "bg-emerald-100 dark:bg-emerald-950/80 text-emerald-700 dark:text-emerald-300 ring-emerald-700/50"
      : "bg-slate-100 dark:bg-slate-800/90 text-slate-700 dark:text-slate-200 ring-slate-600/40";
  return (
    <div
      className={cn(
        "flex h-9 w-9 shrink-0 items-center justify-center rounded-full text-sm font-semibold ring-1",
        cls,
      )}
      aria-hidden
    >
      {label}
    </div>
  );
}

function YouPill() {
  return (
    <span className="shrink-0 rounded bg-muted px-1.5 py-0.5 text-[10px] font-medium uppercase tracking-wide text-muted-foreground">
      You
    </span>
  );
}
