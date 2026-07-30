import { createHmac } from "crypto";
import {
  CognitoIdentityProviderClient,
  InitiateAuthCommand,
  RespondToAuthChallengeCommand,
  GetUserCommand,
  type AuthenticationResultType,
} from "@aws-sdk/client-cognito-identity-provider";

const client = new CognitoIdentityProviderClient({
  region: process.env.AWS_REGION ?? "us-east-1",
});

const CLIENT_ID = process.env.ADMIN_COGNITO_CLIENT_ID ?? "";
const CLIENT_SECRET = process.env.ADMIN_COGNITO_CLIENT_SECRET ?? "";

function secretHash(username: string): string | undefined {
  if (!CLIENT_SECRET) return undefined;
  return createHmac("sha256", CLIENT_SECRET).update(username + CLIENT_ID).digest("base64");
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

export async function loginWithCognito(
  email: string,
  password: string,
): Promise<LoginResult> {
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

/** Validates an access token via Cognito GetUser. Results are cached for 5 min. */
export async function verifyAccessToken(token: string): Promise<AdminUser | null> {
  const cached = tokenCache.get(token);
  if (cached && cached.expiresAt > Date.now()) {
    return { sub: cached.sub, email: cached.email };
  }

  try {
    const resp = await client.send(new GetUserCommand({ AccessToken: token }));
    const attr = (name: string) =>
      resp.UserAttributes?.find((a) => a.Name === name)?.Value ?? "";
    const user: AdminUser = { sub: attr("sub"), email: attr("email") };
    tokenCache.set(token, { ...user, expiresAt: Date.now() + 5 * 60_000 });
    return user;
  } catch {
    tokenCache.delete(token);
    return null;
  }
}
