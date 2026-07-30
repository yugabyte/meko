function getBaseHostedUiConfig() {
  const domain = (import.meta.env.VITE_COGNITO_DOMAIN || "").trim();
  const clientId = (import.meta.env.VITE_COGNITO_CLIENT_ID || "").trim();
  const successUri = (import.meta.env.VITE_COGNITO_SUCCESS_URI || "").trim();
  const redirectUri =
    successUri ||
    (import.meta.env.VITE_COGNITO_REDIRECT_URI || "").trim() ||
    `${window.location.origin}/login`;

  if (!domain || !clientId) {
    throw new Error(
      "Hosted UI is not configured. Set VITE_COGNITO_DOMAIN and VITE_COGNITO_CLIENT_ID.",
    );
  }

  return { domain, clientId, redirectUri };
}

export function buildHostedUILoginUrl(): string {
  const { domain, clientId, redirectUri } = getBaseHostedUiConfig();
  const normalizedDomain = domain.replace(/\/+$/, "");
  const params = new URLSearchParams({
    client_id: clientId,
    redirect_uri: redirectUri,
    response_type: "code",
    scope: "openid email profile",
  });

  return `${normalizedDomain}/login?${params.toString()}`;
}

export function buildGoogleHostedUILoginUrl(): string {
  const url = new URL(buildHostedUILoginUrl());
  url.searchParams.set("identity_provider", "Google");
  return url.toString();
}
