/**
 * Canonical datapack share grant values returned by and sent to the API.
 * Use these constants instead of string literals so UI, mocks, and types stay aligned.
 */
export const DATAPACK_SHARE_GRANT_VIEWER = "viewer" as const;
export const DATAPACK_SHARE_GRANT_CONTRIBUTOR = "contributor" as const;
export const DATAPACK_SHARE_GRANT_MAINTAINER = "maintainer" as const;
export const DATAPACK_SHARE_GRANT_OWNER = "owner" as const;

export const DATAPACK_SHARE_GRANTS = [
  DATAPACK_SHARE_GRANT_VIEWER,
  DATAPACK_SHARE_GRANT_CONTRIBUTOR,
  DATAPACK_SHARE_GRANT_MAINTAINER,
  DATAPACK_SHARE_GRANT_OWNER,
] as const;

export type DatapackShareGrant = (typeof DATAPACK_SHARE_GRANTS)[number];

/**
 * Display a grant in lowerCamelCase: the first segment is lowercased; further segments
 * (split on `_`, `-`, or spaces) are appended with an initial capital (e.g. `SOME_ROLE` → `someRole`).
 * Single-token API values like `maintainer` stay `maintainer`.
 */
export function formatDatapackShareGrantCamelCase(grant: string): string {
  if (grant === DATAPACK_SHARE_GRANT_OWNER) return "Owner";
  if (grant === DATAPACK_SHARE_GRANT_MAINTAINER) return "Maintainer";
  if (grant === DATAPACK_SHARE_GRANT_CONTRIBUTOR) return "Contributor";
  if (grant === DATAPACK_SHARE_GRANT_VIEWER) return "Viewer";
  return grant;
}

export function isDatapackShareGrant(value: unknown): value is DatapackShareGrant {
  return (
    value === DATAPACK_SHARE_GRANT_VIEWER ||
    value === DATAPACK_SHARE_GRANT_CONTRIBUTOR ||
    value === DATAPACK_SHARE_GRANT_MAINTAINER ||
    value === DATAPACK_SHARE_GRANT_OWNER
  );
}

/**
 * True when the signed-in user is the datapack owner on a list row.
 * `GET /datapacks` sets `grant` to `"owner"` for packs you created and the invite grant for shared packs.
 */
export function isDatapackListGrantOwner(dp: { grant?: DatapackShareGrant | null }): boolean {
  return dp.grant === DATAPACK_SHARE_GRANT_OWNER;
}

/** Owner and maintainer may open sharing / manage members; viewer and contributor may not. */
export function isDatapackGrantEligibleToShare(dp: { grant?: DatapackShareGrant | null }): boolean {
  const userGrant = dp.grant;
  return userGrant === DATAPACK_SHARE_GRANT_OWNER || userGrant === DATAPACK_SHARE_GRANT_MAINTAINER;
}

export function isDatapackGrantEligibleToEdit(dp: { grant?: DatapackShareGrant | null }): boolean {
  const userGrant = dp.grant;
  return userGrant === DATAPACK_SHARE_GRANT_OWNER;
}

export function isDatapackGrantEligibleToDelete(dp: { grant?: DatapackShareGrant | null }): boolean {
  const userGrant = dp.grant;
  return userGrant === DATAPACK_SHARE_GRANT_OWNER;
}

export function isDatapackGrantEligibleToAddKnowledgeBase(dp: { grant?: DatapackShareGrant | null }): boolean {
  const userGrant = dp.grant;
  return userGrant === DATAPACK_SHARE_GRANT_OWNER || userGrant === DATAPACK_SHARE_GRANT_MAINTAINER;
}

export function isDatapackGrantEligibleToPromoteMemoryToKnowledge(dp: { grant?: DatapackShareGrant | null }): boolean {
  const userGrant = dp.grant;
  return userGrant === DATAPACK_SHARE_GRANT_OWNER || userGrant === DATAPACK_SHARE_GRANT_MAINTAINER;
}

/**
 * True when the signed-in user can use this datapack in the WorkbenchLM:
 * owner, maintainer, or contributor. Viewer is excluded because the
 * WorkbenchLM writes (memory_add, conversation_add_message) and viewer
 * is read-only — letting a viewer "chat" would fail on the first turn
 * with a permission error from MCP.
 */
export function isDatapackGrantEligibleForWorkbenchLM(
  dp: { grant?: DatapackShareGrant | null },
): boolean {
  const userGrant = dp.grant;
  return (
    userGrant === DATAPACK_SHARE_GRANT_OWNER ||
    userGrant === DATAPACK_SHARE_GRANT_MAINTAINER ||
    userGrant === DATAPACK_SHARE_GRANT_CONTRIBUTOR
  );
}

/** Human-readable list for validation error copy (mock server, etc.). */
export const DATAPACK_SHARE_GRANT_VALIDATION_MESSAGE = `grant must be ${DATAPACK_SHARE_GRANT_VIEWER}, ${DATAPACK_SHARE_GRANT_CONTRIBUTOR}, or ${DATAPACK_SHARE_GRANT_MAINTAINER}`;
