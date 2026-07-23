import { Link } from "react-router-dom";
import { Crown, Users } from "lucide-react";
import { cn } from "@/lib/utils";
import { datapackMembersPath, datapackSharePath } from "@/app/routes/paths";
import type { CurrentUser, DatapackOutgoingMember } from "@/lib/api/mekoApi";
import { shareInviteRoleRowPresentation } from "@/features/datapacks/components/datapack-share/shareInviteRoles";
import {
  DATAPACK_SHARE_GRANT_OWNER,
  DATAPACK_SHARE_GRANT_VIEWER,
  type DatapackShareGrant,
  formatDatapackShareGrantCamelCase,
  isDatapackShareGrant,
} from "@/lib/datapackShareGrants";

export type DatapackMembersListStatus = "loading" | "ready" | "error";

/** Max rows shown in the tile preview. Anything past this is reached
 *  via the "View all" footer link. Matches the KnowledgeTile's preview
 *  cap so the two side-by-side tiles read consistently. */
const PREVIEW_LIMIT = 3;

function sortMembersForDisplay(members: DatapackOutgoingMember[]): DatapackOutgoingMember[] {
  const owners = members.filter((m) => m.is_owner);
  const rest = members.filter((m) => !m.is_owner);
  rest.sort((a, b) => (a.user_name || "").localeCompare(b.user_name || "", undefined, { sensitivity: "base" }));
  return [...owners, ...rest];
}

function outgoingMemberDisplayName(m: DatapackOutgoingMember): string {
  const n = m.user_name?.trim();
  if (n) return n;
  return m.user_email_id?.trim() || "Member";
}

function outgoingMemberInitials(m: DatapackOutgoingMember): string {
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

function resolvedMemberGrant(m: DatapackOutgoingMember): DatapackShareGrant {
  if (isDatapackShareGrant(m.grant)) return m.grant;
  return DATAPACK_SHARE_GRANT_VIEWER;
}

type Props = {
  datapackName: string;
  datapackId: string;
  currentUser: CurrentUser | null;
  members: DatapackOutgoingMember[];
  canShare: boolean;
  /** Drives loading / error empty copy so we do not flash a false empty state */
  membersListStatus?: DatapackMembersListStatus;
  className?: string;
};

/**
 * Members roster from GET member-list (owner first). Owners and maintainers see the full list;
 * viewers/contributors get an error state when the list API is forbidden.
 */
export function DatapackDetailMembersTile({
  datapackName,
  datapackId,
  currentUser,
  canShare,
  members,
  membersListStatus = "ready",
  className,
}: Props) {
  const ordered = sortMembersForDisplay(members);
  const preview = ordered.slice(0, PREVIEW_LIMIT);
  const count = membersListStatus === "loading" ? "…" : String(ordered.length);
  // Disabled when the roster fits within the preview (nothing more to
  // show on a dedicated page), or while we're still loading / errored
  // — mirrors the "+ Add Knowledge" disabled affordance on the sibling tile.
  const viewAllEnabled = membersListStatus === "ready" && ordered.length > PREVIEW_LIMIT;

  return (
    <aside
      className={cn(
        "flex h-full min-h-0 w-full flex-col rounded-xl border border-border bg-card/50 shadow-sm",
        className,
      )}
    >
      <div className="flex items-center justify-between gap-2 border-b border-border px-4 py-3">
        <div className="flex min-w-0 items-center gap-2">
          <Users className="h-4 w-4 shrink-0 text-brand" aria-hidden />
          <span className="truncate text-sm font-semibold text-foreground">Members</span>
          <span className="rounded-full bg-muted px-2 py-0.5 text-xs font-medium text-muted-foreground tabular-nums">
            {count}
          </span>
        </div>
        {canShare && <Link
          to={datapackSharePath(datapackId)}
          className="shrink-0 text-xs font-medium text-brand hover:underline"
        >
          Share →
        </Link>}
      </div>
      <ul className="flex flex-col divide-y divide-border px-2 py-1">
        {membersListStatus === "loading" ? (
          <li className="px-2 py-4 text-center text-xs text-muted-foreground">Loading members…</li>
        ) : membersListStatus === "error" ? (
          <li className="px-2 py-4 text-center text-xs leading-relaxed text-muted-foreground">
            Could not load members. You need owner or maintainer access to view the roster.
          </li>
        ) : ordered.length === 0 ? (
          <li className="px-2 py-4 text-center text-xs text-muted-foreground">No members to display.</li>
        ) : (
          preview.map((m) => {
            const primaryName = outgoingMemberDisplayName(m);
            const email = m.user_email_id?.trim() ?? "";
            const showEmailBelow = email && primaryName.toLowerCase() !== email.toLowerCase();
            const isYou = currentUser != null && m.user_id === currentUser.account_id;

            if (m.is_owner) {
              return (
                <li
                  key={`${m.user_id}-owner`}
                  className={cn(
                    "flex items-center gap-3 px-2 py-2.5",
                    isYou && "rounded-lg bg-muted/30 ring-1 ring-border/60",
                  )}
                >
                  <div
                    // Soft emerald avatar in light mode, saturated
                    // dark-emerald in dark mode. Original light-mode
                    // look was `bg-emerald-950/80` on white = gaudy
                    // solid-dark circle.
                    className="flex h-9 w-9 shrink-0 items-center justify-center rounded-full bg-emerald-100 dark:bg-emerald-950/80 text-sm font-semibold text-emerald-700 dark:text-emerald-300 ring-1 ring-emerald-700/50"
                    aria-hidden
                  >
                    {outgoingMemberInitials(m)}
                  </div>
                  <div className="min-w-0 flex-1">
                    <div className="flex min-w-0 items-center gap-2">
                      <div className="truncate text-sm font-medium text-foreground">{primaryName}</div>
                      {isYou ? (
                        <span className="shrink-0 rounded bg-muted px-1.5 py-0.5 text-[10px] font-medium uppercase tracking-wide text-muted-foreground">
                          You
                        </span>
                      ) : null}
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

            const grant = resolvedMemberGrant(m);
            const roleRow = shareInviteRoleRowPresentation(grant);
            const RoleIcon = roleRow?.Icon;

            return (
              <li
                key={m.user_id}
                className={cn(
                  "flex items-center gap-3 px-2 py-2.5",
                  isYou && "rounded-lg bg-muted/30 ring-1 ring-border/60",
                )}
              >
                <div
                  // Soft slate avatar in light mode, saturated
                  // dark-slate in dark. Text needs its own dark:
                  // variant since `text-slate-200` was dark-only.
                  className="flex h-9 w-9 shrink-0 items-center justify-center rounded-full bg-slate-100 dark:bg-slate-800/90 text-sm font-semibold text-slate-700 dark:text-slate-200 ring-1 ring-slate-600/40"
                  aria-hidden
                >
                  {outgoingMemberInitials(m)}
                </div>
                <div className="min-w-0 flex-1">
                  <div className="flex min-w-0 items-center gap-2">
                    <div className="truncate text-sm font-medium text-foreground">{primaryName}</div>
                    {isYou ? (
                      <span className="shrink-0 rounded bg-muted px-1.5 py-0.5 text-[10px] font-medium uppercase tracking-wide text-muted-foreground">
                        You
                      </span>
                    ) : null}
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
              </li>
            );
          })
        )}
      </ul>
      <div className="mt-auto border-t border-border">
        {viewAllEnabled ? (
          <Link
            to={datapackMembersPath(datapackId)}
            className="block w-full py-3 text-center text-sm font-normal text-foreground/90 transition-colors hover:text-foreground"
          >
            View all →
          </Link>
        ) : (
          <button
            type="button"
            disabled
            className="w-full cursor-not-allowed py-3 text-center text-sm font-normal text-muted-foreground/55 opacity-60"
          >
            View all →
          </button>
        )}
      </div>
    </aside>
  );
}
