import { useEffect } from "react";
import { resolveGuardRedirect } from "./guards";
import { pendingSharePath } from "@/lib/pendingShareLink";
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
    // A share link opened before an OAuth/OIDC sign-in comes back without its
    // return_to, so it resumes from sessionStorage instead.
    const returnTo = new URLSearchParams(search).get("return_to") ?? pendingSharePath();
    const redirect = resolveGuardRedirect({
      location,
      search,
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
