import { useEffect } from "react";
import { addAppEventListener, removeAppEventListener } from "@/lib/browserEvents";
import { AUTH_EXPIRED_EVENT, ONBOARDING_STATUS_CACHE_KEY } from "@/lib/api/mekoApi";
import { removeLocalItem } from "@/lib/browserStorage";
import { queryClient } from "@/lib/queryClient";
import type { NavigateFunction } from "react-router-dom";

type Args = {
  navigate: NavigateFunction;
  redirectTo: string;
  setAuthenticated: (value: boolean) => void;
  setAuthLoading: (value: boolean) => void;
};

export function useAuthExpiredRedirect({
  navigate,
  redirectTo,
  setAuthenticated,
  setAuthLoading,
}: Args) {
  useEffect(() => {
    const onAuthExpired = () => {
      // Session ended without an explicit logout — clear the in-memory query
      // cache so a different user logging in on this tab can't inherit it
      // (query keys aren't scoped per user).
      queryClient.clear();
      setAuthenticated(false);
      setAuthLoading(false);
      removeLocalItem(ONBOARDING_STATUS_CACHE_KEY);
      navigate(redirectTo, { replace: true });
    };

    addAppEventListener(AUTH_EXPIRED_EVENT, onAuthExpired);
    return () => {
      removeAppEventListener(AUTH_EXPIRED_EVENT, onAuthExpired);
    };
  }, [navigate, redirectTo, setAuthenticated, setAuthLoading]);
}
