// Tests for the admin token gate: the admin-pool JWKS verification that replaced
// Cognito GetUser as the authorization decision (MEKO-655), and the live-user
// check GetUser still provides on top of it.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const verify = vi.fn();
const send = vi.fn();

vi.mock("aws-jwt-verify", () => ({
  CognitoJwtVerifier: { create: () => ({ verify }) },
}));

vi.mock("@aws-sdk/client-cognito-identity-provider", () => ({
  CognitoIdentityProviderClient: class {
    send = send;
  },
  GetUserCommand: class {},
  InitiateAuthCommand: class {},
  RespondToAuthChallengeCommand: class {},
}));

/** Re-imports the module so its pool/client env is read fresh — the verifier is
 *  built once at import time. */
async function load(poolId = "us-east-2_adminpool") {
  vi.resetModules();
  vi.stubEnv("ADMIN_COGNITO_POOL_ID", poolId);
  vi.stubEnv("ADMIN_COGNITO_CLIENT_ID", "admin-client-id");
  return import("./adminAuth");
}

const cognitoError = (name: string) => Object.assign(new Error(name), { name });
const emailAttrs = { UserAttributes: [{ Name: "email", Value: "ops@yugabyte.com" }] };

describe("verifyAccessToken", () => {
  beforeEach(() => {
    verify.mockReset();
    send.mockReset();
  });
  afterEach(() => vi.unstubAllEnvs());

  it("rejects every token when the admin pool is not configured", async () => {
    const { verifyAccessToken } = await load("");
    expect(await verifyAccessToken("any-token")).toBeNull();
    expect(verify).not.toHaveBeenCalled();
  });

  // The bypass itself: a customer-pool token used to be admitted because GetUser
  // resolves a token from any pool in the region.
  it("rejects a token the admin pool did not issue", async () => {
    const { verifyAccessToken } = await load();
    verify.mockRejectedValue(new Error("Token not from expected issuer"));

    expect(await verifyAccessToken("customer-pool-token")).toBeNull();
    expect(send).not.toHaveBeenCalled();
  });

  it("accepts an admin token and takes the email from Cognito", async () => {
    const { verifyAccessToken } = await load();
    verify.mockResolvedValue({ sub: "sub-1" });
    send.mockResolvedValue(emailAttrs);

    expect(await verifyAccessToken("admin-token")).toEqual({
      sub: "sub-1",
      email: "ops@yugabyte.com",
    });
  });

  it("rejects a signed token whose user Cognito no longer accepts", async () => {
    const { verifyAccessToken } = await load();
    verify.mockResolvedValue({ sub: "sub-1" });
    send.mockRejectedValue(cognitoError("NotAuthorizedException"));

    expect(await verifyAccessToken("revoked-token")).toBeNull();
  });

  it("rejects a signed token for a deleted user", async () => {
    const { verifyAccessToken } = await load();
    verify.mockResolvedValue({ sub: "sub-1" });
    send.mockRejectedValue(cognitoError("UserNotFoundException"));

    expect(await verifyAccessToken("deleted-user-token")).toBeNull();
  });

  // A throttle must not lock the console out, but it must not be cached either:
  // the empty email is what stops an unattributable tier change (routes/usage.ts).
  it("admits without caching when the Cognito lookup is unavailable", async () => {
    const { verifyAccessToken } = await load();
    verify.mockResolvedValue({ sub: "sub-1" });
    send.mockRejectedValueOnce(cognitoError("TooManyRequestsException"));

    expect(await verifyAccessToken("t")).toEqual({ sub: "sub-1", email: "" });

    send.mockResolvedValue(emailAttrs);
    expect(await verifyAccessToken("t")).toEqual({ sub: "sub-1", email: "ops@yugabyte.com" });
    expect(send).toHaveBeenCalledTimes(2);
  });

  it("serves a verified token from cache without re-checking Cognito", async () => {
    const { verifyAccessToken } = await load();
    verify.mockResolvedValue({ sub: "sub-1" });
    send.mockResolvedValue(emailAttrs);

    await verifyAccessToken("admin-token");
    await verifyAccessToken("admin-token");

    expect(verify).toHaveBeenCalledTimes(1);
    expect(send).toHaveBeenCalledTimes(1);
  });
});
