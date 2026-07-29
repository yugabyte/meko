import { useEffect } from "react";
import { resolveGuardRedirect } from "./guards";
import type { NavigateFunction } from "react-router-dom";

type Args = {
  location: string;
  search: string;
  authLoading: boolean;
  authenticated: boolean;
  hasCompletedOnboarding: boolean;
  navigate: NavigateFunction;
};

export function useOnboardingGuard({
  location,
  search,
  authLoading,
  authenticated,
  hasCompletedOnboarding,
  navigate,
}: Args) {
  useEffect(() => {
    const returnTo = new URLSearchParams(search).get("return_to");
    const redirect = resolveGuardRedirect({
      location,
      authLoading,
      authenticated,
      hasCompletedOnboarding,
      returnTo,
    });
    if (redirect != null) {
      navigate(redirect, { replace: true });
    }
  }, [location, search, authLoading, authenticated, hasCompletedOnboarding, navigate]);
}
