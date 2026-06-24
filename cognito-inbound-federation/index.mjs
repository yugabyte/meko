// Cognito Inbound Federation trigger.
//
// Runs on *every* federated sign-in (Google, GitHub, ...), before Cognito
// validates required user-pool attributes and before it creates/updates the
// federated profile. We use it to fill placeholder values for required
// attributes the IdP didn't supply.
//
// Why this exists: our pool marks `family_name` (and `given_name`) as required.
// Google only returns those claims when the account actually has them set, so a
// Google account with no last name fails federation with
// `invalid_request / attributes required: [family_name]` — the error never
// reaches our api_server, it's thrown inside Cognito. Filling a placeholder here
// lets the user be created; the onboarding flow (ConfirmNameModal) then collects
// and persists the real first/last name.
//
// IDEMPOTENCY: this trigger fires on every sign-in, so it must NOT re-stamp the
// "-" placeholder over a name the user already corrected. We therefore only
// inject placeholders for BRAND-NEW users. For a user who already exists in the
// pool we return WITHOUT setting `userAttributesToMap`, which tells Cognito to
// keep the existing stored attributes (it only updates claims the IdP actually
// sent). Google never sends `family_name` for these accounts, so a `family_name`
// the user set (synced from onboarding via api_server's UpdateProfile) is
// preserved instead of being overwritten with "-".
//
// IMPORTANT: whatever we put in `response.userAttributesToMap` is still filtered
// by the IdP attribute mapping afterward. So every attribute we set here
// (`given_name`, `family_name`, `name`) must ALSO be mapped in the provider's
// attribute-mapping config, or Cognito drops the injected value and the
// required-attribute check fails again.
//
// IAM: this Lambda's execution role needs `cognito-idp:AdminGetUser` on the
// user pool (to tell new vs. returning users apart).
import {
  CognitoIdentityProviderClient,
  AdminGetUserCommand,
} from "@aws-sdk/client-cognito-identity-provider";

const PLACEHOLDER = "-";
const cognito = new CognitoIdentityProviderClient({});

// userExists reports whether `username` already has a profile in the pool.
// A missing user (UserNotFoundException) is the new-user case; any other error
// is rethrown so the sign-in fails loudly rather than silently re-stamping.
async function userExists(userPoolId, username) {
  try {
    await cognito.send(
      new AdminGetUserCommand({ UserPoolId: userPoolId, Username: username }),
    );
    return true;
  } catch (err) {
    if (err?.name === "UserNotFoundException") return false;
    throw err;
  }
}

export const handler = async (event) => {
  // TEMP DIAGNOSTICS: log the event so we can see provider + which attributes
  // arrived from the IdP (userInfo vs idToken).
  console.log("INBOUND_FED_EVENT", JSON.stringify(event));

  if (event.triggerSource !== "InboundFederation_ExternalProvider") {
    console.log("INBOUND_FED_BRANCH", "not external provider", event.triggerSource);
    return event;
  }

  // Returning user: no-op. Leaving `userAttributesToMap` unset makes Cognito
  // keep the stored attributes and update only what the IdP actually sent, so we
  // never overwrite a corrected `family_name` with the placeholder again.
  if (await userExists(event.userPoolId, event.userName)) {
    console.log("INBOUND_FED_BRANCH", "existing user; preserving attributes", event.userName);
    return event;
  }

  const attrs = event.request?.attributes ?? {};
  // OIDC / social providers: prefer the richer userInfo endpoint, fall back to
  // the id_token claims when userInfo is empty or unavailable.
  const raw = {
    ...(attrs.userInfo ?? {}),
    ...(attrs.idToken ?? {}),
  };

  // userAttributesToMap MUST be a flat map of string -> string. The token
  // payloads include non-string values (email_verified booleans, iat/exp
  // numbers, nested objects/arrays like `identities`/`address`); returning any
  // of those makes Cognito reject the whole response with "invalid response
  // format". So coerce scalars to strings and drop everything else.
  const merged = {};
  for (const [key, value] of Object.entries(raw)) {
    if (value === null || value === undefined) continue;
    const t = typeof value;
    if (t === "string") merged[key] = value;
    else if (t === "number" || t === "boolean") merged[key] = String(value);
    // objects / arrays are not mappable -> skip
  }

  if (!merged.given_name) merged.given_name = PLACEHOLDER;
  if (!merged.family_name) merged.family_name = PLACEHOLDER;
  if (!merged.name) {
    merged.name = `${merged.given_name} ${merged.family_name}`.trim() || PLACEHOLDER;
  }

  event.response = { ...(event.response ?? {}), userAttributesToMap: merged };
  console.log("INBOUND_FED_RETURN", JSON.stringify(event.response.userAttributesToMap));
  return event;
};
