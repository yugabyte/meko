import { useEffect } from "react";
import axios from "axios";
import { getCurrentUser, ONBOARDING_STATUS_CACHE_KEY } from "@/lib/api/mekoApi";
import { getLocalItem, removeLocalItem, setLocalItem } from "@/lib/browserStorage";
import { clearPendingLoginMethod, consumePendingLoginMethod } from "@/features/auth/pendingLoginMethod";
import { TELEMETRY_EVENTS } from "@/lib/telemetry/events";
import { identifyUser, trackEvent } from "@/lib/telemetry/rudderstack";

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
        // Redirect-based sign-in (e.g. Google) completes here, not in SignInPage.
        // If a method was stashed before the redirect, the session is now verified
        // → record the real sign-in once, with the now-known email + user_id.
        const pendingMethod = consumePendingLoginMethod();
        if (pendingMethod) {
          identifyUser({
            id: user.account_id,
            email: user.email,
            firstName: user.first_name,
            lastName: user.last_name,
            userType: user.user_type,
          });
          trackEvent(TELEMETRY_EVENTS.USER_SIGNED_IN, {
            email: user.email,
            user_id: user.account_id,
            login_method: pendingMethod,
          });
        }
      })
      .catch((err) => {
        // Ignore aborts triggered by the cleanup below; the component is
        // unmounting (or the effect is re-running) so state updates would
        // be stale.
        if (controller.signal.aborted || axios.isCancel(err)) return;
        setAuthenticated(false);
        removeLocalItem(ONBOARDING_STATUS_CACHE_KEY);
        // Bootstrap failed (e.g. cancelled/failed OAuth, expired session). Clear any
        // stale login marker so it can't be mis-consumed by a later sign-in of a
        // different kind (which would report the wrong login_method).
        clearPendingLoginMethod();
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
