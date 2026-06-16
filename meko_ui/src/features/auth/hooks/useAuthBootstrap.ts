import { useEffect } from "react";
import axios from "axios";
import { getCurrentUser, ONBOARDING_STATUS_CACHE_KEY } from "@/lib/api/mekoApi";
import { getLocalItem, removeLocalItem, setLocalItem } from "@/lib/browserStorage";

type Args = {
  setAuthLoading: (value: boolean) => void;
  setAuthenticated: (value: boolean) => void;
  setHasCompletedOnboarding: (value: boolean) => void;
};

export function readOnboardingStatusCache(): boolean | null {
  const raw = getLocalItem(ONBOARDING_STATUS_CACHE_KEY);
  if (raw == null) return null;
  if (raw === "true") return true;
  if (raw === "false") return false;
  removeLocalItem(ONBOARDING_STATUS_CACHE_KEY);
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
        setLocalItem(ONBOARDING_STATUS_CACHE_KEY, String(user.has_completed_onboarding));
      })
      .catch((err) => {
        // Ignore aborts triggered by the cleanup below; the component is
        // unmounting (or the effect is re-running) so state updates would
        // be stale.
        if (controller.signal.aborted || axios.isCancel(err)) return;
        setAuthenticated(false);
        removeLocalItem(ONBOARDING_STATUS_CACHE_KEY);
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
