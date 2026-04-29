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

/** Human-readable list for validation error copy (mock server, etc.). */
export const DATAPACK_SHARE_GRANT_VALIDATION_MESSAGE = `grant must be ${DATAPACK_SHARE_GRANT_VIEWER}, ${DATAPACK_SHARE_GRANT_CONTRIBUTOR}, or ${DATAPACK_SHARE_GRANT_MAINTAINER}`;
