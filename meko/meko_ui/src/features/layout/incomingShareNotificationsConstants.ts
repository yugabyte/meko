// User-visible copy for the workspace header datapack share invitation bell and popover.

export const INCOMING_SHARE_NOTIFICATION_ARIA = "Datapack share invitations";

export const INCOMING_SHARE_NOTIFICATION_TITLE = "Invitations";

export const INCOMING_SHARE_NOTIFICATION_LOADING = "Loading invitations…";

export const INCOMING_SHARE_NOTIFICATION_EMPTY = "No pending invitations.";

export const INCOMING_SHARE_ACCEPT_LABEL = "Accept";

export const INCOMING_SHARE_DECLINE_LABEL = "Decline";

export const INCOMING_SHARE_TOAST_ACCEPTED_TITLE = "Invite accepted";

export const INCOMING_SHARE_TOAST_DECLINED_TITLE = "Invite declined";

export const INCOMING_SHARE_TOAST_ACTION_FAILED_TITLE = "Could not update invite";

/**
 * Single-line summary for a pending share (owner display name, role label, datapack name).
 */
export function formatIncomingShareInviteMessage(
  ownerName: string,
  grantRoleLabel: string,
  datapackName: string,
): string {
  return `${ownerName} invited you as a ${grantRoleLabel} to ${datapackName}`;
}
