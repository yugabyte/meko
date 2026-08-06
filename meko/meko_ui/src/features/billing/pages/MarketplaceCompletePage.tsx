// FE landing route for the AWS Marketplace fulfillment redirect.
//
// Two entry paths:
//   1. Fresh from AWS: `?t=<token>` present -> we full-page-navigate to
//      /api/v1/marketplace/complete which finalizes the entitlement,
//      flips users.user_type to standard, and 303s to /settings.
//   2. No token in URL: user landed here directly (typed URL, browser
//      back button, bookmark, retry). We don't have a token to
//      finalize, but the user may ALREADY be on Standard from an
//      earlier successful fulfillment. Check /auth/me:
//        - user_type=standard -> friendly "already active" state
//        - user_type=free    -> the error path (missing token; ask
//                                them to restart from AWS).
//
// This avoids the confusing "Missing AWS Marketplace token" red-error
// message when the flow ACTUALLY succeeded and the user just refreshed
// or hit back.
//
// If the user is unauthenticated we never get here — PublicRoutes
// intercepts /marketplace/complete separately and bounces to /signup
// preserving the token in return_to.

import { useEffect, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { CheckCircle2, Loader2 } from "lucide-react";
import { useCurrentUser } from "@/hooks/useCurrentUser";
import { ROUTES } from "@/app/routes/paths";
import { Button } from "@/components/ui/button";

type Status = "activating" | "error" | "already-active";

export default function MarketplaceCompletePage() {
  const [searchParams] = useSearchParams();
  const { data: user } = useCurrentUser({ enabled: true });
  const [status, setStatus] = useState<Status>("activating");
  const [errorMessage, setErrorMessage] = useState<string | null>(null);

  useEffect(() => {
    const token = (searchParams.get("t") ?? "").trim();

    // Fresh fulfillment from AWS. Full-page navigation to the BE
    // completion endpoint (same origin via the express proxy so the
    // auth cookie IS sent); BE 303s to /settings after upserting the
    // entitlement.
    if (token) {
      window.location.assign(`/api/v1/marketplace/complete?t=${encodeURIComponent(token)}`);
      return;
    }

    // No token in URL. We can't finalize anything -- but the user may
    // ALREADY be on Standard from an earlier successful fulfillment,
    // in which case the trip through this page is spurious. Wait for
    // /auth/me to load (it might still be inflight on first paint) and
    // branch on user_type.
    if (user === undefined) {
      // /auth/me still loading; keep the activating spinner up.
      return;
    }
    if (user?.user_type === "standard") {
      setStatus("already-active");
      return;
    }
    setStatus("error");
    setErrorMessage(
      "We couldn't find your Marketplace subscription token. Start again from the AWS Marketplace listing.",
    );
  }, [searchParams, user]);

  return (
    <div className="flex min-h-screen items-center justify-center bg-background p-6 text-foreground">
      <div className="w-full max-w-md rounded-xl border border-border bg-card p-6 text-center">
        {status === "activating" && (
          <>
            <Loader2 className="mx-auto h-6 w-6 animate-spin text-blue-500" aria-hidden />
            <h1 className="mt-3 text-lg font-semibold">
              Activating your AWS Marketplace subscription…
            </h1>
            <p className="mt-2 text-sm text-muted-foreground">
              This only takes a moment. You'll be redirected to Settings once your Standard tier is
              live.
            </p>
          </>
        )}

        {status === "already-active" && (
          <>
            <CheckCircle2 className="mx-auto h-8 w-8 text-emerald-500" aria-hidden />
            <h1 className="mt-3 text-lg font-semibold text-foreground">
              You're on the Standard plan
            </h1>
            <p className="mt-2 text-sm text-muted-foreground">
              Your AWS Marketplace subscription is already active. Head back to your workspace.
            </p>
            <div className="mt-5 flex justify-center gap-2">
              <Button asChild variant="default" size="sm">
                <Link to={ROUTES.datapacks}>Back to Meko</Link>
              </Button>
              <Button asChild variant="outline" size="sm">
                <Link to={ROUTES.settings}>View plan</Link>
              </Button>
            </div>
          </>
        )}

        {status === "error" && (
          <>
            <h1 className="text-lg font-semibold text-red-500">Something went wrong</h1>
            <p className="mt-2 text-sm text-muted-foreground">{errorMessage}</p>
            <div className="mt-5">
              <Button asChild variant="outline" size="sm">
                <Link to={ROUTES.settings}>Back to Settings</Link>
              </Button>
            </div>
          </>
        )}
      </div>
    </div>
  );
}
