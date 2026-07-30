// FE landing route for the AWS Marketplace fulfillment redirect.
// AWS Marketplace → api_server /marketplace/register → 303 to this
// path with `?t=<token>`. We just full-page navigate to the BE's
// /api/v1/marketplace/complete endpoint (same origin via the express
// proxy, so the auth cookie IS sent) and let it 303 to /settings.
//
// If the user is unauthenticated we never get here — PublicRoutes
// intercepts /marketplace/complete separately and bounces to /signup
// preserving the token in return_to.

import { useEffect, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { Loader2 } from "lucide-react";

export default function MarketplaceCompletePage() {
  const [searchParams] = useSearchParams();
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const token = (searchParams.get("t") ?? "").trim();
    if (!token) {
      setError("Missing AWS Marketplace token. Start again from the Marketplace listing.");
      return;
    }
    // Full-page navigation so the browser follows the BE's 303 back
    // to /settings automatically. Proxy relays /api/v1/* to api_server;
    // the same-origin auth cookie is sent along with the request.
    window.location.assign(`/api/v1/marketplace/complete?t=${encodeURIComponent(token)}`);
  }, [searchParams]);

  return (
    <div className="flex min-h-screen items-center justify-center bg-background p-6 text-foreground">
      <div className="w-full max-w-md rounded-xl border border-border bg-card p-6 text-center">
        {error ? (
          <>
            <h1 className="text-lg font-semibold text-red-500">Something went wrong</h1>
            <p className="mt-2 text-sm text-muted-foreground">{error}</p>
          </>
        ) : (
          <>
            <Loader2 className="mx-auto h-6 w-6 animate-spin text-blue-500" aria-hidden />
            <h1 className="mt-3 text-lg font-semibold">Activating your AWS Marketplace subscription…</h1>
            <p className="mt-2 text-sm text-muted-foreground">
              This only takes a moment. You'll be redirected to Settings once your Standard tier is live.
            </p>
          </>
        )}
      </div>
    </div>
  );
}
