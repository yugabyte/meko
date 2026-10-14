// Display helpers for the full members roster: ordering, names, initials, and
// the grant a row shows. The members tile keeps its own copies on purpose.

import type { DatapackOutgoingMember } from "@/lib/api/mekoApi";
import {
  DATAPACK_SHARE_GRANT_VIEWER,
  isDatapackShareGrant,
  type DatapackShareGrant,
} from "@/lib/datapackShareGrants";

export function sortMembersForDisplay(members: DatapackOutgoingMember[]): DatapackOutgoingMember[] {
  const owners = members.filter((m) => m.is_owner);
  const rest = members.filter((m) => !m.is_owner);
  rest.sort((a, b) =>
    (a.user_name || "").localeCompare(b.user_name || "", undefined, { sensitivity: "base" }),
  );
  return [...owners, ...rest];
}

export function memberDisplayName(m: DatapackOutgoingMember): string {
  const n = m.user_name?.trim();
  if (n) return n;
  return m.user_email_id?.trim() || "Member";
}

export function memberInitials(m: DatapackOutgoingMember): string {
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

export function resolvedGrant(m: DatapackOutgoingMember): DatapackShareGrant {
  if (isDatapackShareGrant(m.grant)) return m.grant;
  return DATAPACK_SHARE_GRANT_VIEWER;
}
