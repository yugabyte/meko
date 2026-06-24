import {
  CognitoIdentityProviderClient,
  ListUsersCommand,
  AdminLinkProviderForUserCommand,
} from "@aws-sdk/client-cognito-identity-provider";

const client = new CognitoIdentityProviderClient({});

// Strict, injection-safe email shape: no quotes/whitespace/control chars, so
// the value is safe to interpolate into the ListUsers filter and is a plain
// address (not a quoted local-part). Its job is a guard, not RFC validation.
const EMAIL_RE = /^[^\s@"']+@[^\s@"']+\.[^\s@"']+$/;

// Cognito PreSignUp trigger.
//
// Runs just before Cognito creates a user. For *federated* first-time sign-ins
// (Google, GitHub, ...) we look for an existing pool user with the same verified
// email and link the external identity into it via AdminLinkProviderForUser.
// This keeps one canonical account (one `sub`) regardless of login method.
//
// Linking is gated on several checks — we never link unless ALL hold, otherwise
// we fall through and let Cognito create a fresh, separate user for the
// federated identity:
//   1. the incoming email is present, well-formed, and email_verified=true
//      (mapped from the IdP);
//   2. the matched destination account's email is an exact (case-insensitive)
//      match; and
//   3. the destination account itself has email_verified=true AND a linkable
//      status (CONFIRMED native user, or EXTERNAL_PROVIDER federated user).
// Check 3 closes an account-takeover hole: an attacker could native-signup
// someone else's address and leave it UNCONFIRMED/email_verified=false; without
// this gate the real owner's federated identity would be welded into the
// attacker's shell. The real trust boundary is email_verified=true (a trusted
// IdP/native verification vouched for the address); UNCONFIRMED shells fail it.
// EXTERNAL_PROVIDER is allowed so a user who signed up with one social provider
// (e.g. Google) is linked — not duplicated — when they later sign in with
// another (e.g. GitHub) using the same verified email; AdminLinkProviderForUser
// supports a federated DestinationUser by design.
export const handler = async (event) => {
  try {
    if (event.triggerSource !== "PreSignUp_ExternalProvider") {
      return event;
    }

    // NOTE: Cognito ignores autoVerifyEmail for federated users, so email_verified
    // is set via IdP attribute mapping (Google's email_verified; the proxy returns
    // email_verified=true for GitHub) — not here.

    const userPoolId = event.userPoolId;
    const email = event.request.userAttributes?.email;
    const emailVerified = event.request.userAttributes?.email_verified === "true";
    // event.userName looks like "Google_1234567890" or "GitHub_42".
    const [providerName, providerSub] = splitProviderUserName(event.userName);

    // Only link on a present, *verified*, well-formed email. The link welds the
    // incoming external identity into an existing account that owns this email,
    // so an unverified/attacker-supplied address would be an account-takeover
    // vector. The regex also keeps the value safe to interpolate into the
    // ListUsers filter (a stray quote would otherwise break the expression).
    if (!email || !emailVerified || !EMAIL_RE.test(email)) {
      console.log("PRESIGNUP_SKIP_LINK", JSON.stringify({
        reason: "email missing/unverified/malformed",
        hasEmail: !!email,
        emailVerified,
        wellFormed: email ? EMAIL_RE.test(email) : false,
      }));
      return event;
    }

    const { Users = [] } = await client.send(
      new ListUsersCommand({
        UserPoolId: userPoolId,
        Filter: `email = "${email}"`,
        Limit: 1,
      })
    );

    const match = Users[0];
    if (!match) {
      return event;
    }

    // Re-confirm an exact, case-insensitive email match before linking, instead
    // of trusting whatever Cognito's filter returned — defends against any
    // future change to filter semantics linking us to the wrong account.
    const matchEmail = match.Attributes?.find((a) => a.Name === "email")?.Value;
    const exactMatch = !!matchEmail && matchEmail.toLowerCase() === email.toLowerCase();

    // Only link into a destination whose email is verified by a trusted source
    // and whose status is linkable: CONFIRMED (native, email-verified) or
    // EXTERNAL_PROVIDER (already-federated, email mapped verified). This allows
    // cross-provider linking (Google then GitHub, same verified email) while
    // still rejecting an UNCONFIRMED/unverified shell an attacker may have
    // created for an address they don't control.
    const status = match.UserStatus;
    const isLinkableStatus = status === "CONFIRMED" || status === "EXTERNAL_PROVIDER";
    const destEmailVerified = (match.Attributes || []).some(
      (a) => a.Name === "email_verified" && a.Value === "true",
    );

    if (!exactMatch || !isLinkableStatus || !destEmailVerified) {
      console.log("PRESIGNUP_SKIP_LINK", JSON.stringify({
        reason: "destination not eligible",
        exactMatch,
        status,
        destEmailVerified,
      }));
      return event;
    }

    await client.send(
      new AdminLinkProviderForUserCommand({
        UserPoolId: userPoolId,
        DestinationUser: {
          ProviderName: "Cognito",
          ProviderAttributeValue: match.Username,
        },
        SourceUser: {
          ProviderName: providerName,
          ProviderAttributeName: "Cognito_Subject",
          ProviderAttributeValue: providerSub,
        },
      })
    );
    event.response.autoConfirmUser = true;
    console.log("PRESIGNUP_LINKED", "linked + auto-confirmed existing account");

    return event;
  } catch (err) {
    // Surface the real failure instead of letting Cognito report a generic
    // "Unrecognizable lambda output".
    console.error("PRESIGNUP_ERROR", err?.name, err?.message, err?.stack);
    throw err;
  }
};

function splitProviderUserName(userName) {
  const idx = userName.indexOf("_");
  // No underscore → treat the whole thing as the provider sub (avoids
  // slice(0, -1) lopping off the last char).
  if (idx === -1) {
    return ["", userName];
  }
  return [userName.slice(0, idx), userName.slice(idx + 1)];
}
