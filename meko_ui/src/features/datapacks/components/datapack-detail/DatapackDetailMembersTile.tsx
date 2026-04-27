import { Link } from "react-router-dom";
import { Crown, Users } from "lucide-react";
import { cn } from "@/lib/utils";
import { datapackSharePath } from "@/app/routes/paths";
import type { CurrentUser } from "@/lib/mekoApi";

function memberDisplayName(user: CurrentUser): string {
  const full = `${user.first_name ?? ""} ${user.last_name ?? ""}`.trim();
  if (full) return full;
  return user.email ?? "You";
}

function memberInitials(user: CurrentUser): string {
  const f = user.first_name?.trim().charAt(0) ?? "";
  const l = user.last_name?.trim().charAt(0) ?? "";
  if (f && l) return `${f}${l}`.toUpperCase();
  if (f) return f.toUpperCase();
  const e = user.email?.trim().charAt(0);
  return e ? e.toUpperCase() : "?";
}

type Props = {
  datapackName: string;
  currentUser: CurrentUser | null;
  className?: string;
};

/**
 * Compact members summary beside the datapack detail tabs (signed-in user only until roster API exists).
 */
export function DatapackDetailMembersTile({ datapackName, currentUser, className }: Props) {
  const count = currentUser ? 1 : 0;
  const primaryName = currentUser ? memberDisplayName(currentUser) : "";
  const showEmailBelow =
    currentUser?.email &&
    primaryName.trim().toLowerCase() !== currentUser.email.trim().toLowerCase();

  return (
    <aside
      className={cn(
        "flex h-fit min-h-0 w-full flex-col self-start rounded-xl border border-border bg-card/50 shadow-sm",
        className,
      )}
    >
      <div className="flex items-center justify-between gap-2 border-b border-border px-4 py-3">
        <div className="flex min-w-0 items-center gap-2">
          <Users className="h-4 w-4 shrink-0 text-blue-400" aria-hidden />
          <span className="truncate text-sm font-semibold text-foreground">Members</span>
          <span className="rounded-full bg-muted px-2 py-0.5 text-xs font-medium text-muted-foreground">
            {count}
          </span>
        </div>
        <Link
          to={datapackSharePath(datapackName)}
          className="shrink-0 text-xs font-medium text-blue-400 hover:underline"
        >
          Share →
        </Link>
      </div>
      <ul className="flex flex-col divide-y divide-border px-2 py-1">
        {currentUser ? (
          <li className="flex items-center gap-3 px-2 py-2.5">
            <div
              className="flex h-9 w-9 shrink-0 items-center justify-center rounded-full bg-emerald-950/80 text-sm font-semibold text-emerald-300 ring-1 ring-emerald-700/50"
              aria-hidden
            >
              {memberInitials(currentUser)}
            </div>
            <div className="min-w-0 flex-1">
              <div className="truncate text-sm font-medium text-foreground">{primaryName}</div>
              <div className="mt-0.5 flex items-center gap-1 text-xs text-muted-foreground">
                <Crown className="h-3 w-3 shrink-0 text-amber-500/90" aria-hidden />
                <span>Owner</span>
              </div>
              {showEmailBelow && currentUser.email ? (
                <div className="mt-1 truncate text-[11px] text-muted-foreground/90" title={currentUser.email}>
                  {currentUser.email}
                </div>
              ) : null}
            </div>
          </li>
        ) : (
          <li className="px-2 py-4 text-center text-xs text-muted-foreground">Loading member…</li>
        )}
      </ul>
    </aside>
  );
}
