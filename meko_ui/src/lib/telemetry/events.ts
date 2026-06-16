// Centralized RudderStack event names. Keep every tracked event name here so the
// same string is never re-typed at a call site and analytics dashboards stay consistent.

export const TELEMETRY_EVENTS = {
  /** User submitted the sign-in form (the "Continue" button on Sign In). */
  SIGN_IN_CLICKED: "Sign In - Clicked",
  /** Sign-in succeeded and the current user was resolved. */
  SIGNED_IN: "Signed In",
  /** User submitted the sign-up form (the "Continue" button on Sign Up). */
  SIGN_UP_CLICKED: "Sign Up - Clicked",
  /** Owner sent a datapack share invite by email (the "Send invite" button). */
  DATAPACK_SHARE_INVITE_SENT: "Datapack Share Invite - Sent",
  /** User clicked "Sign out" — pairs with "Signed In" to measure session length. */
  LOGGED_OUT: "Logged Out",
} as const;

export type TelemetryEventName = (typeof TELEMETRY_EVENTS)[keyof typeof TELEMETRY_EVENTS];
