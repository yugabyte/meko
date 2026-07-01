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

// Reserved, non-deliverable base domain (RFC 2606 .invalid TLD) used to
// synthesize a unique placeholder email when a federated IdP (e.g. LinkedIn,
// whose email claim is OPTIONAL per the OIDC spec) doesn't supply one. The pool
// marks email required AND uses it as the username alias, so federation can't
// complete without a value. We mint a per-identity unique, provider-tagged
// address of the shape "<username>@no-reply.<provider>.meko.invalid" (provider
// parsed from the Cognito username, which is "<Provider>_<sub>" — e.g. linkedin
// / github) and leave email_verified=false so it's never treated as a real,
// verified address. The app gates the account and collects + verifies the
// user's real email during onboarding, then overwrites both Cognito and the DB.
//
// IMPORTANT: the api_server detects these addresses by the "no-reply." host
// prefix + ".meko.invalid" suffix (see service.isPlaceholderEmail) to drive the
// "collect your email" onboarding gate. Keep the two in sync.
const PLACEHOLDER_EMAIL_BASE_DOMAIN = "meko.invalid";

// providerFromUsername parses the IdP/provider name out of a federated Cognito
// username ("<Provider>_<sub>", e.g. "LinkedIn_abc" / "GitHub_123"), lowercased
// and sanitized for use in a hostname. Falls back to "federated" when there's
// no provider segment.
function providerFromUsername(username) {
  const u = String(username || "");
  const sep = u.indexOf("_");
  const provider = sep === -1 ? "" : u.slice(0, sep);
  return provider.toLowerCase().replace(/[^a-z0-9-]/g, "") || "federated";
}

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
  if (event.triggerSource !== "InboundFederation_ExternalProvider") {
    return event;
  }

  // Returning user: no-op. We return an explicit EMPTY `userAttributesToMap`
  // (set below) so Cognito keeps the stored attributes and updates only what the
  // IdP actually sent, so we never overwrite a corrected `family_name` with the
  // placeholder again. (An empty map — not an unset/undefined one — is what
  // makes this a true no-op; see the note on the assignment below.)
  if (await userExists(event.userPoolId, event.userName)) {
    console.log("INBOUND_FED_EXISTING_USER", event.userName);
    // Documented no-op: returning an EMPTY map tells Cognito to keep the user's
    // existing attributes unchanged. Returning undefined (or a partial map) is
    // NOT a no-op — Cognito drops any attribute absent from userAttributesToMap,
    // which would wipe a returning user's stored profile (e.g. a corrected
    // family_name) on every sign-in. So we must set {} explicitly here.
    event.response = { ...(event.response ?? {}), userAttributesToMap: {} };
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

  // Email is required + the username alias, so a missing email (LinkedIn's
  // email claim is optional) would fail federation outright. Synthesize a
  // unique, non-deliverable placeholder from the Cognito username (already
  // "<Provider>_<sub>", so unique per identity → no collisions) and force
  // email_verified=false. The app detects this domain post-login and makes the
  // user supply + verify their real email before granting access.
  if (!merged.email) {
    const provider = providerFromUsername(event.userName);
    const localPart = String(event.userName || "user").replace(/[^A-Za-z0-9._-]/g, "_");
    merged.email = `${localPart}@no-reply.${provider}.${PLACEHOLDER_EMAIL_BASE_DOMAIN}`;
    merged.email_verified = "false";
    console.log("INBOUND_FED_PLACEHOLDER_EMAIL", merged.email);
  }

  event.response = { ...(event.response ?? {}), userAttributesToMap: merged };
  // Log only the attribute KEYS we mapped (not values) to avoid writing the
  // user's name/email into CloudWatch.
  console.log("INBOUND_FED_MAPPED_ATTRS", Object.keys(merged).join(","));
  return event;
};
