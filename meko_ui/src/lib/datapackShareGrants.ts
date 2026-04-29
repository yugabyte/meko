/**
 * Canonical datapack share grant values returned by and sent to the API.
 * Use these constants instead of string literals so UI, mocks, and types stay aligned.
 */
export const DATAPACK_SHARE_GRANT_VIEWER = "viewer" as const;
export const DATAPACK_SHARE_GRANT_CONTRIBUTOR = "contributor" as const;
export const DATAPACK_SHARE_GRANT_MAINTAINER = "maintainer" as const;

export const DATAPACK_SHARE_GRANTS = [
  DATAPACK_SHARE_GRANT_VIEWER,
  DATAPACK_SHARE_GRANT_CONTRIBUTOR,
  DATAPACK_SHARE_GRANT_MAINTAINER,
] as const;

export type DatapackShareGrant = (typeof DATAPACK_SHARE_GRANTS)[number];

export function isDatapackShareGrant(value: unknown): value is DatapackShareGrant {
  return (
    value === DATAPACK_SHARE_GRANT_VIEWER ||
    value === DATAPACK_SHARE_GRANT_CONTRIBUTOR ||
    value === DATAPACK_SHARE_GRANT_MAINTAINER
  );
}

/** Human-readable list for validation error copy (mock server, etc.). */
export const DATAPACK_SHARE_GRANT_VALIDATION_MESSAGE = `grant must be ${DATAPACK_SHARE_GRANT_VIEWER}, ${DATAPACK_SHARE_GRANT_CONTRIBUTOR}, or ${DATAPACK_SHARE_GRANT_MAINTAINER}`;
