// Centralized RudderStack event names. Keep every tracked event name here so the
// same string is never re-typed at a call site and analytics dashboards stay consistent.
// Names follow the GTM snake_case convention.

export const TELEMETRY_EVENTS = {
  /** Account created (email/password signup submitted). Props: email, signup_method. */
  USER_SIGNED_UP: "user_signed_up",
  /** Successful sign-in. Props: email, login_method. */
  USER_SIGNED_IN: "user_signed_in",
  /** User clicked "Sign out". Pairs with user_signed_in to measure session length. */
  USER_LOGGED_OUT: "user_logged_out",
  /** Datapack created via the setup wizard. Props: email, datapack_id, datapack_name, is_first, total_datapacks. */
  DATAPACK_CREATED: "datapack_created",
  /** A document finished uploading + processing. Props: email, datapack_id, document_id, file_type, file_size_kb, is_first, total_docs. */
  DOCUMENT_UPLOADED: "document_uploaded",
  /** First message of a new Workbench conversation. Props: email, conversation_id, datapack_id, client, role, total_conversations, is_first. */
  CONVERSATION_STARTED: "conversation_started",
  /** Owner sent a datapack share invite. Props: datapack_id, shared_by_email, invited_email, shared_by_role, invited_role, is_first_share, total_shares, shared_timestamp. */
  DATAPACK_SHARED: "datapack_shared",
  /** Recipient accepted a datapack invite. Props: datapack_id, recipient_email, recipient_role, shared_by_email, shared_by_user_id, accepted_timestamp. */
  INVITE_ACCEPTED: "invite_accepted",
  /** User generated an API key. Props: user_id, email, key_label, is_first. */
  API_KEY_GENERATED: "api_key_generated",
} as const;

export type TelemetryEventName = (typeof TELEMETRY_EVENTS)[keyof typeof TELEMETRY_EVENTS];

/** Auth method values for signup_method / login_method. Today only email/password is
 *  wired into the UI; google_oauth / linkedin / github slot in here as they're added. */
export const AUTH_METHODS = {
  EMAIL_PASSWORD: "email_password",
} as const;

/** Value for the conversation_started `client` property. */
export const TELEMETRY_CLIENT_WORKBENCH = "workbench" as const;
