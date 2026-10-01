import { AUTH_ROUTES, PROTECTED_ROUTE_PREFIXES, ROUTES } from "./paths";
import { safeReturnTo } from "@/features/auth/returnTo";

type GuardInput = {
  location: string;
  /** Current search string (including leading "?"). Used to carry a
   *  deep-link's pathname+search into the return_to= query when the
   *  guard sends an unauthenticated user to sign-in. */
  search?: string;
  authLoading: boolean;
  authenticated: boolean;
  hasCompletedOnboarding: boolean;
  /** Raw ?return_to= from the URL, if present. When set to a safe
   *  same-origin path, the guard honors it instead of bouncing an
   *  already-authed user to their default landing page — critical for
   *  the AWS Marketplace flow where /signup carries the buyer back to
   *  /marketplace/complete after they authenticate. */
  returnTo?: string | null;
};

export function isAuthRoute(location: string): boolean {
  return AUTH_ROUTES.includes(location as (typeof AUTH_ROUTES)[number]);
}

export function isProtectedRoute(location: string): boolean {
  return PROTECTED_ROUTE_PREFIXES.some(
    (prefix) => location === prefix || location.startsWith(`${prefix}/`),
  );
}

export function isOnboardingRoute(location: string): boolean {
  return location === ROUTES.onboarding || location.startsWith(`${ROUTES.onboarding}/`);
}

export function resolveGuardRedirect(input: GuardInput): string | null {
  const {
    location,
    authLoading,
    authenticated,
    hasCompletedOnboarding,
  } = input;

  if (authLoading) return null;

  const onAuthRoute = isAuthRoute(location);
  const onProtectedRoute = isProtectedRoute(location);

  // Onboarding isn't enforced: navigating away (e.g. a sidebar link) skips
  // it. Fresh users still land on /onboarding after login (see App.tsx).

  if (authenticated && onAuthRoute) {
    // Marketplace / invite links land on /signup with a return_to
    // pointing back into the app. Honor that instead of the default
    // landing so the buyer flow can resume.
    const rt = safeReturnTo(input.returnTo);
    if (rt) return rt;
    return hasCompletedOnboarding ? ROUTES.datapacks : ROUTES.onboarding;
  }


  if (authenticated && hasCompletedOnboarding && location === ROUTES.connect) {
    return ROUTES.connect;
  }

  if (!authenticated && onProtectedRoute) {
    // Carry the deep-link into /signin directly (NOT /login — that route
    // is a <Navigate> which drops the query). SignInPage reads
    // ?return_to= via safeReturnTo. Mirrors the branch in App.tsx; both
    // navigations race because the guard's effect and the parent's
    // <Navigate> both fire on this state — whichever lands last, the
    // query is preserved either way.
    const returnTo = `${location}${input.search ?? ""}`;
    const qs = new URLSearchParams({ return_to: returnTo }).toString();
    return `${ROUTES.signin}?${qs}`;
  }

  return null;
}
