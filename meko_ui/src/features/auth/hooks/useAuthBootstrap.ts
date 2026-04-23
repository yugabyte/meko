import { useEffect } from "react";
import axios from "axios";
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
    const controller = new AbortController();
    setAuthLoading(true);

    getCurrentUser({ signal: controller.signal })
      .then((user) => {
        if (controller.signal.aborted) return;
        setAuthenticated(true);
        setHasCompletedOnboarding(user.has_completed_onboarding);
        localStorage.setItem(
          ONBOARDING_STATUS_CACHE_KEY,
          String(user.has_completed_onboarding),
        );
      })
      .catch((err) => {
        // Ignore aborts triggered by the cleanup below; the component is
        // unmounting (or the effect is re-running) so state updates would
        // be stale.
        if (controller.signal.aborted || axios.isCancel(err)) return;
        setAuthenticated(false);
        localStorage.removeItem(ONBOARDING_STATUS_CACHE_KEY);
      })
      .finally(() => {
        if (controller.signal.aborted) return;
        setAuthLoading(false);
      });

    return () => {
      controller.abort();
    };
  }, [setAuthLoading, setAuthenticated, setHasCompletedOnboarding]);
}
