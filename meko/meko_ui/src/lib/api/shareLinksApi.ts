// Datapack share links: one URL per datapack that lets anyone signed in join it,
// plus removing an accepted member. Gated by MEKO_ENABLE_LINK_SHARING on both
// sides -- see lib/linkSharingFeature.ts.

import { request } from "@/lib/api/mekoApi";

type RouteOptions = { signal?: AbortSignal };

/** Roles a share link can grant. Maintainer is email-invite only. */
export const SHARE_LINK_GRANTS = ["viewer", "contributor"] as const;
export type ShareLinkGrant = (typeof SHARE_LINK_GRANTS)[number];

export const SHARE_LINK_STATUS_ACTIVE = "active" as const;
export const SHARE_LINK_STATUS_OFF = "off" as const;

/** API error code for a redeem by someone the owner removed from the datapack. */
export const SHARE_LINK_REMOVED_ERROR_CODE = "REMOVED_FROM_DATAPACK";
export type ShareLinkStatus = typeof SHARE_LINK_STATUS_ACTIVE | typeof SHARE_LINK_STATUS_OFF;

export type DatapackShareLink = {
  datapack_id: string;
  token: string;
  grant: ShareLinkGrant;
  status: ShareLinkStatus;
  /** People who joined through the link; repeat clicks don't count. */
  redemption_count: number;
  /** People who joined through the link and still hold its role: the ones a
   *  role change moves with it. */
  members_at_link_role: number;
  created_by_user_id: string;
  created_at: string;
  updated_at: string;
};

export type ShareLinkRedemption = {
  datapack_id: string;
  grant: string;
  already_member: boolean;
};

function shareLinkRoute(datapackId: string): string {
  return `/datapacks/${encodeURIComponent(datapackId)}/share-link`;
}

function shareTokenRoute(token: string): string {
  return `/share/${encodeURIComponent(token)}`;
}

/** The datapack's link, or null when it has none yet. Owner or maintainer. */
export async function getShareLink(datapackId: string, options: RouteOptions = {}) {
  const res = await request<{ share_link: DatapackShareLink | null }>(
    shareLinkRoute(datapackId),
    "GET",
    undefined,
    options,
  );
  return res.share_link;
}

/** Creates the link, or changes its role or status. Omitted fields are kept. */
/** A share-link change. `reset` replaces the token, so the old URL stops working. */
export type ShareLinkUpdate = { grant?: ShareLinkGrant; status?: ShareLinkStatus; reset?: boolean };

export async function updateShareLink(datapackId: string, body: ShareLinkUpdate) {
  const res = await request<{ share_link: DatapackShareLink }>(
    shareLinkRoute(datapackId),
    "PUT",
    body,
  );
  return res.share_link;
}

/** Joins the signed-in user to the datapack behind the link. */
export function redeemShareLink(token: string) {
  return request<ShareLinkRedemption>(`${shareTokenRoute(token)}/redeem`, "POST");
}

/** Removes an accepted member. Owner or maintainer; the owner can't be removed. */
export function removeDatapackMember(datapackId: string, userId: string) {
  return request<unknown>(
    `/datapacks/${encodeURIComponent(datapackId)}/members/${encodeURIComponent(userId)}`,
    "DELETE",
  );
}
