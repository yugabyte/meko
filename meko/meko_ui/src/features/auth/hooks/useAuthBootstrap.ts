import { useEffect } from "react";
import axios from "axios";
import { getCurrentUser, ONBOARDING_STATUS_CACHE_KEY } from "@/lib/api/mekoApi";
import { getLocalItem, removeLocalItem, setLocalItem } from "@/lib/browserStorage";
import {
  clearPendingLoginMethod,
  consumePendingLoginMethod,
} from "@/features/auth/pendingLoginMethod";
import { TELEMETRY_EVENTS } from "@/lib/telemetry/events";
import { identifyThenTrack } from "@/lib/telemetry/rudderstack";

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

/** Read-and-strip ?welcome=1 from the current URL. Returns true if present.
 *  Called once per bootstrap — stripping on consume means a refresh after
 *  sign-up doesn't re-fire the signup event. */
export function consumeWelcomeParam(): boolean {
  if (typeof window === "undefined") return false;
  const url = new URL(window.location.href);
  if (url.searchParams.get("welcome") !== "1") return false;
  url.searchParams.delete("welcome");
  const next = url.pathname + (url.search ? url.search : "") + url.hash;
  window.history.replaceState(window.history.state, "", next);
  return true;
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
        // ?welcome=1 is set by the OAuth callback only when the account was
        // JIT-provisioned in this round-trip (see api_server appHomeRedirect).
        // It's the only reliable "new vs existing" signal — /auth/me has no
        // created_at, and has_completed_onboarding is false for anyone who
        // skipped onboarding. Strip on every boot so the URL is clean even on
        // paths that don't fire telemetry; a refresh must not re-fire the
        // signup event.
        const welcome = consumeWelcomeParam();
        // Redirect-based sign-in (Google / LinkedIn / OIDC) completes here, not
        // in SignInPage / SignUpPage. If a method was stashed before the
        // redirect, the session is now verified → record the event with the
        // now-known email + user_id.
        const pendingMethod = consumePendingLoginMethod();
        if (pendingMethod) {
          const traits = {
            id: user.account_id,
            email: user.email,
            firstName: user.first_name,
            lastName: user.last_name,
          };
          // On a fresh JIT provision (welcome) fire BOTH user_signed_up and
          // user_signed_in: email users get both events over their lifecycle
          // (sign_up at the form submit, sign_in on first manual sign-in), so
          // firing only one here would make social users miss DAU and funnel
          // steps keyed on either event. identifyThenTrack identifies now and
          // tracks after a short gap; a second identifyThenTrack call reuses
          // the same identify() and queues its own track(), so both land.
          if (welcome) {
            identifyThenTrack(traits, TELEMETRY_EVENTS.USER_SIGNED_UP, {
              email: user.email,
              signup_method: pendingMethod,
            });
          }
          identifyThenTrack(traits, TELEMETRY_EVENTS.USER_SIGNED_IN, {
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
