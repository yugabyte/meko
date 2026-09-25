import { useQuery } from "@tanstack/react-query";

import { getAuthConfig, type AuthConfigResponse } from "@/lib/api/mekoApi";

/** Which identity provider this deployment authenticates against. */
export type AuthProvider = "cognito" | "oidc" | "none";

/**
 * What the api-server says the sign-in page should offer. Served unauthenticated
 * at GET /auth/config, because it has to be read before anyone can sign in.
 *
 * `cognito` is the hosted deployments: a password form, a signup link, and the
 * brokered social buttons. `oidc` is a customer's own issuer, which owns its
 * users — so no password form and no signup, only a redirect to the issuer.
 * `none` is a bring-up with no IdP, where every request shares one account.
 */
export interface AuthConfig {
  provider: AuthProvider;
  loginUrl?: string;
  passwordLogin: boolean;
  signup: boolean;
  socialProviders: string[];
}

/**
 * The shape assumed when /auth/config cannot be read — an older api-server that
 * does not serve it, or a transient failure. Cognito, because that is what every
 * deployment predating the endpoint runs; guessing anything else would hide the
 * sign-in form from users who need it.
 */
export const FALLBACK_AUTH_CONFIG: AuthConfig = {
  provider: "cognito",
  passwordLogin: true,
  signup: true,
  socialProviders: ["google", "linkedin"],
};

export const AUTH_CONFIG_QUERY_KEY = "auth-config";

function parse(raw: AuthConfigResponse | undefined): AuthConfig {
  const provider = raw?.provider;
  if (provider !== "cognito" && provider !== "oidc" && provider !== "none") {
    return FALLBACK_AUTH_CONFIG;
  }
  return {
    provider,
    loginUrl: typeof raw?.login_url === "string" ? raw.login_url : undefined,
    passwordLogin: raw?.password_login === true,
    signup: raw?.signup === true,
    socialProviders: Array.isArray(raw?.social_providers)
      ? raw.social_providers.filter((p): p is string => typeof p === "string")
      : [],
  };
}

/**
 * Reads the deployment's auth config.
 *
 * `null` while in flight — callers render nothing rather than flashing a
 * password form that this deployment may not have, which is the whole reason
 * the page waits. A failure resolves to the Cognito fallback rather than
 * leaving the page blank.
 *
 * Cached for the session: which IdP a deployment runs is fixed at install, so
 * refetching it on every mount of the sign-in page buys nothing.
 */
export function useAuthConfig(): AuthConfig | null {
  const { data, isError } = useQuery({
    queryKey: [AUTH_CONFIG_QUERY_KEY],
    queryFn: async ({ signal }) => parse(await getAuthConfig({ signal })),
    staleTime: Infinity,
    gcTime: Infinity,
    retry: false,
    refetchOnWindowFocus: false,
  });

  if (isError) return FALLBACK_AUTH_CONFIG;
  return data ?? null;
}
