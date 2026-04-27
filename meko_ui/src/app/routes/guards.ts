import { AUTH_ROUTES, PROTECTED_ROUTE_PREFIXES, ROUTES } from "./paths";

type GuardInput = {
  location: string;
  authLoading: boolean;
  authenticated: boolean;
  hasCompletedOnboarding: boolean;
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
  const onOnboardingRoute = isOnboardingRoute(location);

  if (authenticated && onAuthRoute) {
    return ROUTES.onboarding;
  }

  if (authenticated && !hasCompletedOnboarding && !onOnboardingRoute) {
    return ROUTES.onboarding;
  }

  if (authenticated && hasCompletedOnboarding && location === ROUTES.home) {
    return ROUTES.home;
  }

  if (!authenticated && onProtectedRoute) {
    return ROUTES.login;
  }

  return null;
}
