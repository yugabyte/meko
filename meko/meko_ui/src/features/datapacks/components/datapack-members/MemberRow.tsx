// One row of the full members roster: name, role, and for owners and
// maintainers the role picker and Remove action on everyone else's row.
import { Crown, Loader2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import type { DatapackOutgoingMember } from "@/lib/api/mekoApi";
import { shareInviteRoleRowPresentation } from "@/features/datapacks/components/datapack-share/shareInviteRoles";
import {
  DATAPACK_SHARE_GRANT_CONTRIBUTOR,
  DATAPACK_SHARE_GRANT_MAINTAINER,
  DATAPACK_SHARE_GRANT_VIEWER,
  formatDatapackShareGrantCamelCase,
  isDatapackShareGrant,
  type DatapackShareGrant,
} from "@/lib/datapackShareGrants";
import { memberDisplayName, memberInitials, resolvedGrant } from "./memberDisplay";

// Grants offered in the role-change dropdown. Owner is intentionally
// omitted — there's exactly one and the BE doesn't allow transferring it
// through this endpoint.
const SELECTABLE_GRANTS: DatapackShareGrant[] = [
  DATAPACK_SHARE_GRANT_VIEWER,
  DATAPACK_SHARE_GRANT_CONTRIBUTOR,
  DATAPACK_SHARE_GRANT_MAINTAINER,
];

export function MemberRow({
  member,
  isYou,
  canEditRole,
  isUpdating,
  actionsLocked,
  onRoleChange,
  onRemove,
}: {
  member: DatapackOutgoingMember;
  isYou: boolean;
  /** Also gates Remove: the same people may change a role or take it away. */
  canEditRole: boolean;
  isUpdating: boolean;
  /** Another row's role change or removal is in flight; one change at a time. */
  actionsLocked: boolean;
  onRoleChange: (userID: string, grant: DatapackShareGrant) => void;
  onRemove: (member: DatapackOutgoingMember) => void;
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
          {member.source === "link" ? (
            <span className="shrink-0 rounded bg-muted px-1.5 py-0.5 text-[10px] font-medium text-muted-foreground">
              joined via link
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
            disabled={isUpdating || actionsLocked}
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
          <Button
            type="button"
            variant="ghost"
            size="sm"
            className="h-7 px-2 text-xs text-muted-foreground hover:text-destructive"
            disabled={isUpdating || actionsLocked}
            onClick={() => onRemove(member)}
          >
            Remove
          </Button>
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
