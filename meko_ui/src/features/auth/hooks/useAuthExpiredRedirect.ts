import { useEffect } from "react";
import { addAppEventListener, removeAppEventListener } from "@/lib/browserEvents";
import { AUTH_EXPIRED_EVENT, ONBOARDING_STATUS_CACHE_KEY } from "@/lib/mekoApi";
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
      setAuthenticated(false);
      setAuthLoading(false);
      localStorage.removeItem(ONBOARDING_STATUS_CACHE_KEY);
      navigate(redirectTo, { replace: true });
    };

    addAppEventListener(AUTH_EXPIRED_EVENT, onAuthExpired);
    return () => {
      removeAppEventListener(AUTH_EXPIRED_EVENT, onAuthExpired);
    };
  }, [navigate, redirectTo, setAuthenticated, setAuthLoading]);
}
