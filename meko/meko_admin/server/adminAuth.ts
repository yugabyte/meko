import { createHmac } from "crypto";
import {
  CognitoIdentityProviderClient,
  InitiateAuthCommand,
  RespondToAuthChallengeCommand,
  GetUserCommand,
  type AuthenticationResultType,
} from "@aws-sdk/client-cognito-identity-provider";
import { CognitoJwtVerifier } from "aws-jwt-verify";

const client = new CognitoIdentityProviderClient({
  region: process.env.AWS_REGION ?? "us-east-1",
});

const POOL_ID = process.env.ADMIN_COGNITO_POOL_ID ?? "";
const CLIENT_ID = process.env.ADMIN_COGNITO_CLIENT_ID ?? "";
const CLIENT_SECRET = process.env.ADMIN_COGNITO_CLIENT_SECRET ?? "";

function secretHash(username: string): string | undefined {
  if (!CLIENT_SECRET) return undefined;
  return createHmac("sha256", CLIENT_SECRET)
    .update(username + CLIENT_ID)
    .digest("base64");
}

// In-memory cache: access token → { sub, email, expiresAt }
type CachedUser = { sub: string; email: string; expiresAt: number };
const tokenCache = new Map<string, CachedUser>();

// Evict expired entries every 10 minutes so stale tokens don't accumulate.
setInterval(() => {
  const now = Date.now();
  tokenCache.forEach((cached, token) => {
    if (cached.expiresAt <= now) tokenCache.delete(token);
  });
}, 10 * 60_000).unref();

export type LoginResult =
  | { type: "success"; tokens: AuthenticationResultType }
  | { type: "new_password_required"; session: string; email: string };

export async function loginWithCognito(email: string, password: string): Promise<LoginResult> {
  if (!CLIENT_ID) throw new Error("ADMIN_COGNITO_CLIENT_ID is not configured");

  const hash = secretHash(email);
  const resp = await client.send(
    new InitiateAuthCommand({
      AuthFlow: "USER_PASSWORD_AUTH",
      ClientId: CLIENT_ID,
      AuthParameters: {
        USERNAME: email,
        PASSWORD: password,
        ...(hash ? { SECRET_HASH: hash } : {}),
      },
    }),
  );

  if (resp.ChallengeName === "NEW_PASSWORD_REQUIRED") {
    return { type: "new_password_required", session: resp.Session!, email };
  }

  return { type: "success", tokens: resp.AuthenticationResult! };
}

export async function completeNewPassword(
  email: string,
  newPassword: string,
  session: string,
  givenName?: string,
  familyName?: string,
): Promise<AuthenticationResultType> {
  const hash = secretHash(email);
  const resp = await client.send(
    new RespondToAuthChallengeCommand({
      ClientId: CLIENT_ID,
      ChallengeName: "NEW_PASSWORD_REQUIRED",
      Session: session,
      ChallengeResponses: {
        USERNAME: email,
        NEW_PASSWORD: newPassword,
        ...(hash ? { SECRET_HASH: hash } : {}),
        ...(givenName ? { "userAttributes.given_name": givenName } : {}),
        ...(familyName ? { "userAttributes.family_name": familyName } : {}),
      },
    }),
  );
  return resp.AuthenticationResult!;
}

export type AdminUser = { sub: string; email: string };

// Authorization rests on the token's own signed claims: issued by the dedicated
// ADMIN pool, for the admin app client, and an access token rather than an id
// token. GetUser establishes none of that -- it is an unsigned API routed by the
// token itself, so it resolves a token from ANY pool in the region, and using it
// as the gate let an ordinary customer's access token authenticate as an admin
// (MEKO-655). Missing config fails closed; there is no weaker fallback.
const verifier =
  POOL_ID && CLIENT_ID
    ? CognitoJwtVerifier.create({
        userPoolId: POOL_ID,
        tokenUse: "access",
        clientId: CLIENT_ID,
      })
    : null;

/** Cognito's live view of the caller, called only once the token is proven to be
 *  this pool's. The JWKS check covers the signature and exp alone, so this stays
 *  the check that catches an admin who has since been disabled or deleted and a
 *  token killed by GlobalSignOut -- and it supplies the email, which an access
 *  token carries no claim for. */
async function lookupUser(token: string): Promise<{ email: string } | "revoked" | "unavailable"> {
  try {
    const resp = await client.send(new GetUserCommand({ AccessToken: token }));
    return { email: resp.UserAttributes?.find((a) => a.Name === "email")?.Value ?? "" };
  } catch (err) {
    const name = (err as { name?: string })?.name ?? "";
    if (name === "NotAuthorizedException" || name === "UserNotFoundException") return "revoked";
    return "unavailable";
  }
}

/** Verifies an admin access token. Results are cached for 5 min. */
export async function verifyAccessToken(token: string): Promise<AdminUser | null> {
  const cached = tokenCache.get(token);
  if (cached && cached.expiresAt > Date.now()) {
    return { sub: cached.sub, email: cached.email };
  }

  if (!verifier) {
    console.error(
      "[adminAuth] ADMIN_COGNITO_POOL_ID / ADMIN_COGNITO_CLIENT_ID unset -- rejecting all requests",
    );
    return null;
  }

  let sub: string;
  try {
    sub = (await verifier.verify(token)).sub;
  } catch {
    tokenCache.delete(token);
    return null;
  }

  const lookup = await lookupUser(token);
  if (lookup === "revoked") {
    tokenCache.delete(token);
    return null;
  }
  if (lookup === "unavailable") {
    // Cognito throttled or unreachable. Admit on the verified claims rather than
    // locking the console out over a blip, but do NOT cache: the next request
    // retries the live check, and nothing invents an email for the tier audit
    // trail (routes/usage.ts refuses the write while it is empty).
    return { sub, email: "" };
  }

  const user: AdminUser = { sub, email: lookup.email };
  tokenCache.set(token, { ...user, expiresAt: Date.now() + 5 * 60_000 });
  return user;
}
