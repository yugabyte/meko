// Public-side landing for /marketplace/complete when the visitor is
// NOT yet signed in. Preserves the ?t=<token> by forwarding it into a
// `return_to` query on /signup, so after signup + email confirm + login
// the user lands back on /marketplace/complete?t=... and the authed
// path can finish the entitlement lookup.

import { useEffect } from "react";
import { Navigate, useSearchParams } from "react-router-dom";
import { ROUTES } from "@/app/routes/paths";

export default function MarketplaceSignInBouncePage() {
  const [searchParams] = useSearchParams();
  const token = (searchParams.get("t") ?? "").trim();
  const returnTo = token
    ? `${ROUTES.marketplaceComplete}?t=${encodeURIComponent(token)}`
    : ROUTES.marketplaceComplete;
  const signupUrl = `${ROUTES.signup}?source=aws-marketplace&return_to=${encodeURIComponent(returnTo)}`;

  // Track the mount for potential telemetry; today it's just a redirect.
  useEffect(() => {
    /* no-op; the render returns a Navigate */
  }, []);

  return <Navigate to={signupUrl} replace />;
}
