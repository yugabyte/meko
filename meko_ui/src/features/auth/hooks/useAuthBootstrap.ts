import { useEffect } from "react";
import { getCurrentUser, ONBOARDING_STATUS_CACHE_KEY } from "@/lib/mekoApi";

type Args = {
  setAuthLoading: (value: boolean) => void;
  setAuthenticated: (value: boolean) => void;
  setHasCompletedOnboarding: (value: boolean) => void;
};

export function readOnboardingStatusCache(): boolean | null {
  const raw = localStorage.getItem(ONBOARDING_STATUS_CACHE_KEY);
  if (raw == null) return null;
  if (raw === "true") return true;
  if (raw === "false") return false;
  localStorage.removeItem(ONBOARDING_STATUS_CACHE_KEY);
  return null;
}

export function useAuthBootstrap({
  setAuthLoading,
  setAuthenticated,
  setHasCompletedOnboarding,
}: Args) {
  useEffect(() => {
    let mounted = true;
    setAuthLoading(true);

    getCurrentUser()
      .then((user) => {
        if (!mounted) return;
        setAuthenticated(true);
        setHasCompletedOnboarding(user.has_completed_onboarding);
        localStorage.setItem(
          ONBOARDING_STATUS_CACHE_KEY,
          String(user.has_completed_onboarding),
        );
      })
      .catch(() => {
        if (!mounted) return;
        setAuthenticated(false);
        localStorage.removeItem(ONBOARDING_STATUS_CACHE_KEY);
      })
      .finally(() => {
        if (!mounted) return;
        setAuthLoading(false);
      });

    return () => {
      mounted = false;
    };
  }, [setAuthLoading, setAuthenticated, setHasCompletedOnboarding]);
}
