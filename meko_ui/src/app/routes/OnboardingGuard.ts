import { useEffect } from "react";
import { resolveGuardRedirect } from "./guards";
import type { NavigateFunction } from "react-router-dom";

type Args = {
  location: string;
  authLoading: boolean;
  authenticated: boolean;
  hasCompletedOnboarding: boolean;
  navigate: NavigateFunction;
};

export function useOnboardingGuard({
  location,
  authLoading,
  authenticated,
  hasCompletedOnboarding,
  navigate,
}: Args) {
  useEffect(() => {
    const redirect = resolveGuardRedirect({
      location,
      authLoading,
      authenticated,
      hasCompletedOnboarding,
    });
    if (redirect != null) {
      navigate(redirect, { replace: true });
    }
  }, [location, authLoading, authenticated, hasCompletedOnboarding, navigate]);
}
