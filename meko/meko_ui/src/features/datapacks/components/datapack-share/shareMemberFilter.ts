// Pure search/filter helpers for the datapack members roster.
//
// Role taxonomy comes from datapackShareGrants / shareInviteRoles — this file
// never defines its own role list.
import type { DatapackOutgoingMember } from "@/lib/api/mekoApi";
import type { DatapackShareGrant } from "@/lib/datapackShareGrants";
import {
  DATAPACK_SHARE_GRANT_OWNER,
  formatDatapackShareGrantCamelCase,
} from "@/lib/datapackShareGrants";
import { SHARE_INVITE_ROLES } from "@/features/datapacks/components/datapack-share/shareInviteRoles";

/** Sentinel for the "any role" option in the role dropdown. */
export const SHARE_MEMBER_ROLE_FILTER_ALL = "all" as const;

export type ShareMemberRoleFilter = typeof SHARE_MEMBER_ROLE_FILTER_ALL | DatapackShareGrant;

export type ShareMemberFilter = {
  /** Free text matched against name, email, email domain, and role label. */
  query: string;
  role: ShareMemberRoleFilter;
};

export const EMPTY_SHARE_MEMBER_FILTER: ShareMemberFilter = {
  query: "",
  role: SHARE_MEMBER_ROLE_FILTER_ALL,
};

/**
 * Options for the role dropdown: Owner first, then the invite roles in their
 * canonical order. Derived from SHARE_INVITE_ROLES so a new role shows up here
 * automatically.
 */
export const SHARE_MEMBER_ROLE_FILTER_OPTIONS: { id: ShareMemberRoleFilter; label: string }[] = [
  { id: SHARE_MEMBER_ROLE_FILTER_ALL, label: "All roles" },
  {
    id: DATAPACK_SHARE_GRANT_OWNER,
    label: formatDatapackShareGrantCamelCase(DATAPACK_SHARE_GRANT_OWNER),
  },
  ...SHARE_INVITE_ROLES.map((r) => ({ id: r.id as ShareMemberRoleFilter, label: r.label })),
];

/**
 * The role to filter and display by.
 *
 * `GET member-list` tags the OWNER's own row with `maintainer` (a known backend
 * quirk in GetOutgoingMembers, also worked around in DatapackSharePage), so the
 * `is_owner` flag — not `grant` — decides whether a row is the owner.
 */
export function effectiveMemberGrant(member: DatapackOutgoingMember): DatapackShareGrant {
  return member.is_owner ? DATAPACK_SHARE_GRANT_OWNER : member.grant;
}

/** Lowercased domain part of an email (`""` when there is no `@`). */
export function memberEmailDomain(email: string | null | undefined): string {
  const at = (email ?? "").lastIndexOf("@");
  if (at < 0) return "";
  return email!.slice(at + 1).trim().toLowerCase();
}

/**
 * Does one member match the filter?
 *
 * Query terms are whitespace-separated and ALL must match (AND), each against
 * any of name / email / domain / role label. That makes `yugabyte viewer` a
 * useful query, and a bare `@yugabyte.com` or `yugabyte.com` both work because
 * the email itself is searched as a substring.
 */
export function matchesShareMemberFilter(
  member: DatapackOutgoingMember,
  filter: ShareMemberFilter,
): boolean {
  const grant = effectiveMemberGrant(member);
  if (filter.role !== SHARE_MEMBER_ROLE_FILTER_ALL && grant !== filter.role) {
    return false;
  }

  const terms = filter.query.trim().toLowerCase().split(/\s+/).filter(Boolean);
  if (terms.length === 0) return true;

  const haystack = [
    member.user_name ?? "",
    member.user_email_id ?? "",
    memberEmailDomain(member.user_email_id),
    formatDatapackShareGrantCamelCase(grant),
  ]
    .join(" ")
    .toLowerCase();

  return terms.every((t) => haystack.includes(t));
}

/** Filter the roster, preserving the server's ordering. */
export function filterShareMembers(
  members: DatapackOutgoingMember[],
  filter: ShareMemberFilter,
): DatapackOutgoingMember[] {
  return members.filter((m) => matchesShareMemberFilter(m, filter));
}

/** True when any filter is active (drives the "Showing X of Y" line and Clear button). */
export function isShareMemberFilterActive(filter: ShareMemberFilter): boolean {
  return filter.query.trim().length > 0 || filter.role !== SHARE_MEMBER_ROLE_FILTER_ALL;
}
