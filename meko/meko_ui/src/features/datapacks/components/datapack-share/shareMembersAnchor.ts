// Anchor on the share page's Members card, so another card (an "already a member"
// toast) can send the owner to where roles are changed.

export const SHARE_PAGE_MEMBERS_ANCHOR_ID = "datapack-share-members";

export function scrollToShareMembers(): void {
  document
    .getElementById(SHARE_PAGE_MEMBERS_ANCHOR_ID)
    ?.scrollIntoView({ behavior: "smooth", block: "start" });
}
